package network

// SelectFromVMI inspects a VMI object in unstructured form to choose a network and
// an IP to use based on the provided preferences.
// Note: this is best-effort and does not perform reachability checks.
func SelectFromVMI(vmi map[string]interface{}, in SelectionInput) SelectionResult {
	res := SelectionResult{}

	spec, _ := vmi["spec"].(map[string]interface{})
	status, _ := vmi["status"].(map[string]interface{})
	networks, _ := spec["networks"].([]interface{})
	interfaces, _ := status["interfaces"].([]interface{})

	// Map NADs present in spec
	presentNAD := map[string]bool{}
	for _, n := range networks {
		if m, ok := n.(map[string]interface{}); ok {
			if multus, ok := m["multus"].(map[string]interface{}); ok {
				if nn, ok := multus["networkName"].(string); ok && nn != "" {
					presentNAD[nn] = true
				}
			}
		}
	}

	// Helper: first available IP
	firstIP := func() string {
		for _, it := range interfaces {
			if mm, ok := it.(map[string]interface{}); ok {
				if ip, ok := mm["ipAddress"].(string); ok && ip != "" {
					return ip
				}
			}
		}
		return ""
	}

	switch in.Mode {
	case ModePodOnly:
		res.IP = firstIP()
		return res
	case ModeNadList:
		for _, name := range in.NadList {
			if presentNAD[name] {
				res.NetworkName = name
				res.NetworksAnnotation = name
				res.IP = firstIP()
				return res
			}
		}
		res.IP = firstIP()
		return res
	case ModeAuto:
		if in.PreferPod {
			if ip := firstIP(); ip != "" {
				res.IP = ip
				return res
			}
		}
		for _, name := range in.NadList { // user preferences
			if presentNAD[name] {
				res.NetworkName = name
				res.NetworksAnnotation = name
				res.IP = firstIP()
				return res
			}
		}
		// First available NAD
		for name := range presentNAD {
			res.NetworkName = name
			res.NetworksAnnotation = name
			res.IP = firstIP()
			return res
		}
		// Fallback: pod network
		res.IP = firstIP()
		return res
	default:
		res.IP = firstIP()
		return res
	}
}
