# GuestOps Credential Rotation

## What it is

`virtops-operator` is a Kubernetes operator that performs **in-guest admin credential rotation** for **KubeVirt / OpenShift Virtualization** virtual machines.

It works by:

- Watching `AdminCredentialRotationPolicy` (ACRP) custom resources.
- Selecting `VirtualMachineInstance` (VMI) targets via a label selector.
- Creating **ephemeral Kubernetes Jobs** (one per target VM) that run a dedicated **executor** image.
- Writing an audit trail into the ACRP `.status` (timestamps + per-VM results). The new credentials are **not** stored in the CR status.

Currently implemented rotations:

- **Linux**: SSH **public key rotation** (updates `~/.ssh/authorized_keys`).
- **Linux**: SSH **password rotation** (updates the local account password).
- **Windows**: **local password rotation** via WinRM (NTLM over port `5985`).

## How to install

This release is meant to be installed with **Kustomize**.

From the repository root, update the images in `manifests/deployment.yaml` if needed, then apply:

```bash
kubectl apply -k .
```

This installs:

- Namespace `virt-ops`
- CRD `AdminCredentialRotationPolicy`
- ClusterRole / ClusterRoleBinding
- Deployment `virtops-operator`

Notes:

- The ACRP resource is **namespaced**. A policy acts on VMIs in the **same namespace** as the policy.
- The operator can watch **all namespaces** by default (cluster-wide watch), while still using a namespaced CRD.

## AdminCredentialRotationPolicy spec reference

This section documents the fields you can set under `.spec`, their accepted values, and whether they are currently implemented.

### Core fields

| Field | Type | Description | Allowed values / default | Status |
| --- | --- | --- | --- | --- |
| `spec.os` | string | Guest OS family. | `linux` \| `windows` | Implemented |
| `spec.schedule` | string | Cron schedule (5 fields). If empty, rotations run only on manual trigger. | Example: `0 */12 * * *` | Implemented |
| `spec.concurrency.maxConcurrent` | int | Max number of per-VM Jobs created per run. | Default: all matched VMIs | Implemented |
| `spec.concurrency.reachabilityTimeoutSeconds` | int | Timeout used for pre-flight reachability checks. | `>= 1` | Roadmap |
| `spec.trigger.enableAnnotation` | bool | Enables manual trigger via annotation `guestops.io/rotate-now`. | Default: `false` | Implemented |

### Targets (`spec.targets`)

| Field | Type | Description | Allowed values / default | Status |
| --- | --- | --- | --- | --- |
| `spec.targets.selector.matchLabels` | map[string]string | Selects the target VMIs in the policy namespace. If omitted, all VMIs in the namespace are eligible. | Any label map | Implemented |
| `spec.targets.networkAttachments` | []string | List of Multus NetworkAttachmentDefinitions (NAD) preferred for reaching the guest. | Each entry is a NAD `networkName` (commonly `name` or `namespace/name`) | Implemented (best-effort) |
| `spec.targets.networkSelection.mode` | string | Advanced network selection mode. | `auto` \| `podOnly` \| `nadList` (default: `auto`) | Implemented (best-effort) |
| `spec.targets.networkSelection.preferPod` | bool | In `auto` mode, prefer Pod network if an IP is present. | Default: `true` | Implemented |
| `spec.targets.networkSelection.nadList` | []string | Ordered NAD preferences used by `auto`/`nadList`. | List of NAD `networkName` values | Implemented |

Secondary networks notes:

- The operator selects a NAD by matching your NAD names against the VMI `.spec.networks[].multus.networkName` values.
- When a NAD is selected, the Job pod gets the Multus annotation `k8s.v1.cni.cncf.io/networks` set to that NAD name.
- Current selection is best-effort: it does not verify reachability and the chosen IP comes from the first available VMI `status.interfaces[].ipAddress`.

Example values:

```yaml
spec:
  targets:
    networkAttachments:
      - corp-net
      - virt-ops/corp-net
```

or

```yaml
spec:
  targets:
    networkSelection:
      mode: nadList
      nadList:
        - virt-ops/corp-net
```

### Method (`spec.method`)

| Field | Type | Description | Allowed values / default | Status |
| --- | --- | --- | --- | --- |
| `spec.method.type` | string | How the executor connects to the guest. | `ssh` \| `winrm` | Implemented |
| `spec.method.user` | string | Guest account to rotate (SSH: remote user; WinRM: local user). | Required by executors | Implemented |
| `spec.method.port` | int | TCP port for SSH/WinRM. | SSH default: `22`. WinRM default: `5985` (TLS=false) / `5986` (TLS=true) | Implemented |
| `spec.method.tls` | bool | WinRM TLS toggle (affects default port and endpoint scheme). | Default: `false` | Implemented (WinRM only) |
| `spec.method.auth.bootstrapSecretRef` | string | Secret name providing initial access credentials (“bootstrap credentials”). | Required in current implementation | Implemented |

### Rotation (`spec.rotation`)

| Field | Type | Description | Allowed values / default | Status |
| --- | --- | --- | --- | --- |
| `spec.rotation.kind` | string | What is rotated. | `ssh-key` \| `windows-password` \| `linux-password` | Implemented |
| `spec.rotation.source` | string | Where the new credential comes from. | `generate` \| `external` | Implemented |
| `spec.rotation.externalSecretRef` | string | External Secret reference used when `source=external` (manual mode). | Free-form string | Implemented |
| `spec.rotation.externalSecretKey` | string | Key within the external Secret that holds the credential. Defaults to `privateKey` for `ssh-key`, `password` for password kinds. | Free-form string | Implemented |
| `spec.rotation.vault` | object | Vault-driven mode: auto-creates ExternalSecret and triggers rotation on data-hash changes. See [Vault-driven mode](#vault-driven-mode). | Object with `secretStoreRef`, `secretPath`, `property` | Implemented |
| `spec.rotation.authorizedKeysMode` | string | SSH authorized_keys update strategy. | `replace` (default) \| `append` | Implemented (SSH only) |
| `spec.rotation.length` | int | Length of generated password (legacy). | Used only when `spec.rotation.passwordPolicy` is not set. Default: executor uses `24`. | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.passwordPolicy` | object | Password generation policy. | See fields below. | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.passwordPolicy.length` | int | Exact password length. | `>= 8` | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.passwordPolicy.minLength` | int | Minimum password length (range mode). | `>= 8` | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.passwordPolicy.maxLength` | int | Maximum password length (range mode). | `>= 8` | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.passwordPolicy.minUpper` | int | Minimum uppercase characters. | `>= 0` | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.passwordPolicy.minLower` | int | Minimum lowercase characters. | `>= 0` | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.passwordPolicy.minDigits` | int | Minimum digits (includes `0`). | `>= 0` | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.passwordPolicy.minSpecial` | int | Minimum special characters (safe charset). | `>= 0` | Implemented (WinRM + SSH linux-password) |
| `spec.rotation.overlapSeconds` | int | Coexistence window for old/new credentials. | `>= 0` | Roadmap |

Password policy precedence:

- **`passwordPolicy.length`** overrides `passwordPolicy.minLength/maxLength`.
- If `passwordPolicy` is set but no length/range is provided, `spec.rotation.length` is used if present, otherwise the default is `24`.

### Publish (`spec.publish`)

| Field | Type | Description | Allowed values / default | Status |
| --- | --- | --- | --- | --- |
| `spec.publish.mode` | string | Whether and when to publish rotated credentials to a Kubernetes Secret. Defaults to `Always` when unset. | `Always` (default) \| `Never` | Implemented |
| `spec.publish.secretName` | string | Target Secret name to create/update when publishing. | string (default: `<policy-name>-publish`) | Implemented |

### Safety (`spec.safety`)

| Field | Type | Description | Allowed values / default | Status |
| --- | --- | --- | --- | --- |
| `spec.safety.retryAttempts` | int | Retry attempts for transient errors. | `>= 0` | Roadmap |
| `spec.safety.backoffSeconds` | int | Backoff between retries. | `>= 0` | Roadmap |
| `spec.safety.pauseOnError` | bool | Pause further rotations after failures. | `true` \| `false` | Roadmap |
| `spec.safety.maxFailures` | int | Max failures before circuit-break. | `>= 0` | Roadmap |

### Secrets reference (bootstrap)

| Secret purpose | Referenced by | Required keys (stringData) | Notes |
| --- | --- | --- | --- |
| SSH bootstrap credentials | `spec.method.auth.bootstrapSecretRef` | `username` (recommended); `privateKey` (recommended for `rotation.kind=ssh-key`); `password` (**required** for `rotation.kind=linux-password`) | Files mounted as `/bootstrap/username`, `/bootstrap/password`, `/bootstrap/privateKey`. |
| WinRM bootstrap credentials | `spec.method.auth.bootstrapSecretRef` | `password` (required); `username` (recommended) | Files mounted as `/bootstrap/username`, `/bootstrap/password`. |

## How to use

### 1) Prepare bootstrap Secrets

Linux (SSH bootstrap):

```bash
kubectl apply -f examples/secrets/bootstrap-ssh.yaml
```

Linux (SSH bootstrap for `rotation.kind=linux-password`):

```bash
kubectl apply -f examples/secrets/bootstrap-ssh-password.yaml
```

Windows (WinRM bootstrap):

```bash
kubectl apply -f examples/secrets/bootstrap-win.yaml
```

### 2) Create a policy

Linux SSH key rotation (generated keypair, `authorized_keys` replace):

```bash
kubectl apply -f examples/linux-ssh-gen.yaml
```

Linux password rotation (generated password):

```bash
kubectl apply -f examples/linux-password-gen.yaml
```

Windows password rotation (generated password):

```bash
kubectl apply -f examples/windows-password-gen.yaml
```

### 3) Trigger a run

Policies can run on schedule, and can also be triggered manually via annotation:

```bash
kubectl apply -f examples/rotate-now-annotation-patch.yaml
```

### 4) Observe status and fetch the rotated credential output

Check policy status:

```bash
kubectl get acrp -n virt-ops <policy-name> -o yaml
```

Find Jobs created by a policy:

```bash
kubectl get jobs -n virt-ops -l guestops.io/policy=<policy-name>
```

The executor prints a final JSON line to stdout. You can read the logs from the Job pod.

If `spec.publish.mode: Always` (default), the operator will also publish the rotated credentials to a Kubernetes Secret in the policy namespace. This is essential for continuous rotation: subsequent runs use the publish Secret as bootstrap credentials instead of the original `bootstrapSecretRef`.

Default publish Secret name:

- `<policy-name>-publish` (when `spec.publish.secretName` is omitted)

Published Secret data format (per-VM keys):

- `VMNAME.username`
- `VMNAME.password` (for `linux-password` and `windows-password`)
- `VMNAME.privateKey` and `VMNAME.publicKey` (for `ssh-key`)

Bootstrap behavior:

- On subsequent runs, if the publish Secret contains the required keys for a VM, the operator will use that Secret as bootstrap credentials instead of `spec.method.auth.bootstrapSecretRef`.

- Linux SSH executor outputs:
  - `newPublicKey`
  - `newPrivateKeyB64` (only when `source: generate`)
  - `newPasswordB64` (only when `rotation.kind=linux-password`)

- Windows WinRM executor outputs:
  - `newPasswordB64`

Example (Linux, generated keypair):

```bash
kubectl logs -n virt-ops job/<job-name> -c executor | tail -n 1
```

Example (extract private key b64):

```bash
kubectl logs -n virt-ops job/<job-name> -c executor | tail -n 1 | jq -r '.newPrivateKeyB64'
```

Security note: base64 output is still sensitive. Treat logs as secrets.

RBAC note: publishing requires the operator ServiceAccount to be able to read pod logs (`get` on `pods/log`) in target namespaces.

Secondary networks (Multus) are configured via `spec.targets.networkAttachments` / `spec.targets.networkSelection` (see the spec reference table above).

## Vault-driven mode (automatic ESO integration)

When `spec.rotation.source: external` and `spec.rotation.vault` is configured, the controller fully automates the ESO integration:

1. **Creates an ExternalSecret** in the policy namespace, referencing a `ClusterSecretStore` (created manually in the operator namespace for security).
2. **Polls the synced Secret** every 15 seconds for the `reconcile.external-secrets.io/data-hash` annotation.
3. **Triggers rotation automatically** when the data-hash changes (i.e. Vault updated the credential and ESO synced it).
4. **Cron schedule is ignored** — rotation is driven entirely by Vault changes.

This means: update the credential in Vault → ESO syncs → controller detects data-hash change → rotation runs automatically. No cron, no manual annotation needed.

### Prerequisites

- External Secrets Operator installed in the cluster
- A `ClusterSecretStore` created manually (contains the Vault token, protected by RBAC)

### Vault config fields

| Field | Description |
|-------|-------------|
| `vault.secretStoreRef` | Name of the `ClusterSecretStore` to reference |
| `vault.secretPath` | Path in Vault (e.g. `ssh/admin`) |
| `vault.property` | Field name in the Vault secret (e.g. `private_key`). Optional. |

### Example: vault-driven SSH key rotation

Create a `ClusterSecretStore` manually (one-time):

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

Then create the ACRP — no schedule, no externalSecretRef:

```bash
kubectl apply -f examples/linux-ssh-vault.yaml
```

When the credential in Vault is updated, the rotation runs automatically within ~15 seconds.

### Manual external mode

When `spec.rotation.source: external` and `spec.rotation.vault` is **not** set, the controller expects the Secret to exist already (created manually or by a separate ExternalSecret). The Secret name is specified in `spec.rotation.externalSecretRef`. Cron schedule and manual annotation work as usual.

Examples:

```bash
# Linux SSH key (external Secret)
kubectl apply -f examples/linux-ssh-external.yaml

# Linux password (external Secret)
kubectl apply -f examples/linux-password-external.yaml

# Windows password (external Secret)
kubectl apply -f examples/windows-password-external.yaml
```

## Roadmap / Enhancements

The CRD includes additional fields that are not fully implemented yet. Planned enhancements include:

- `spec.rotation.overlapSeconds` for SSH key rotation (coexistence window + cleanup/final replace).
- `spec.safety` retry/backoff/circuit-breaker behaviors.
- SSH troubleshooting verbosity flag.
