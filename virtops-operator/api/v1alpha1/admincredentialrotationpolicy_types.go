package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AdminCredentialRotationPolicySpec defines the desired state of the object.
// Fields are aligned with the CRD YAML in this repo (with dynamic networkSelection).
// Comments explain the intent for readers who are new to the project.

type NetworkSelection struct {
	// Mode controls the network selection strategy: auto, podOnly, or an explicit NAD list.
	// +kubebuilder:validation:Enum=auto;podOnly;nadList
	Mode string `json:"mode,omitempty"`
	// PreferPod indicates whether, in auto mode, to prefer the pod network when available.
	PreferPod bool `json:"preferPod,omitempty"`
	// NadList is the ordered list of NetworkAttachmentDefinitions to try in nadList mode,
	// or as a preference hint in auto mode.
	NadList []string `json:"nadList,omitempty"`
}

type Targets struct {
	// Selector filters VMs by label.
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
	// NetworkSelection enables per-VM dynamic network selection.
	NetworkSelection *NetworkSelection `json:"networkSelection,omitempty"`
	// NetworkAttachments keeps compatibility with older policies (explicit NAD list).
	NetworkAttachments []string `json:"networkAttachments,omitempty"`
}

type Method struct {
	// Type is the access method: ssh (Linux) or winrm (Windows).
	// +kubebuilder:validation:Enum=ssh;winrm
	Type string `json:"type"`
	// User to run the operation as (e.g. root or Administrator via WinRM).
	User string `json:"user,omitempty"`
	// Port to connect to (22 for SSH, 5986 for WinRM TLS).
	Port int32 `json:"port,omitempty"`
	// TLS indicates whether to use TLS (WinRM only).
	TLS bool `json:"tls,omitempty"`
	// Auth references the Secret containing the current credentials (bootstrap).
	Auth MethodAuth `json:"auth"`
}

type MethodAuth struct {
	// BootstrapSecretRef is the name of the Secret containing the current credentials for initial access.
	BootstrapSecretRef string `json:"bootstrapSecretRef"`
}

type PasswordPolicy struct {
	Length     *int32 `json:"length,omitempty"`
	MinLength  *int32 `json:"minLength,omitempty"`
	MaxLength  *int32 `json:"maxLength,omitempty"`
	MinUpper   *int32 `json:"minUpper,omitempty"`
	MinLower   *int32 `json:"minLower,omitempty"`
	MinDigits  *int32 `json:"minDigits,omitempty"`
	MinSpecial *int32 `json:"minSpecial,omitempty"`
}

type Rotation struct {
	// Kind is the rotation type: SSH key, Linux password, or Windows password.
	// +kubebuilder:validation:Enum=ssh-key;linux-password;windows-password
	Kind               string `json:"kind"`
	AuthorizedKeysMode string `json:"authorizedKeysMode,omitempty"`
	// Source of the new credential: generate (by the controller) or external (via ESO/Vault).
	// +kubebuilder:validation:Enum=generate;external
	Source string `json:"source"`
	// ExternalSecretRef is a logical reference to a Secret managed by External Secrets (future).
	ExternalSecretRef string `json:"externalSecretRef,omitempty"`
	// Length is the generated password length (when applicable).
	Length         *int32          `json:"length,omitempty"`
	PasswordPolicy *PasswordPolicy `json:"passwordPolicy,omitempty"`
	// OverlapSeconds is the coexistence window (e.g. temporarily keep two SSH keys).
	OverlapSeconds *int32 `json:"overlapSeconds,omitempty"`
}

type Publish struct {
	// Mode: Always | Never
	// +kubebuilder:validation:Enum=Always;Never
	Mode string `json:"mode,omitempty"`
	// SecretName is the Secret name to publish the rotated credential to (when allowed by Mode).
	SecretName string `json:"secretName,omitempty"`
}

type Safety struct {
	RetryAttempts  *int32 `json:"retryAttempts,omitempty"`
	BackoffSeconds *int32 `json:"backoffSeconds,omitempty"`
	PauseOnError   bool   `json:"pauseOnError,omitempty"`
	MaxFailures    *int32 `json:"maxFailures,omitempty"`
}

type Concurrency struct {
	MaxConcurrent              *int32 `json:"maxConcurrent,omitempty"`
	ReachabilityTimeoutSeconds *int32 `json:"reachabilityTimeoutSeconds,omitempty"`
}

type Trigger struct {
	EnableAnnotation bool `json:"enableAnnotation,omitempty"`
}

// AdminCredentialRotationPolicySpec defines how and when to rotate credentials.
type AdminCredentialRotationPolicySpec struct {
	Targets     Targets      `json:"targets"`
	OS          string       `json:"os"` // +kubebuilder:validation:Enum=linux;windows
	Method      Method       `json:"method"`
	Rotation    Rotation     `json:"rotation"`
	Schedule    string       `json:"schedule,omitempty"`
	Publish     *Publish     `json:"publish,omitempty"`
	Safety      *Safety      `json:"safety,omitempty"`
	Concurrency *Concurrency `json:"concurrency,omitempty"`
	Trigger     *Trigger     `json:"trigger,omitempty"`
}

// RotationResult summarizes the rotation outcome for a single VM.
type RotationResult struct {
	VMName    string       `json:"vmName,omitempty"`
	Phase     string       `json:"phase,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Message   string       `json:"message,omitempty"`
	RotatedAt *metav1.Time `json:"rotatedAt,omitempty"`
}

// AdminCredentialRotationPolicyStatus tracks the policy status.
type AdminCredentialRotationPolicyStatus struct {
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
	NextRunTime *metav1.Time       `json:"nextRunTime,omitempty"`
	LastRunTime *metav1.Time       `json:"lastRunTime,omitempty"`
	Results     []RotationResult   `json:"results,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=admincredentialrotationpolicies,scope=Namespaced,shortName=acrp
// +kubebuilder:printcolumn:name="OS",type=string,JSONPath=.spec.os
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=.spec.schedule
// +kubebuilder:printcolumn:name="Publish",type=string,JSONPath=.spec.publish.mode
// +kubebuilder:printcolumn:name="LastRun",type=date,JSONPath=.status.lastRunTime

type AdminCredentialRotationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AdminCredentialRotationPolicySpec   `json:"spec,omitempty"`
	Status AdminCredentialRotationPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type AdminCredentialRotationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AdminCredentialRotationPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AdminCredentialRotationPolicy{}, &AdminCredentialRotationPolicyList{})
}
