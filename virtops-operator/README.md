# virtops-operator (GuestOps & Admin Credential Rotation)

This repository contains the `virtops-operator` implementation (Operator SDK / controller-runtime) for rotating administrative credentials inside KubeVirt / OpenShift Virtualization VMs.

## Layout
- `main.go`: manager startup, logging, healthz/readyz, controller registration.
- `api/v1alpha1/`: API types (`AdminCredentialRotationPolicy`).
- `controllers/`: reconciler for `AdminCredentialRotationPolicy`, including:
  - cron scheduling and manual trigger via annotation `guestops.io/rotate-now`;
  - VMI discovery via LabelSelector;
  - dynamic network selection (pod or Multus NAD) and `k8s.v1.cni.cncf.io/networks` annotation on Jobs;
  - per-VM ephemeral Job creation with executor environment configuration;
  - status updates (`lastRunTime`, `nextRunTime`, `results`).
- `pkg/`: internal packages (some are placeholders) for network selection, executor stubs, scheduler, publish, safety.
- `config/`: base manifests (RBAC, CRD). The operator is typically deployed in namespace `virt-ops` and watches all namespaces by default (cluster-wide watch).

## Local build
- Requirements: Go >= 1.25
- Basic build:
```
go build ./...
```

## Running against a cluster (development)
- `WATCH_NAMESPACE` is optional: when set, it limits the watch scope to that namespace; when unset, the operator watches all namespaces (cluster-wide).
- Apply RBAC and CRD (minimal set):
```
kubectl apply -f config/rbac/service_account.yaml
kubectl apply -f config/rbac/role.yaml
kubectl apply -f config/rbac/role_binding.yaml
kubectl apply -f config/crd/bases/admincredentialrotationpolicies.guestops.io.crd.yaml
```
- Run the manager locally against the cluster (using your current kubeconfig context):
```
go run ./main.go
```

## Implemented features (MVP)
- **Cron scheduler**: 5-field cron parsing, `nextRunTime` calculation, execution when due.
- **Manual trigger**: annotation `guestops.io/rotate-now` (removed after execution).
- **Dynamic network selection**: pod network or Multus NAD, with preferences from `spec.targets.networkSelection`.
- **Per-VM ephemeral Jobs**: one Job for each target VMI; Multus annotation applied when needed; executor env/file inputs.
- **Status**: `status.lastRunTime`, `status.nextRunTime`, `status.results` for audit.
- **External credential source (ESO/Vault)**: when `spec.rotation.source: external`, the controller mounts a Secret synced by External Secrets Operator into the executor Job, instead of having the executor generate the credential.

### SSH executor (real)
- The controller uses the image defined in `SSH_EXECUTOR_IMAGE`. When it is set to a real executor image (≠ `alpine:3.19`), the Job container runs `/executor` (instead of the placeholder) and:
  - mounts an `EmptyDir` at `/work` (writable) to generate keys with a read-only root filesystem;
  - mounts the bootstrap Secret at `/bootstrap` when `spec.method.auth.bootstrapSecretRef` is set.
- Default timeouts: `CONNECT_TIMEOUT_SECONDS=30`, `EXEC_TIMEOUT_SECONDS=60`; `MODE_REPLACE=true` (replaces `authorized_keys`).
- Image pull policy: Job pods set `imagePullPolicy: Always` to make testing with reused tags easier and ensure the latest executor build is pulled. In production, consider immutable digests (`@sha256:<digest>`) for stability.
- SSH behavior in-container: `HOME=/work` (writable `EmptyDir`), `UserKnownHostsFile=/dev/null` and `GlobalKnownHostsFile=/dev/null` to avoid writes on read-only rootfs and `known_hosts`.
- Executor inputs via mounted files:
  - `/bootstrap/username`, `/bootstrap/password`, `/bootstrap/privateKey` (optional depending on the bootstrap method).
- In addition to SSH key rotation (`rotation.kind=ssh-key`), it also supports Linux password rotation (`rotation.kind=linux-password`, `source=generate`):
  - it generates a new password (via `passwordPolicy` or legacy `length`) and attempts to apply it first via `passwd` (self-change, when the current password is available from bootstrap), then via `chpasswd` (with `sudo -n` when available);
  - it does not require sudo if the remote user can change its own password (and the current password is present in the bootstrap Secret); otherwise you need root or passwordless sudo on the target;
  - output: JSON containing `newPasswordB64` in the executor pod logs.

### WinRM executor (real)
- The controller uses the image defined in `WINRM_EXECUTOR_IMAGE`. When it is set to a real executor image (≠ `alpine:3.19`), the Job container runs `/executor` and:
  - mounts an `EmptyDir` at `/tmp` to support Python libraries with `readOnlyRootFilesystem: true`;
  - mounts the bootstrap Secret at `/bootstrap` when `spec.method.auth.bootstrapSecretRef` is set.
- Default ports:
  - `5985` when `spec.method.tls: false`
  - `5986` when `spec.method.tls: true`
- Requirement: for WinRM you must set `spec.method.user` (e.g. `Administrator`), used as `TARGET_USER`.
- Output: the executor prints a final JSON line with `newPasswordB64` (new password in base64). The password is not written into `.status`.

## Configuration
- Deployment env vars (see `config/manager/deployment.yaml`):
  - `WATCH_NAMESPACE` (optional): when set, restricts the watch to a single namespace; when unset, the operator watches cluster-wide.
  - `SSH_EXECUTOR_IMAGE`: SSH executor container image. If equal to `alpine:3.19` (default), Jobs run a placeholder (`/bin/sh -c echo ...`). Otherwise Jobs run the real executor (`/executor`).
  - `WINRM_EXECUTOR_IMAGE`: WinRM executor container image. If equal to `alpine:3.19`, Jobs run a placeholder. Otherwise Jobs run the real executor (`/executor`).

### OpenShift security (SCC)
- The Deployment does not set a fixed `fsGroup` to remain compatible with the OpenShift `restricted` SCC. Pods run as non-root and the platform assigns an allowed group automatically.

## Operator Docker image
Multi-stage build is ready:
```
docker build -t YOUR_REGISTRY/virtops-operator:dev .
docker push YOUR_REGISTRY/virtops-operator:dev
```
Update `config/manager/deployment.yaml` with your image and apply the Deployment.

### OpenShift offline build (vendoring)
To avoid network dependencies during build, the Dockerfile is configured to use vendored modules (`GOFLAGS=-mod=vendor`) and the `golang:1.25` builder image.

Recommended steps to generate `vendor/` directly on OpenShift:

1. Create a builder Pod with Go 1.25:
   ```
   oc -n virt-ops run vendorgen --image=golang:1.25 --restart=Never --command -- sleep 3600
   ```
2. Copy the project contents into the Pod root (note the `/.` suffix):
   ```
   oc -n virt-ops cp <PATH_TO_REPO>/. vendorgen:/workspace
   ```
3. Regenerate the vendor directory in the Pod (correct path: `/workspace/vendor`):
   ```
   oc -n virt-ops exec vendorgen -- sh -lc 'cd /workspace && rm -rf vendor go.sum && go clean -modcache && go mod tidy && go mod vendor'
   ```
4. Copy `vendor/` and also `go.mod` and `go.sum` back to your local workspace:
   ```
   oc -n virt-ops cp vendorgen:/workspace/vendor <PATH_TO_REPO>/vendor
   oc -n virt-ops cp vendorgen:/workspace/go.mod <PATH_TO_REPO>/go.mod
   oc -n virt-ops cp vendorgen:/workspace/go.sum <PATH_TO_REPO>/go.sum
   ```
5. Remove the temporary Pod:
   ```
   oc -n virt-ops delete pod vendorgen
   ```
6. If you use a Git-based BuildConfig, commit the files:
   ```
   git add vendor go.mod go.sum Dockerfile
   git commit -m "Vendor deps and Dockerfile with -mod=vendor"
   git push
   ```
7. Start the build on OpenShift:
  - If a Git BuildConfig is already configured: `oc start-build virtops-operator --follow`
  - Or with a binary build:
     ```
     oc new-build --strategy=docker --binary --name=virtops-operator
     oc start-build virtops-operator --from-dir=<PATH_TO_REPO> --follow
     ```

## CRD quick reference (main spec fields)
- `spec.targets.selector`: LabelSelector used to filter VMIs.
- `spec.targets.networkSelection`:
  - `mode`: `auto` | `podOnly` | `nadList`
  - `preferPod`: in `auto` mode, prefer pod network when an IP is available.
  - `nadList`: preferred NAD list (used in `nadList` and as hints for `auto`).
- `spec.os`: `linux` | `windows`.
- `spec.method`: `type` (`ssh`|`winrm`), `user`, `port`, `tls`, `auth.bootstrapSecretRef`.
- `spec.rotation`: `kind` (`ssh-key`|`linux-password`|`windows-password`), `source` (`generate`|`external`), other optional fields.
  - WinRM: `spec.rotation.passwordPolicy` (preferred) or legacy `spec.rotation.length`.
- `spec.publish`: publish the rotated credential into a Kubernetes Secret and automatically bootstrap subsequent runs from the published values.
  - `spec.publish.mode`: `Always` | `Never`
  - `spec.publish.secretName`: optional (default: `<policy-name>-publish`)
- `spec.schedule`: 5-field cron expression.
- `spec.concurrency.maxConcurrent`: Job limit per run.
- `spec.trigger.enableAnnotation`: enables the manual trigger.

Full reference: `config/crd/bases/admincredentialrotationpolicies.guestops.io.crd.yaml`.

## Dynamic network selection (summary)
- The controller inspects target VMIs and uses `pkg/network/selection_vmi.go` to select either the pod network (when an IP is available) or a NAD matching the configured preferences.
- When a NAD is selected, the Job pod receives the `k8s.v1.cni.cncf.io/networks` annotation set to that NAD name.
- Fallback: `spec.targets.networkAttachments` (compatibility with older configs).

## Execution: ephemeral Jobs
- One Job per target VMI.
- When `SSH_EXECUTOR_IMAGE` or `WINRM_EXECUTOR_IMAGE` are set to real executor images, the container runs `/executor` and receives env/file-path inputs:
  - `TARGET_IP`, `TARGET_USER`, `TARGET_PORT` (defaults: 22 for SSH, 5985 for WinRM non-TLS, 5986 for WinRM TLS);
  - `ROTATION_KIND`, `ROTATION_SOURCE`;
  - `CONNECT_TIMEOUT_SECONDS=30`, `EXEC_TIMEOUT_SECONDS=60`.
- SSH:
  - `MODE_REPLACE=true`;
  - when a bootstrap Secret is present: `BOOTSTRAP_USERNAME_FILE=/bootstrap/username`, `BOOTSTRAP_PASSWORD_FILE=/bootstrap/password`, `BOOTSTRAP_PRIVATEKEY_FILE=/bootstrap/privateKey`.
- External source (when `source=external`):
  - `EXTERNAL_CREDENTIAL_FILE=/external/<key>` (path to the credential file from the ESO-synced Secret).
- WinRM:
  - `WINRM_TLS=true|false`;
  - `PASSWORD_POLICY_LENGTH` (optional; exact length; highest precedence).
  - `PASSWORD_POLICY_MIN_LENGTH` / `PASSWORD_POLICY_MAX_LENGTH` (optional; range; random length within range).
  - `PASSWORD_POLICY_MIN_UPPER`, `PASSWORD_POLICY_MIN_LOWER`, `PASSWORD_POLICY_MIN_DIGITS`, `PASSWORD_POLICY_MIN_SPECIAL` (optional; minimum constraints).
  - `PASSWORD_LENGTH` (legacy; used only when `passwordPolicy` is not set; executor default: 24).
- Volumes:
  - `EmptyDir` at `/work` (writable) for key generation;
  - `EmptyDir` at `/tmp` (writable) for WinRM/Python with read-only rootfs;
  - bootstrap Secret at `/bootstrap` (when configured);
  - external Secret at `/external` (when `source=external`).
- Job names are guaranteed to be ≤ 63 characters via truncation and hash suffix.
- Auto-cleanup: completed Jobs are automatically removed after 600 seconds (`ttlSecondsAfterFinished=600`).

## Quick test
1. Apply RBAC + CRD and the operator Deployment (with your pushed image).
2. Create bootstrap Secrets (see `community-release/examples/secrets/`).
3. Apply an example `AdminCredentialRotationPolicy` (see `community-release/examples/`).
4. Manual trigger: `kubectl annotate acrp <policy> guestops.io/rotate-now=$(date +%s) --overwrite`.
5. Verify Jobs/Pods and Multus annotations:
   - `kubectl get jobs -n <ns> -l guestops.io/policy=<policy> -o yaml`
6. Check policy status: `kubectl get acrp <policy> -o yaml`.

## Secret publishing (spec.publish)

The controller publishes rotated credentials into a Kubernetes Secret so that subsequent rotation runs can use them as bootstrap credentials.

**Default**: `spec.publish.mode` defaults to `Always` when unset. This ensures continuous autonomous rotation: without publishing, the bootstrap credentials would become stale after the first run and all subsequent runs would fail.

When `spec.publish.mode: Always` (default), the controller:

- Reads the executor Pod logs (last JSON line) to extract the rotated credential (e.g. `newPasswordB64`, `newPrivateKeyB64`).
- Creates/updates a publish Secret (default name: `<policy-name>-publish`) in the policy namespace.

To explicitly disable publishing, set `spec.publish.mode: Never`. This is only appropriate for one-shot rotations or when an external system manages bootstrap credentials.

Secret data format (per-VM keys):

- `VMNAME.username`
- `VMNAME.password` (for `linux-password` and `windows-password`)
- `VMNAME.privateKey` and `VMNAME.publicKey` (for `ssh-key`)

Automatic bootstrap:

- If the publish Secret exists and contains the required keys for that VM, subsequent Jobs will use that Secret as bootstrap credentials instead of `spec.method.auth.bootstrapSecretRef`.
- This is essential for both `source=generate` and `source=external`: after the first run, the VM's credential no longer matches `bootstrapSecretRef`, so the publish Secret becomes the only valid bootstrap source.

RBAC requirement:

- The operator ServiceAccount must be allowed to read pod logs (`get` on `pods/log`) in target namespaces, otherwise publishing will fail.

## External credential source (ESO/Vault)

When `spec.rotation.source: external`, the controller expects a Kubernetes Secret (synced by External Secrets Operator from Vault or another backend) to exist in the policy namespace. The Secret name is specified in `spec.rotation.externalSecretRef`.

### How it works
1. Before scheduling Jobs, the controller checks that the referenced Secret exists and contains the expected key.
2. If the Secret is missing or the key is absent, the controller sets `status.conditions[ExternalSecretReady]=False` and requeues after 30 seconds. The `rotate-now` annotation is preserved so the rotation runs automatically once the Secret is synced.
3. When the Secret is ready, the controller mounts it at `/external` (read-only) in the executor Job and sets `EXTERNAL_CREDENTIAL_FILE=/external/<key>`.
4. The executor reads the credential from that file instead of generating one.

### Default Secret keys
- `ssh-key`: `privateKey`
- `linux-password`: `password`
- `windows-password`: `password`

Override with `spec.rotation.externalSecretKey` if your ESO `ExternalSecret` uses a different key name.

### Example with ESO
```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: vault-ssh-key
  namespace: virt-ops
spec:
  secretStoreRef:
    name: vault-backend
    kind: SecretStore
  target:
    name: vault-ssh-key-secret
  data:
    - secretKey: privateKey
      remoteRef:
        key: secret/ssh/admin
        property: private_key
---
apiVersion: guestops.io/v1alpha1
kind: AdminCredentialRotationPolicy
metadata:
  name: linux-ssh-external
  namespace: virt-ops
spec:
  targets:
    selector:
      matchLabels:
        role: linux-admin
  os: linux
  method:
    type: ssh
    user: root
    port: 22
    auth:
      bootstrapSecretRef: bootstrap-ssh
  rotation:
    kind: ssh-key
    source: external
    externalSecretRef: vault-ssh-key-secret
    authorizedKeysMode: replace
  schedule: "0 */12 * * *"
  trigger:
    enableAnnotation: true
```

### Vault-driven mode (automatic ExternalSecret creation)

When `spec.rotation.source: external` and `spec.rotation.vault` is configured, the controller fully automates the ESO integration:

1. **Creates an ExternalSecret** in the policy namespace, referencing a `ClusterSecretStore` (created manually in the operator namespace for security).
2. **Polls the synced Secret** every 15 seconds for the `reconcile.external-secrets.io/data-hash` annotation.
3. **Triggers rotation automatically** when the data-hash changes (i.e. Vault updated the credential and ESO synced it).
4. **Cron schedule is ignored** — rotation is driven entirely by Vault changes.

This means: update the credential in Vault → ESO syncs → controller detects data-hash change → rotation runs automatically. No cron, no manual annotation needed. The rotation triggers within ~15 seconds of the ESO sync.

The `ClusterSecretStore` must be created manually (it contains the Vault token and should be protected by RBAC). The controller only needs `get`/`list`/`watch` on `secretstores` and `clustersecretstores`.

### Manual mode (externalSecretRef)

When `spec.rotation.source: external` and `spec.rotation.vault` is **not** set, the controller expects the Secret to exist already (created manually or by a separate ExternalSecret). The Secret name is specified in `spec.rotation.externalSecretRef`. Cron schedule and manual annotation work as usual.

### Vault config fields

| Field | Description |
|-------|-------------|
| `vault.secretStoreRef` | Name of the `ClusterSecretStore` to reference |
| `vault.secretPath` | Path in Vault (e.g. `ssh/admin`) |
| `vault.property` | Field name in the Vault secret (e.g. `private_key`). Optional — if omitted, the entire secret value is used. |

### Example: vault-driven SSH key rotation

Prerequisite: create a `ClusterSecretStore` manually (one-time, in the operator namespace):

```yaml
apiVersion: external-secrets.io/v1
kind: ClusterSecretStore
metadata:
  name: vault-backend
spec:
  provider:
    vault:
      server: "http://vault.vault.svc:8200"
      path: "secret"
      version: "v2"
      auth:
        tokenSecretRef:
          name: vault-token
          namespace: virt-ops
          key: token
```

Then create the ACRP — no schedule, no externalSecretRef, no ExternalSecret to manage:

```yaml
apiVersion: guestops.io/v1alpha1
kind: AdminCredentialRotationPolicy
metadata:
  name: linux-ssh-vault
  namespace: test-vms
spec:
  targets:
    selector:
      matchLabels:
        role: linux-admin
  os: linux
  method:
    type: ssh
    user: root
    port: 22
    auth:
      bootstrapSecretRef: bootstrap-ssh
  rotation:
    kind: ssh-key
    source: external
    vault:
      secretStoreRef: vault-backend
      secretPath: ssh/admin
      property: private_key
    authorizedKeysMode: replace
  trigger:
    enableAnnotation: true
```

When the credential in Vault is updated, the rotation runs automatically within seconds.

## Limitations (roadmap)
- Active reachability checks are not implemented yet.
- Safety (retry/backoff/circuit breaker), metrics, and richer Conditions will be added later.

## References
- Installation manifests and examples: `community-release/`.

## Troubleshooting
- **Cannot create Job: `must be no more than 63 characters`**
  - Names are automatically truncated; rebuild and redeploy the operator if you still see the error.

- **OwnerReference error: `cannot set blockOwnerDeletion ... finalizers`**
  - Ensure you applied the updated RBAC: the ClusterRole must include `admincredentialrotationpolicies/finalizers` with `verbs: ["update"]`.

- **Executor image pull failed (authentication required)**
  - Grant the policy namespace the `system:image-puller` role on the project that hosts the image:
    - All ServiceAccounts in the consumer namespace: `oc -n virt-ops policy add-role-to-group system:image-puller system:serviceaccounts:<ns-policy>`
    - Only the default ServiceAccount: `oc -n virt-ops policy add-role-to-user system:image-puller system:serviceaccount:<ns-policy>:default`
  - Alternatively, retag the image into the policy namespace and update `SSH_EXECUTOR_IMAGE`.

- **Publishing fails with `forbidden` on `pods/log`**
  - Verify the operator ClusterRole includes `resources: ["pods/log"]` with `verbs: ["get"]` and that it is bound to the `virtops-operator` ServiceAccount.

- **SSH: `Load key "...": invalid format` after copying a key from logs**
  - Use `newPrivateKeyB64` and decode with `base64 -d` (Linux) or `base64 -D` (macOS): see “Extracting keys from Job logs”.
  - Ensure the file ends with a newline; if in doubt: `printf '\n' >> key`.

- **Warning SSH: `Permanently added 'X' (ED25519) to the list of known hosts.`**
  - This is informational. `known_hosts` files are not written because they are set to `/dev/null`.

- **Inconsistent vendoring ("marked as explicit in vendor/modules.txt ...")**
  - Regenerate vendor in a Go 1.25 Pod and also copy `go.mod` and `go.sum` back:
    ```
    oc -n virt-ops run vendorgen --image=golang:1.25 --restart=Never --command -- sleep 3600
    oc -n virt-ops cp <PATH_TO_VIRTOPS_OPERATOR>/. vendorgen:/workspace
    oc -n virt-ops exec vendorgen -- sh -lc 'cd /workspace && rm -rf vendor go.sum && go clean -modcache && go mod tidy && go mod vendor'
    oc -n virt-ops cp vendorgen:/workspace/vendor <PATH_TO_VIRTOPS_OPERATOR>/vendor
    oc -n virt-ops cp vendorgen:/workspace/go.mod <PATH_TO_VIRTOPS_OPERATOR>/go.mod
    oc -n virt-ops cp vendorgen:/workspace/go.sum <PATH_TO_VIRTOPS_OPERATOR>/go.sum
    oc -n virt-ops delete pod vendorgen
    ```
  - In the Dockerfile use `GOFLAGS=-mod=vendor`.
  - Build-time debugging (optional):
    ```
    RUN echo "GOMOD=$(go env GOMOD)" && head -n 30 go.mod && sed -n '1,120p' vendor/modules.txt
    ```

- **`controller-runtime` Options errors (e.g. `MetricsBindAddress`, `Port`, `Namespace`)**
  - With v0.16.x use `ctrl.Options{ Metrics: metricsserver.Options{BindAddress: ...}, Cache: cache.Options{...} }`.
  - This repo is already aligned to `controller-runtime v0.16.3`.

- **OpenShift SCC `restricted`: Pod rejected because of a fixed `fsGroup`**
  - Do not enforce a static `fsGroup` in the Deployment. Pods run as non-root and OCP assigns an allowed group automatically.
  - Avoid forcing `runAsUser`/`fsGroup` values not allowed by the SCC.

- **Operator scope: cluster-wide vs single namespace**
  - By default the operator watches all namespaces (cluster-wide).
  - To restrict to a specific namespace, set `WATCH_NAMESPACE` in the Deployment.
  - RBAC: use `ClusterRole` and `ClusterRoleBinding` to operate cross-namespace.

- **No Jobs created by the policy**
  - Verify VMI labels match `spec.targets.selector`.
  - If you use Multus NADs, ensure the NAD exists in the VMI/policy namespace and that the `k8s.v1.cni.cncf.io/networks` annotation is present on the Job pod.
  - Check controller logs: `oc -n virt-ops logs -f deploy/virtops-operator`.
  - Ensure `spec.trigger.enableAnnotation: true` is set when using the manual trigger.
  - If `spec.concurrency.maxConcurrent` is `0`, no Job will be scheduled: set it to a value ≥ 1.

## SSH executor (build and usage)
- Dockerfile and entrypoint are in `executors/ssh/`.
- Build/push on OpenShift (example with Binary Build):
  ```
  oc -n virt-ops new-build --strategy=docker --binary --name=virtops-ssh-executor || true
  oc -n virt-ops start-build virtops-ssh-executor --from-dir=executors/ssh --follow
  ```
- Configure the operator Deployment to use the real image:
  ```
  oc -n virt-ops set env deploy/virtops-operator \
    SSH_EXECUTOR_IMAGE=image-registry.openshift-image-registry.svc:5000/virt-ops/virtops-ssh-executor:latest
  oc -n virt-ops rollout restart deploy/virtops-operator
  ```

## authorized_keys update mode (`rotation.kind: ssh-key`)
  - `spec.rotation.authorizedKeysMode: replace` (default): overwrites `~/.ssh/authorized_keys`.
  - `spec.rotation.authorizedKeysMode: append`: appends the new key if not present.

## WinRM executor (build and usage)
- Dockerfile and entrypoint are in `executors/winrm/`.
- Build/push on OpenShift (example with Binary Build):
  ```
  oc -n virt-ops new-build --strategy=docker --binary --name=virtops-winrm-executor || true
  oc -n virt-ops start-build virtops-winrm-executor --from-dir=executors/winrm --follow
  ```
- Configure the operator Deployment to use the real image:
  ```
  oc -n virt-ops set env deploy/virtops-operator \
    WINRM_EXECUTOR_IMAGE=image-registry.openshift-image-registry.svc:5000/virt-ops/virtops-winrm-executor:latest
  oc -n virt-ops rollout restart deploy/virtops-operator
  ```

## Extracting keys from Job logs

- The executor Pod prints a final JSON line with fields:
  - `status`, `message`, `newPublicKey`
  - `newPrivateKeyB64` (base64, without newline) when `source=generate`.

- Recommended extraction (base64):
  - Linux:
    ```
    oc logs -n <ns> <job-pod> | grep -oE '{.*}' | tail -n1 \
      | jq -r '.newPrivateKeyB64' | base64 -d > new_key.key
    chmod 600 new_key.key
    ```
  - macOS:
    ```
    oc logs -n <ns> <job-pod> | grep -oE '{.*}' | tail -n1 \
      | jq -r '.newPrivateKeyB64' | base64 -D > new_key.key
    chmod 600 new_key.key
    ```

- Verify and use:
  ```
  ssh-keygen -y -f new_key.key  # must match newPublicKey
  ssh -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -i new_key.key <user>@<ip>
  ```

## Extracting Windows password from Job logs (WinRM)
- The WinRM executor Pod prints a final JSON line with `newPasswordB64`.
- Extraction (base64):
  - Linux:
    ```
    oc logs -n <ns> <job-pod> | grep -oE '{.*}' | tail -n1 \
      | jq -r '.newPasswordB64' | base64 -d
    ```
  - macOS:
    ```
    oc logs -n <ns> <job-pod> | grep -oE '{.*}' | tail -n1 \
      | jq -r '.newPasswordB64' | base64 -D
    ```
