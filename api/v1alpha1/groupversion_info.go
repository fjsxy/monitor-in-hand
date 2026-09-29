// Package v1alpha1 contains API Schema definitions for the monitoring v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=monitoring.fjsxy.github.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "monitoring.fjsxy.github.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
