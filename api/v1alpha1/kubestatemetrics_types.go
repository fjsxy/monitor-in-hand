package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CollectorMode selects how a managed instance chooses Kubernetes collectors.
type CollectorMode string

const (
	CollectorModeDefault CollectorMode = "Default"
	CollectorModeInclude CollectorMode = "Include"
)

// KubeStateMetricsCollectors declares the stable collector set exposed by the Operator.
type KubeStateMetricsCollectors struct {
	// Mode selects the shared default catalog or an explicit subset.
	// +kubebuilder:default=Default
	// +kubebuilder:validation:Enum=Default;Include
	// +optional
	Mode CollectorMode `json:"mode,omitempty"`

	// Names contains collector names when mode is Include.
	// +optional
	Names []string `json:"names,omitempty"`
}

// MetricFilterMode selects which metric names kube-state-metrics exposes.
type MetricFilterMode string

const (
	MetricFilterModeAll   MetricFilterMode = "All"
	MetricFilterModeAllow MetricFilterMode = "Allow"
	MetricFilterModeDeny  MetricFilterMode = "Deny"
)

// KubeStateMetricsMetricFilter declares a typed metric-name filter.
type KubeStateMetricsMetricFilter struct {
	// Mode exposes all metrics or applies an allow/deny filter.
	// +kubebuilder:default=All
	// +kubebuilder:validation:Enum=All;Allow;Deny
	// +optional
	Mode MetricFilterMode `json:"mode,omitempty"`

	// Patterns contains Go regular expressions for Allow and Deny modes.
	// +optional
	Patterns []string `json:"patterns,omitempty"`
}

// DeploymentMode identifies the topology used by a KubeStateMetrics instance.
type DeploymentMode string

const (
	DeploymentModeDeployment         DeploymentMode = "Deployment"
	DeploymentModeStatefulSetSharded DeploymentMode = "StatefulSetSharded"
	DeploymentModeDaemonSetSharded   DeploymentMode = "DaemonSetSharded"
)

// NamespaceFilterMode identifies how an instance selects namespaces to collect.
type NamespaceFilterMode string

const (
	NamespaceFilterModeAll     NamespaceFilterMode = "All"
	NamespaceFilterModeInclude NamespaceFilterMode = "Include"
)

// NamespaceFilter selects the namespaces collected by every workload role.
type NamespaceFilter struct {
	// Mode defaults to All.
	// +kubebuilder:default=All
	// +kubebuilder:validation:Enum=All;Include
	Mode NamespaceFilterMode `json:"mode,omitempty"`

	// Names are literal namespace names included in Include mode.
	// +optional
	Names []string `json:"names,omitempty"`

	// NamePatterns are Go/RE2 expressions matched against complete namespace names.
	// +optional
	NamePatterns []string `json:"namePatterns,omitempty"`
}

// KubeStateMetricsPhase summarizes the reconciliation state of a managed instance.
type KubeStateMetricsPhase string

const (
	KubeStateMetricsPhasePending     KubeStateMetricsPhase = "Pending"
	KubeStateMetricsPhaseDeleting    KubeStateMetricsPhase = "Deleting"
	KubeStateMetricsPhaseDisabling   KubeStateMetricsPhase = "Disabling"
	KubeStateMetricsPhaseDisabled    KubeStateMetricsPhase = "Disabled"
	KubeStateMetricsPhaseReconciling KubeStateMetricsPhase = "Reconciling"
	KubeStateMetricsPhaseReady       KubeStateMetricsPhase = "Ready"
	KubeStateMetricsPhaseDegraded    KubeStateMetricsPhase = "Degraded"
)

// KubeStateMetricsStatefulSetShardedSpec configures the experimental sharded topology.
type KubeStateMetricsStatefulSetShardedSpec struct {
	// Shards is the number of automated kube-state-metrics shards and must be at least two.
	Shards int32 `json:"shards"`
}

// KubeStateMetricsDaemonSetShardedSpec configures the experimental per-node Pod sharding topology.
type KubeStateMetricsDaemonSetShardedSpec struct {
	// NodePodResources replaces spec.podResources for the node DaemonSet when present.
	// +optional
	NodePodResources *corev1.ResourceRequirements `json:"nodePodResources,omitempty"`

	// ClusterPodResources replaces spec.podResources for the cluster collectors Deployment when present.
	// +optional
	ClusterPodResources *corev1.ResourceRequirements `json:"clusterPodResources,omitempty"`

	// UnscheduledPodResources replaces spec.podResources for the unscheduled Pods Deployment when present.
	// +optional
	UnscheduledPodResources *corev1.ResourceRequirements `json:"unscheduledPodResources,omitempty"`
}

// KubeStateMetricsDeploymentSpec is a discriminated union selected by Mode.
type KubeStateMetricsDeploymentSpec struct {
	// Mode is the selected workload topology.
	// +kubebuilder:default=Deployment
	// +kubebuilder:validation:Enum=Deployment;StatefulSetSharded;DaemonSetSharded
	// +optional
	Mode DeploymentMode `json:"mode,omitempty"`

	// StatefulSetSharded configures the experimental StatefulSetSharded mode.
	// It must be present only when mode is StatefulSetSharded.
	// +optional
	StatefulSetSharded *KubeStateMetricsStatefulSetShardedSpec `json:"statefulSetSharded,omitempty"`

	// DaemonSetSharded configures the experimental DaemonSetSharded mode.
	// It must be present only when mode is DaemonSetSharded.
	// +optional
	DaemonSetSharded *KubeStateMetricsDaemonSetShardedSpec `json:"daemonSetSharded,omitempty"`
}

// KubeStateMetricsSpec declares one managed kube-state-metrics instance.
type KubeStateMetricsSpec struct {
	// TargetNamespace must name an existing Namespace. The Operator never creates it.
	// +kubebuilder:validation:MinLength=1
	TargetNamespace string `json:"targetNamespace"`

	// Enabled controls whether directly managed resources exist and defaults to true.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Suspended stops the workload while retaining its Service, ServiceAccount, and RBAC.
	// +kubebuilder:default=false
	// +optional
	Suspended *bool `json:"suspended,omitempty"`

	// NamespaceFilter selects the namespaces collected by this instance.
	// +kubebuilder:default={mode:All}
	// +optional
	NamespaceFilter NamespaceFilter `json:"namespaceFilter,omitempty"`

	// Deployment selects the workload topology.
	// +kubebuilder:default={mode:Deployment}
	// +optional
	Deployment KubeStateMetricsDeploymentSpec `json:"deployment,omitempty"`

	// Collectors controls the Kubernetes resources observed by kube-state-metrics.
	// +kubebuilder:default={mode:Default}
	// +optional
	Collectors KubeStateMetricsCollectors `json:"collectors,omitempty"`

	// MetricFilter controls which generated metrics are exposed.
	// +kubebuilder:default={mode:All}
	// +optional
	MetricFilter KubeStateMetricsMetricFilter `json:"metricFilter,omitempty"`

	// PodResources is passed unchanged to the kube-state-metrics container.
	// When omitted, the Operator does not inject requests or limits.
	// Claims are rejected because the managed Pod API does not expose resourceClaims.
	// +optional
	PodResources *corev1.ResourceRequirements `json:"podResources,omitempty"`
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
	// +kubebuilder:validation:Enum=Pending;Deleting;Disabling;Disabled;Reconciling;Ready;Degraded
	Phase KubeStateMetricsPhase `json:"phase,omitempty"`
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
	// EffectiveCollectors reports the collector set used by the applied workload.
	// +optional
	EffectiveCollectors []string `json:"effectiveCollectors,omitempty"`
	// +optional
	DesiredPods int32 `json:"desiredPods,omitempty"`
	// +optional
	AvailablePods int32 `json:"availablePods,omitempty"`
	// EffectiveNamespaceCount is the number of literal namespaces delivered to every workload role.
	// +optional
	EffectiveNamespaceCount int32 `json:"effectiveNamespaceCount,omitempty"`
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

	Spec   KubeStateMetricsSpec   `json:"spec"`
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
