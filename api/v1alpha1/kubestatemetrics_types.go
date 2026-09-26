package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// DeploymentMode identifies the topology used by a KubeStateMetrics instance.
type DeploymentMode string

const (
	DeploymentModeDeployment DeploymentMode = "Deployment"
)

// KubeStateMetricsDeploymentSpec selects the workload topology.
type KubeStateMetricsDeploymentSpec struct {
	// Mode is the selected workload topology.
	// +kubebuilder:default=Deployment
	// +kubebuilder:validation:Enum=Deployment
	// +optional
	Mode DeploymentMode `json:"mode,omitempty"`
}

// KubeStateMetricsSpec declares one managed kube-state-metrics instance.
type KubeStateMetricsSpec struct {
	// TargetNamespace must name an existing Namespace. The Operator never creates it.
	// +kubebuilder:validation:MinLength=1
	TargetNamespace string `json:"targetNamespace"`

	// Enabled defaults to true. Full disable lifecycle behavior is delivered separately.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Suspended defaults to false. Full suspend lifecycle behavior is delivered separately.
	// +kubebuilder:default=false
	// +optional
	Suspended *bool `json:"suspended,omitempty"`

	// Deployment selects the workload topology.
	// +kubebuilder:default={mode:Deployment}
	// +optional
	Deployment KubeStateMetricsDeploymentSpec `json:"deployment,omitempty"`
}

// ManagedResourceReference identifies a directly managed resource for discovery.
type ManagedResourceReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// KubeStateMetricsStatus reports the observed managed-instance state.
type KubeStateMetricsStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// +optional
	ManagedResources []ManagedResourceReference `json:"managedResources,omitempty"`
	// +optional
	AppliedTargetNamespace string `json:"appliedTargetNamespace,omitempty"`
	// +optional
	KubeStateMetricsVersion string `json:"kubeStateMetricsVersion,omitempty"`
	// +optional
	DeploymentMode DeploymentMode `json:"deploymentMode,omitempty"`
	// +optional
	DesiredPods int32 `json:"desiredPods,omitempty"`
	// +optional
	AvailablePods int32 `json:"availablePods,omitempty"`
}

// KubeStateMetrics is a cluster-scoped declaration of one managed kube-state-metrics instance.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=ksm
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.kubeStateMetricsVersion`
// +kubebuilder:printcolumn:name="Target Namespace",type=string,JSONPath=`.spec.targetNamespace`
type KubeStateMetrics struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   KubeStateMetricsSpec   `json:"spec,omitempty"`
	Status KubeStateMetricsStatus `json:"status,omitempty"`
}

// KubeStateMetricsList contains a list of KubeStateMetrics resources.
// +kubebuilder:object:root=true
type KubeStateMetricsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KubeStateMetrics `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KubeStateMetrics{}, &KubeStateMetricsList{})
}
