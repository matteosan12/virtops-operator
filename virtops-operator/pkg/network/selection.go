package network

// Package network provides helpers to dynamically select which network to use
// (pod network or a Multus NetworkAttachmentDefinition) to reach the VM.
// In a future release this package may include:
// - Reading VMI (status.interfaces) and VM (spec.networks) to discover addresses and interfaces.
// - Policies: auto/podOnly/nadList with preferences.
// - Reachability checks (SSH/WinRM probes) and best-path selection.
// - Building the "k8s.v1.cni.cncf.io/networks" annotation for ephemeral Jobs when a NAD is needed.

// Placeholder for public APIs used by the controller.

type Mode string

const (
	ModeAuto    Mode = "auto"
	ModePodOnly Mode = "podOnly"
	ModeNadList Mode = "nadList"
)

type SelectionInput struct {
	Mode      Mode
	PreferPod bool
	NadList   []string
}

type SelectionResult struct {
	// Selected network name. Empty means pod network.
	NetworkName string
	// Annotation value to apply to the Job to activate the NAD (when not empty).
	NetworksAnnotation string
	// Chosen IP for the connection (if available at selection time).
	IP string
}

// Select determines the network to use (placeholder; currently returns the pod network).
func Select(in SelectionInput) SelectionResult {
	return SelectionResult{}
}
