package ssh

// Package ssh will implement the executor for guest operations via SSH (Linux):
// - SSH key rotation (add new key, validate access, remove old key after overlap).
// - Linux local password change when requested.
// - File copy and remote commands (phase 2, optional).
// The executor runs as ephemeral Kubernetes Jobs with minimal privileges.

// Placeholder: minimal definitions to avoid build errors.

type Options struct{}

func RotateKey(opts Options) error { return nil }
