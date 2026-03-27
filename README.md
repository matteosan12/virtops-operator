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
| `spec.rotation.kind` | string | What is rotated. | `ssh-key` \| `windows-password` ( `linux-password` is not supported yet) | Partially implemented |
| `spec.rotation.source` | string | Where the new credential comes from. | `generate` \| `provided` (SSH only) \| `external` (roadmap). WinRM: `generate` only. | Partially implemented |
| `spec.rotation.providedSecretRef` | string | Secret holding the provided public key (only when `source=provided`). | Required when `source=provided` | Implemented (SSH only) |
| `spec.rotation.externalSecretRef` | string | External Secret reference used when `source=external`. | Free-form string | Roadmap |
| `spec.rotation.authorizedKeysMode` | string | SSH authorized_keys update strategy. | `replace` (default) \| `append` | Implemented (SSH only) |
| `spec.rotation.length` | int | Length of generated password. | Default: executor uses `24` (min enforced by executor). | Implemented (WinRM only) |
| `spec.rotation.overlapSeconds` | int | Coexistence window for old/new credentials. | `>= 0` | Roadmap |

### Publish (`spec.publish`)

| Field | Type | Description | Allowed values / default | Status |
| --- | --- | --- | --- | --- |
| `spec.publish.mode` | string | Whether and when to publish rotated credentials to a Kubernetes Secret. | `Always` \| `TestingOnly` \| `Never` | Roadmap |
| `spec.publish.secretName` | string | Target Secret name to create/update when publishing. | string | Roadmap |

### Safety (`spec.safety`)

| Field | Type | Description | Allowed values / default | Status |
| --- | --- | --- | --- | --- |
| `spec.safety.retryAttempts` | int | Retry attempts for transient errors. | `>= 0` | Roadmap |
| `spec.safety.backoffSeconds` | int | Backoff between retries. | `>= 0` | Roadmap |
| `spec.safety.pauseOnError` | bool | Pause further rotations after failures. | `true` \| `false` | Roadmap |
| `spec.safety.maxFailures` | int | Max failures before circuit-break. | `>= 0` | Roadmap |

### Secrets reference (bootstrap and provided)

| Secret purpose | Referenced by | Required keys (stringData) | Notes |
| --- | --- | --- | --- |
| SSH bootstrap credentials | `spec.method.auth.bootstrapSecretRef` | `privateKey` (recommended) or `password`; `username` (recommended) | Files mounted as `/bootstrap/username`, `/bootstrap/password`, `/bootstrap/privateKey`. |
| WinRM bootstrap credentials | `spec.method.auth.bootstrapSecretRef` | `password` (required); `username` (recommended) | Files mounted as `/bootstrap/username`, `/bootstrap/password`. |
| Provided SSH public key | `spec.rotation.providedSecretRef` | `publicKey` (required) | File mounted as `/provided/publicKey`. |

## How to use

### 1) Prepare bootstrap Secrets

Linux (SSH bootstrap):

```bash
kubectl apply -f examples/secrets/bootstrap-ssh.yaml
```

Windows (WinRM bootstrap):

```bash
kubectl apply -f examples/secrets/bootstrap-win.yaml
```

For `source: provided` (SSH public key):

```bash
kubectl apply -f examples/secrets/provided-publickey.yaml
```

### 2) Create a policy

Linux SSH key rotation (generated keypair, `authorized_keys` replace):

```bash
kubectl apply -f examples/linux-ssh-gen.yaml
```

Linux SSH key rotation (install provided public key, append mode):

```bash
kubectl apply -f examples/linux-ssh-provided.yaml
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

- Linux SSH executor outputs:
  - `newPublicKey`
  - `newPrivateKeyB64` (only when `source: generate`)

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

Secondary networks (Multus) are configured via `spec.targets.networkAttachments` / `spec.targets.networkSelection` (see the spec reference table above).

## Roadmap / Enhancements

The CRD includes additional fields that are not fully implemented yet. Planned enhancements include:

- `spec.rotation.overlapSeconds` for SSH key rotation (coexistence window + cleanup/final replace).
- `spec.publish` to publish rotated credentials to a Kubernetes Secret (opt-in, safe defaults).
- `spec.safety` retry/backoff/circuit-breaker behaviors.
- SSH troubleshooting verbosity flag.
