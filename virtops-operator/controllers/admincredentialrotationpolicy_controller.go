package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	record "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	guestopsv1alpha1 "virtops-operator/api/v1alpha1"
	"virtops-operator/pkg/network"
	"virtops-operator/pkg/publish"
	"virtops-operator/pkg/scheduler"
)

// AdminCredentialRotationPolicyReconciler reconciles AdminCredentialRotationPolicy objects.
// The reconciler is the "brain" of the operator: it reacts to changes and plans/coordinates actions.
type AdminCredentialRotationPolicyReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	Recorder           record.EventRecorder
	Log                logr.Logger
	SSHExecutorImage   string
	WinRMExecutorImage string
	KubeClient         kubernetes.Interface
}

//+kubebuilder:rbac:groups=guestops.io,resources=admincredentialrotationpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=guestops.io,resources=admincredentialrotationpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events;pods;secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is called when the desired/actual state of the watched resources changes.
// MVP: it validates the resource existence and coordinates a rotation run when due.
func (r *AdminCredentialRotationPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var policy guestopsv1alpha1.AdminCredentialRotationPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if errors.IsNotFound(err) {
			// Resource deleted: nothing to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciling AdminCredentialRotationPolicy",
		"name", policy.Name,
		"namespace", policy.Namespace,
		"os", policy.Spec.OS,
	)

	// Determine whether a manual run was requested via annotation
	rotateNow := false
	if policy.Spec.Trigger != nil && policy.Spec.Trigger.EnableAnnotation {
		if val, ok := policy.Annotations["guestops.io/rotate-now"]; ok && val != "" {
			rotateNow = true
		}
	}

	// Determine whether the cron schedule is due
	cronDue := false
	nextRun := time.Time{}
	if policy.Spec.Schedule != "" {
		var last *time.Time
		if policy.Status.LastRunTime != nil {
			lr := policy.Status.LastRunTime.Time
			last = &lr
		}
		due, next, err := scheduler.IsDue(policy.Spec.Schedule, last, time.Now())
		if err != nil {
			logger.Error(err, "error parsing cron schedule", "spec", policy.Spec.Schedule)
		} else {
			cronDue = due
			nextRun = next
		}
	}

	// Collect completions/failures from Jobs and update status (no secrets included)
	updated, collectErr := r.collectJobCompletions(ctx, &policy)
	if updated {
		if err := r.Status().Update(ctx, &policy); err != nil {
			logger.Error(err, "error updating status after job completion")
		}
	}
	if collectErr != nil {
		logger.Error(collectErr, "error collecting job completions")
		return ctrl.Result{}, collectErr
	}

	if !rotateNow && !cronDue {
		if inFlight, err := r.hasInFlightJobs(ctx, &policy); err != nil {
			logger.Error(err, "error checking in-flight jobs")
		} else if inFlight {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// Update NextRunTime if computed
		if !nextRun.IsZero() {
			policy.Status.NextRunTime = &metav1.Time{Time: nextRun}
			_ = r.Status().Update(ctx, &policy)
		}
		return ctrl.Result{}, nil
	}

	logger.Info("starting rotation run", "policy", policy.Name, "namespace", policy.Namespace, "rotateNow", rotateNow, "cronDue", cronDue)

	// List target VMIs based on the policy label selector
	var selector labels.Selector
	if policy.Spec.Targets.Selector != nil {
		var err error
		selector, err = metav1.LabelSelectorAsSelector(policy.Spec.Targets.Selector)
		if err != nil {
			logger.Error(err, "invalid label selector")
			return ctrl.Result{}, nil
		}
	}
	vmiList := &unstructured.UnstructuredList{}
	vmiList.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstanceList"})
	listOpts := []client.ListOption{client.InNamespace(policy.Namespace)}
	if selector != nil {
		listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: selector})
	}
	if err := r.List(ctx, vmiList, listOpts...); err != nil {
		logger.Error(err, "unable to list target VMIs")
		return ctrl.Result{}, err
	}

	// Build network selection input
	selectIn := network.SelectionInput{Mode: network.ModeAuto, PreferPod: true}
	if ns := policy.Spec.Targets.NetworkSelection; ns != nil {
		selectIn.Mode = network.Mode(ns.Mode)
		selectIn.PreferPod = ns.PreferPod
		selectIn.NadList = append(selectIn.NadList, ns.NadList...)
	} else if len(policy.Spec.Targets.NetworkAttachments) > 0 {
		selectIn.Mode = network.ModeNadList
		selectIn.NadList = append(selectIn.NadList, policy.Spec.Targets.NetworkAttachments...)
	}

	// Determine how many VMIs to process in this run based on concurrency settings
	maxJobs := len(vmiList.Items)
	if policy.Spec.Concurrency != nil && policy.Spec.Concurrency.MaxConcurrent != nil {
		if int(*policy.Spec.Concurrency.MaxConcurrent) < maxJobs {
			maxJobs = int(*policy.Spec.Concurrency.MaxConcurrent)
		}
	}

	publishMode, publishSecretName := publishConfig(&policy)
	var publishSecret *corev1.Secret
	if publishMode == publish.ModeAlways {
		var s corev1.Secret
		if err := r.Get(ctx, client.ObjectKey{Namespace: policy.Namespace, Name: publishSecretName}, &s); err == nil {
			publishSecret = &s
		}
	}

	jobsCreated := 0
	for _, item := range vmiList.Items {
		if jobsCreated >= maxJobs {
			break
		}
		vmi := item.UnstructuredContent()
		res := network.SelectFromVMI(vmi, selectIn)
		jobName := buildJobName(policy.Name, item.GetName())

		// Determine executor image and destination port with sensible defaults
		img := r.SSHExecutorImage
		effPort := policy.Spec.Method.Port
		if policy.Spec.Method.Type == "winrm" {
			img = r.WinRMExecutorImage
			if img == "" {
				img = "alpine:3.19"
			}
			if effPort == 0 {
				if policy.Spec.Method.TLS {
					effPort = 5986
				} else {
					effPort = 5985
				}
			}
		} else { // ssh by default
			if effPort == 0 {
				effPort = 22
			}
		}
		if img == "" {
			img = "alpine:3.19"
		}
		// Use real executor if a non-placeholder image is configured
		useRealExecutor := (img != "" && img != "alpine:3.19")

		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: policy.Namespace,
				Labels: map[string]string{
					"guestops.io/policy": policy.Name,
					"guestops.io/vm":     item.GetName(),
				},
			},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: map[string]string{},
					},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{
							{
								Name:            "executor",
								Image:           img,
								ImagePullPolicy: corev1.PullAlways,
								Command:         []string{"/executor"},
								Env: []corev1.EnvVar{
									{Name: "TARGET_VM", Value: item.GetName()},
									{Name: "TARGET_IP", Value: res.IP},
									{Name: "METHOD", Value: policy.Spec.Method.Type},
									{Name: "TARGET_USER", Value: policy.Spec.Method.User},
									{Name: "TARGET_PORT", Value: fmt.Sprintf("%d", effPort)},
									{Name: "ROTATION_KIND", Value: policy.Spec.Rotation.Kind},
									{Name: "ROTATION_SOURCE", Value: policy.Spec.Rotation.Source},
									{Name: "BOOTSTRAP_SECRET_REF", Value: policy.Spec.Method.Auth.BootstrapSecretRef},
								},
								SecurityContext: &corev1.SecurityContext{
									AllowPrivilegeEscalation: boolPtr(false),
									ReadOnlyRootFilesystem:   boolPtr(true),
									RunAsNonRoot:             boolPtr(true),
								},
							},
						},
					},
				},
			},
		}
		// If we are still on the placeholder image, keep the placeholder command.
		if !useRealExecutor {
			c := &job.Spec.Template.Spec.Containers[0]
			c.Command = []string{"/bin/sh", "-c"}
			c.Args = []string{fmt.Sprintf(
				"echo rotating for VM=%s IP=%s method=%s kind=%s user=%s port=%d policy=%s && sleep 2",
				item.GetName(), res.IP, policy.Spec.Method.Type, policy.Spec.Rotation.Kind, policy.Spec.Method.User, effPort, policy.Name,
			)}
		}
		// Auto-cleanup finished Jobs after 600 seconds
		ttl := int32(600)
		job.Spec.TTLSecondsAfterFinished = &ttl
		// If a real executor image is configured, switch to /executor and mount Secrets
		if useRealExecutor {
			zero := int32(0)
			deadline := int64(120)
			job.Spec.BackoffLimit = &zero
			job.Spec.ActiveDeadlineSeconds = &deadline
			c := &job.Spec.Template.Spec.Containers[0]
			c.ImagePullPolicy = corev1.PullAlways
			c.Command = []string{"/executor"}
			c.Args = nil
			c.Env = []corev1.EnvVar{
				{Name: "TARGET_IP", Value: res.IP},
				{Name: "TARGET_USER", Value: policy.Spec.Method.User},
				{Name: "TARGET_PORT", Value: fmt.Sprintf("%d", effPort)},
				{Name: "ROTATION_KIND", Value: policy.Spec.Rotation.Kind},
				{Name: "ROTATION_SOURCE", Value: policy.Spec.Rotation.Source},
				{Name: "CONNECT_TIMEOUT_SECONDS", Value: "30"},
				{Name: "EXEC_TIMEOUT_SECONDS", Value: "60"},
			}
			if policy.Spec.Method.Type == "ssh" {
				mode := strings.ToLower(strings.TrimSpace(policy.Spec.Rotation.AuthorizedKeysMode))
				replace := true
				if mode == "append" {
					replace = false
				}
				c.Env = append(c.Env, corev1.EnvVar{Name: "MODE_REPLACE", Value: fmt.Sprintf("%t", replace)})
				if policy.Spec.Rotation.Kind == "linux-password" {
					if policy.Spec.Rotation.PasswordPolicy != nil {
						pp := policy.Spec.Rotation.PasswordPolicy
						if pp.Length != nil {
							c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_LENGTH", Value: fmt.Sprintf("%d", *pp.Length)})
						} else {
							if pp.MinLength != nil {
								c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_LENGTH", Value: fmt.Sprintf("%d", *pp.MinLength)})
							}
							if pp.MaxLength != nil {
								c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MAX_LENGTH", Value: fmt.Sprintf("%d", *pp.MaxLength)})
							}
							if pp.MinLength == nil && pp.MaxLength == nil {
								if policy.Spec.Rotation.Length != nil {
									c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_LENGTH", Value: fmt.Sprintf("%d", *policy.Spec.Rotation.Length)})
								}
							}
						}
						if pp.MinUpper != nil {
							c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_UPPER", Value: fmt.Sprintf("%d", *pp.MinUpper)})
						}
						if pp.MinLower != nil {
							c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_LOWER", Value: fmt.Sprintf("%d", *pp.MinLower)})
						}
						if pp.MinDigits != nil {
							c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_DIGITS", Value: fmt.Sprintf("%d", *pp.MinDigits)})
						}
						if pp.MinSpecial != nil {
							c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_SPECIAL", Value: fmt.Sprintf("%d", *pp.MinSpecial)})
						}
					} else if policy.Spec.Rotation.Length != nil {
						c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_LENGTH", Value: fmt.Sprintf("%d", *policy.Spec.Rotation.Length)})
					}
				}
				// Writable workdir for key generation in read-only rootfs
				job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes,
					corev1.Volume{
						Name:         "work",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}},
					},
				)
				c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "work", MountPath: "/work"})
			}
			if policy.Spec.Method.Type == "winrm" {
				c.Env = append(c.Env, corev1.EnvVar{Name: "WINRM_TLS", Value: fmt.Sprintf("%t", policy.Spec.Method.TLS)})
				if policy.Spec.Rotation.PasswordPolicy != nil {
					pp := policy.Spec.Rotation.PasswordPolicy
					if pp.Length != nil {
						c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_LENGTH", Value: fmt.Sprintf("%d", *pp.Length)})
					} else {
						if pp.MinLength != nil {
							c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_LENGTH", Value: fmt.Sprintf("%d", *pp.MinLength)})
						}
						if pp.MaxLength != nil {
							c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MAX_LENGTH", Value: fmt.Sprintf("%d", *pp.MaxLength)})
						}
						if pp.MinLength == nil && pp.MaxLength == nil {
							if policy.Spec.Rotation.Length != nil {
								c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_LENGTH", Value: fmt.Sprintf("%d", *policy.Spec.Rotation.Length)})
							}
						}
					}
					if pp.MinUpper != nil {
						c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_UPPER", Value: fmt.Sprintf("%d", *pp.MinUpper)})
					}
					if pp.MinLower != nil {
						c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_LOWER", Value: fmt.Sprintf("%d", *pp.MinLower)})
					}
					if pp.MinDigits != nil {
						c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_DIGITS", Value: fmt.Sprintf("%d", *pp.MinDigits)})
					}
					if pp.MinSpecial != nil {
						c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_POLICY_MIN_SPECIAL", Value: fmt.Sprintf("%d", *pp.MinSpecial)})
					}
				} else if policy.Spec.Rotation.Length != nil {
					c.Env = append(c.Env, corev1.EnvVar{Name: "PASSWORD_LENGTH", Value: fmt.Sprintf("%d", *policy.Spec.Rotation.Length)})
				}
				// Provide a writable temp dir for Python/libs when rootfs is read-only
				job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes,
					corev1.Volume{
						Name:         "tmp",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					},
				)
				c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"})
			}
			bootstrapSecretRef := policy.Spec.Method.Auth.BootstrapSecretRef
			bootstrapItems, usePublishedBootstrap := publishedBootstrapItems(publishSecret, item.GetName(), policy.Spec.Method.Type, policy.Spec.Rotation.Kind)
			if usePublishedBootstrap {
				bootstrapSecretRef = publishSecretName
				logger.Info("using published bootstrap credentials", "policy", policy.Name, "namespace", policy.Namespace, "vm", item.GetName(), "secret", publishSecretName)
			}
			if bootstrapSecretRef != "" {
				volSecret := &corev1.SecretVolumeSource{SecretName: bootstrapSecretRef}
				if len(bootstrapItems) > 0 {
					volSecret.Items = bootstrapItems
				}
				job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes,
					corev1.Volume{
						Name: "bootstrap",
						VolumeSource: corev1.VolumeSource{
							Secret: volSecret,
						},
					},
				)
				c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
					Name: "bootstrap", MountPath: "/bootstrap", ReadOnly: true,
				})
				c.Env = append(c.Env,
					corev1.EnvVar{Name: "BOOTSTRAP_USERNAME_FILE", Value: "/bootstrap/username"},
					corev1.EnvVar{Name: "BOOTSTRAP_PASSWORD_FILE", Value: "/bootstrap/password"},
				)
				if policy.Spec.Method.Type == "ssh" {
					c.Env = append(c.Env, corev1.EnvVar{Name: "BOOTSTRAP_PRIVATEKEY_FILE", Value: "/bootstrap/privateKey"})
				}
			}
			if res.NetworksAnnotation != "" {
				if job.Spec.Template.ObjectMeta.Annotations == nil {
					job.Spec.Template.ObjectMeta.Annotations = map[string]string{}
				}
				job.Spec.Template.ObjectMeta.Annotations["k8s.v1.cni.cncf.io/networks"] = res.NetworksAnnotation
			}
		}

		if err := ctrl.SetControllerReference(&policy, job, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, job); err != nil {
			if !errors.IsAlreadyExists(err) {
				return ctrl.Result{}, err
			}
		}
		jobsCreated++

		rotAt := metav1.Now()
		policy.Status.Results = append(policy.Status.Results, guestopsv1alpha1.RotationResult{
			VMName:    item.GetName(),
			Phase:     "Scheduled",
			Reason:    "JobCreated",
			Message:   fmt.Sprintf("created job %s targeting IP %s", jobName, res.IP),
			RotatedAt: &rotAt,
		})
	}

	if jobsCreated > 0 {
		logger.Info("scheduled rotation jobs", "policy", policy.Name, "namespace", policy.Namespace, "jobs", jobsCreated, "publishMode", publishMode, "publishSecret", publishSecretName)
	}

	// Update status
	now := metav1.Now()
	policy.Status.LastRunTime = &now
	if policy.Spec.Schedule != "" {
		if nr, err := scheduler.NextFromCron(policy.Spec.Schedule, time.Now()); err == nil {
			policy.Status.NextRunTime = &metav1.Time{Time: nr}
		}
	}
	if err := r.Status().Update(ctx, &policy); err != nil {
		logger.Error(err, "error updating status")
	}

	// Remove the annotation to avoid repeated manual retriggers
	if rotateNow {
		patch := client.MergeFrom(policy.DeepCopy())
		if policy.Annotations != nil {
			delete(policy.Annotations, "guestops.io/rotate-now")
		}
		if err := r.Patch(ctx, &policy, patch); err != nil {
			logger.Error(err, "unable to remove rotate-now annotation")
		}
	}

	if inFlight, err := r.hasInFlightJobs(ctx, &policy); err != nil {
		logger.Error(err, "error checking in-flight jobs")
	} else if inFlight {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	// Requeue close to the next scheduled run (if present)
	if policy.Status.NextRunTime != nil {
		d := time.Until(policy.Status.NextRunTime.Time)
		if d > 0 {
			return ctrl.Result{RequeueAfter: d}, nil
		}
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller with the manager.
func (r *AdminCredentialRotationPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&guestopsv1alpha1.AdminCredentialRotationPolicy{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}

func buildJobName(policyName, vmName string) string {
	p := sanitizeDNSLabel(policyName)
	v := sanitizeDNSLabel(vmName)
	h := hash8(p + "-" + v)

	// Allocate budget for name <=63: "acrp-" + p + "-" + v + "-" + h
	// Overhead = 7 + len(h) ("acrp-"=5, two hyphens=2)
	overhead := 7 + len(h)
	budget := 63 - overhead
	if budget < 0 {
		budget = 0
	}

	// Split budget between p and v
	half := budget / 2
	p = truncate(p, half)
	v = truncate(v, budget-half)
	name := fmt.Sprintf("acrp-%s-%s-%s", p, v, h)
	if len(name) > 63 {
		// Fallback hard cap
		p = truncate(p, 24)
		v = truncate(v, 24)
		name = fmt.Sprintf("acrp-%s-%s-%s", p, v, h)
		if len(name) > 63 {
			name = name[:63]
		}
	}
	return name
}

func sanitizeDNSLabel(s string) string {
	s = strings.ToLower(s)
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b = append(b, r)
		} else {
			b = append(b, '-')
		}
	}
	res := strings.Trim(string(b), "-")
	if res == "" {
		res = "name"
	}
	return res
}

func truncate(s string, max int) string {
	if max < 0 {
		return s
	}
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func hash8(s string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// collectJobCompletions updates policy.Status.Results based on the current Job states.
func (r *AdminCredentialRotationPolicyReconciler) collectJobCompletions(ctx context.Context, policy *guestopsv1alpha1.AdminCredentialRotationPolicy) (bool, error) {
	logger := log.FromContext(ctx)
	jobList, err := r.listJobsForPolicy(ctx, policy)
	if err != nil {
		return false, err
	}
	updated := false
	var finalErr error
	now := metav1.Now()

	for i := range jobList.Items {
		job := &jobList.Items[i]
		vmName := job.Labels["guestops.io/vm"]
		if vmName == "" {
			continue
		}

		completionTime := jobCompletionTime(job, &now)

		phase := ""
		reason := ""
		if isJobComplete(job) {
			phase = "Completed"
			reason = "TargetAuthUpdated"
		} else if isJobFailed(job) {
			phase = "Failed"
			reason = jobFailureReason(job)
			if reason == "" {
				reason = "ExecutorError"
			} else if reason == "DeadlineExceeded" {
				reason = "Timeout"
			} else if reason == "BackoffLimitExceeded" {
				reason = "BackoffExceeded"
			}
		} else {
			// Not in a final state yet
			continue
		}

		if lastScheduledIndex(policy.Status.Results, vmName) < 0 && hasFinalResultAtTime(policy.Status.Results, vmName, completionTime) {
			continue
		}

		// Short message, no secrets
		c := job.Spec.Template.Spec.Containers[0]
		targetIP := envVal(&c, "TARGET_IP")
		mode := envVal(&c, "MODE_REPLACE")
		modeStr := "replace"
		if strings.ToLower(mode) == "false" || mode == "0" {
			modeStr = "append"
		}
		method := policy.Spec.Method.Type
		kind := policy.Spec.Rotation.Kind
		source := policy.Spec.Rotation.Source

		message := ""
		if phase == "Completed" {
			if len(c.Command) > 0 && c.Command[0] == "/executor" {
				if kind == "ssh-key" {
					message = fmt.Sprintf("updated authorized_keys on %s (method=%s, kind=%s, source=%s, mode=%s)", targetIP, method, kind, source, modeStr)
				} else if kind == "windows-password" || kind == "linux-password" {
					message = fmt.Sprintf("updated password on %s (method=%s, kind=%s, source=%s)", targetIP, method, kind, source)
				} else {
					message = fmt.Sprintf("rotation completed on %s (method=%s, kind=%s, source=%s)", targetIP, method, kind, source)
				}
			} else {
				message = fmt.Sprintf("placeholder execution (no-op) for VM=%s (method=%s)", vmName, method)
			}
		} else {
			message = fmt.Sprintf("job failed for VM=%s (method=%s)", vmName, method)
		}

		// Enrich message with executor JSON logs (last JSON line) when running real executor.
		if len(c.Command) > 0 && c.Command[0] == "/executor" {
			execRes, err := r.executorResultFromJobLogs(ctx, job)
			if err == nil {
				if execRes.Message != "" {
					message = execRes.Message
				}
				if phase == "Failed" && strings.ToLower(execRes.Status) == "error" {
					if reason == "" || reason == "TargetAuthUpdated" {
						reason = "ExecutorError"
					}
				}
				if phase == "Completed" {
					pmode, psecretName := publishConfig(policy)
					if pmode == publish.ModeAlways {
						kind = envVal(&c, "ROTATION_KIND")
						if kind == "" {
							kind = policy.Spec.Rotation.Kind
						}
						user := envVal(&c, "TARGET_USER")
						if user == "" {
							user = policy.Spec.Method.User
						}
						if err := r.publishExecutorResult(ctx, policy, pmode, psecretName, vmName, user, kind, execRes); err != nil {
							phase = "Failed"
							reason = "PublishFailed"
							message = truncate(fmt.Sprintf("publish failed: %v", err), 300)
							if finalErr == nil {
								finalErr = fmt.Errorf("publish failed for vm %s: %w", vmName, err)
							}
						}
					}
				}
			} else if phase == "Completed" {
				pmode, _ := publishConfig(policy)
				if pmode == publish.ModeAlways {
					phase = "Failed"
					reason = "PublishFailed"
					message = truncate(fmt.Sprintf("publish failed: cannot read executor logs: %v", err), 300)
					if finalErr == nil {
						finalErr = fmt.Errorf("publish failed for vm %s: cannot read executor logs: %w", vmName, err)
					}
				}
			}
		}

		if idx := lastScheduledIndex(policy.Status.Results, vmName); idx >= 0 {
			policy.Status.Results[idx].Phase = phase
			policy.Status.Results[idx].Reason = reason
			policy.Status.Results[idx].Message = message
			policy.Status.Results[idx].RotatedAt = completionTime
		} else {
			policy.Status.Results = append(policy.Status.Results, guestopsv1alpha1.RotationResult{
				VMName:    vmName,
				Phase:     phase,
				Reason:    reason,
				Message:   message,
				RotatedAt: completionTime,
			})
		}
		updated = true
		logger.Info("rotation job finished", "policy", policy.Name, "namespace", policy.Namespace, "vm", vmName, "job", job.Name, "phase", phase, "reason", reason)
	}
	return updated, finalErr
}

type executorResult struct {
	Status           string `json:"status"`
	Message          string `json:"message"`
	NewPasswordB64   string `json:"newPasswordB64,omitempty"`
	NewPrivateKeyB64 string `json:"newPrivateKeyB64,omitempty"`
	NewPublicKey     string `json:"newPublicKey,omitempty"`
}

func (r *AdminCredentialRotationPolicyReconciler) executorResultFromJobLogs(ctx context.Context, job *batchv1.Job) (executorResult, error) {
	if r.KubeClient == nil {
		return executorResult{}, fmt.Errorf("kube client not configured")
	}

	// Find pod created by Job controller
	pods, err := r.KubeClient.CoreV1().Pods(job.Namespace).List(ctx, metav1.ListOptions{LabelSelector: fmt.Sprintf("job-name=%s", job.Name)})
	if err != nil {
		return executorResult{}, err
	}
	if len(pods.Items) == 0 {
		return executorResult{}, fmt.Errorf("no pods found for job")
	}

	// Prefer the newest pod
	best := &pods.Items[0]
	for i := 1; i < len(pods.Items); i++ {
		p := &pods.Items[i]
		if p.CreationTimestamp.Time.After(best.CreationTimestamp.Time) {
			best = p
		}
	}

	tail := int64(200)
	limit := int64(65536)
	opts := &corev1.PodLogOptions{Container: "executor", TailLines: &tail, LimitBytes: &limit}
	req := r.KubeClient.CoreV1().Pods(job.Namespace).GetLogs(best.Name, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return executorResult{}, err
	}
	defer stream.Close()

	b, err := io.ReadAll(stream)
	if err != nil {
		return executorResult{}, err
	}
	res, ok := parseLastExecutorResult(string(b))
	if !ok {
		return executorResult{}, fmt.Errorf("no json message found in logs")
	}
	res.Message = strings.TrimSpace(res.Message)
	if res.Message != "" {
		res.Message = truncate(res.Message, 300)
	}
	return res, nil
}

func parseLastExecutorResult(logs string) (executorResult, bool) {
	lines := strings.Split(logs, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var res executorResult
		if err := json.Unmarshal([]byte(l), &res); err != nil {
			continue
		}
		if res.Status == "" && res.Message == "" {
			continue
		}
		return res, true
	}
	return executorResult{}, false
}

func publishConfig(policy *guestopsv1alpha1.AdminCredentialRotationPolicy) (publish.Mode, string) {
	if policy == nil || policy.Spec.Publish == nil {
		return publish.ModeNever, ""
	}
	mode := publish.Mode(strings.TrimSpace(policy.Spec.Publish.Mode))
	secretName := strings.TrimSpace(policy.Spec.Publish.SecretName)
	if secretName == "" {
		secretName = fmt.Sprintf("%s-publish", policy.Name)
	}
	return mode, secretName
}

func publishedBootstrapItems(publishSecret *corev1.Secret, vmName, methodType, rotationKind string) ([]corev1.KeyToPath, bool) {
	if publishSecret == nil || publishSecret.Data == nil {
		return nil, false
	}
	vmUserKey := fmt.Sprintf("%s.username", vmName)
	vmPassKey := fmt.Sprintf("%s.password", vmName)
	vmPrivKey := fmt.Sprintf("%s.privateKey", vmName)

	needPassword := (rotationKind == "linux-password" || rotationKind == "windows-password" || methodType == "winrm")
	needPrivateKey := (methodType == "ssh" && rotationKind == "ssh-key")

	if _, ok := publishSecret.Data[vmUserKey]; !ok {
		return nil, false
	}
	items := []corev1.KeyToPath{{Key: vmUserKey, Path: "username"}}
	if needPassword {
		if _, ok := publishSecret.Data[vmPassKey]; !ok {
			return nil, false
		}
		items = append(items, corev1.KeyToPath{Key: vmPassKey, Path: "password"})
	}
	if needPrivateKey {
		if _, ok := publishSecret.Data[vmPrivKey]; !ok {
			return nil, false
		}
		items = append(items, corev1.KeyToPath{Key: vmPrivKey, Path: "privateKey"})
	}
	return items, true
}

func (r *AdminCredentialRotationPolicyReconciler) publishExecutorResult(ctx context.Context, policy *guestopsv1alpha1.AdminCredentialRotationPolicy, mode publish.Mode, secretName, vmName, user, kind string, res executorResult) error {
	if mode != publish.ModeAlways {
		return nil
	}
	logger := log.FromContext(ctx)
	data := map[string][]byte{}
	data[fmt.Sprintf("%s.username", vmName)] = []byte(user)

	switch kind {
	case "linux-password", "windows-password":
		if strings.TrimSpace(res.NewPasswordB64) == "" {
			return fmt.Errorf("missing newPasswordB64 in executor output")
		}
		pw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.NewPasswordB64))
		if err != nil {
			return fmt.Errorf("invalid newPasswordB64: %w", err)
		}
		data[fmt.Sprintf("%s.password", vmName)] = pw
	case "ssh-key":
		if strings.TrimSpace(res.NewPrivateKeyB64) == "" {
			return fmt.Errorf("missing newPrivateKeyB64 in executor output")
		}
		pk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.NewPrivateKeyB64))
		if err != nil {
			return fmt.Errorf("invalid newPrivateKeyB64: %w", err)
		}
		data[fmt.Sprintf("%s.privateKey", vmName)] = pk
		if strings.TrimSpace(res.NewPublicKey) != "" {
			data[fmt.Sprintf("%s.publicKey", vmName)] = []byte(strings.TrimSpace(res.NewPublicKey))
		}
	default:
		return fmt.Errorf("unsupported rotation kind %q", kind)
	}

	labels := map[string]string{
		"guestops.io/policy": policy.Name,
	}
	if err := publish.Do(ctx, r.Client, policy.Namespace, mode, secretName, labels, data); err != nil {
		logger.Error(err, "publish failed", "policy", policy.Name, "namespace", policy.Namespace, "vm", vmName, "secret", secretName, "kind", kind)
		return err
	}
	logger.Info("published rotated credentials", "policy", policy.Name, "namespace", policy.Namespace, "vm", vmName, "secret", secretName, "kind", kind, "keys", len(data))
	return nil
}

func (r *AdminCredentialRotationPolicyReconciler) hasInFlightJobs(ctx context.Context, policy *guestopsv1alpha1.AdminCredentialRotationPolicy) (bool, error) {
	jobList, err := r.listJobsForPolicy(ctx, policy)
	if err != nil {
		return false, err
	}
	for i := range jobList.Items {
		job := &jobList.Items[i]
		if !isJobComplete(job) && !isJobFailed(job) {
			return true, nil
		}
	}
	return false, nil
}

func (r *AdminCredentialRotationPolicyReconciler) listJobsForPolicy(ctx context.Context, policy *guestopsv1alpha1.AdminCredentialRotationPolicy) (*batchv1.JobList, error) {
	if policy == nil {
		return &batchv1.JobList{}, nil
	}
	selector := fmt.Sprintf("guestops.io/policy=%s", policy.Name)
	if r.KubeClient != nil {
		return r.KubeClient.BatchV1().Jobs(policy.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	}
	jobList := &batchv1.JobList{}
	if err := r.List(ctx, jobList, client.InNamespace(policy.Namespace), client.MatchingLabels{"guestops.io/policy": policy.Name}); err != nil {
		return nil, err
	}
	return jobList, nil
}

func isJobComplete(j *batchv1.Job) bool {
	if j.Status.Succeeded > 0 {
		return true
	}
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isJobFailed(j *batchv1.Job) bool {
	if j.Status.Failed > 0 {
		return true
	}
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobFailureReason(j *batchv1.Job) string {
	for _, c := range j.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.Reason
		}
	}
	return ""
}

func envVal(c *corev1.Container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

func hasFinalResultAtTime(results []guestopsv1alpha1.RotationResult, vm string, t *metav1.Time) bool {
	if t == nil {
		return false
	}
	for i := len(results) - 1; i >= 0; i-- {
		r := results[i]
		if r.VMName != vm {
			continue
		}
		if r.Phase != "Completed" && r.Phase != "Failed" {
			continue
		}
		if r.RotatedAt == nil {
			continue
		}
		if r.RotatedAt.Time.Equal(t.Time) {
			return true
		}
	}
	return false
}

func lastScheduledIndex(results []guestopsv1alpha1.RotationResult, vm string) int {
	for i := len(results) - 1; i >= 0; i-- {
		if results[i].VMName == vm && results[i].Phase == "Scheduled" {
			return i
		}
	}
	return -1
}

func jobCompletionTime(job *batchv1.Job, fallback *metav1.Time) *metav1.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime
	}
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			t := c.LastTransitionTime
			return &t
		}
	}
	return fallback
}

func boolPtr(b bool) *bool { return &b }
