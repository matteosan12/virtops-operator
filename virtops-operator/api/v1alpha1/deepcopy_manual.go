package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// DeepCopyInto performs a deep copy of this object into out.
func (in *AdminCredentialRotationPolicy) DeepCopyInto(out *AdminCredentialRotationPolicy) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	// Spec and Status are mostly simple types/pointers; a direct copy is sufficient for the MVP.
	out.Spec = in.Spec
	out.Status = in.Status
}

// DeepCopy creates a new instance by copying the contents of this object.
func (in *AdminCredentialRotationPolicy) DeepCopy() *AdminCredentialRotationPolicy {
	if in == nil {
		return nil
	}
	out := new(AdminCredentialRotationPolicy)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *AdminCredentialRotationPolicy) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto performs a deep copy for the list.
func (in *AdminCredentialRotationPolicyList) DeepCopyInto(out *AdminCredentialRotationPolicyList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AdminCredentialRotationPolicy, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy creates a new instance for the list.
func (in *AdminCredentialRotationPolicyList) DeepCopy() *AdminCredentialRotationPolicyList {
	if in == nil {
		return nil
	}
	out := new(AdminCredentialRotationPolicyList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object for the list.
func (in *AdminCredentialRotationPolicyList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
