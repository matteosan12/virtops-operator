// Package v1alpha1 contains the API type definitions for guestops.io/v1alpha1.
// These files follow the Kubebuilder/Operator SDK style to keep code generation straightforward.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion identifies the API group and version (guestops.io/v1alpha1).
	GroupVersion = schema.GroupVersion{Group: "guestops.io", Version: "v1alpha1"}

	// SchemeBuilder is used to register types into a Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme registers the types into an external Scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
