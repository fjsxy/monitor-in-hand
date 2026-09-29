package kubestatemetrics

import (
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
)

const (
	ReasonDeleteFailed       = "DeleteFailed"
	ReasonDeleting           = "Deleting"
	ReasonDisabled           = "Disabled"
	ReasonDisabling          = "Disabling"
	ReasonRecovered          = "Recovered"
	ReasonSuspended          = "Suspended"
	ReasonSuspending         = "Suspending"
	ReasonWaitingForDeletion = "WaitingForDeletion"
)

// DerivePhase 从删除状态与 conditions 推导摘要，按删除、停用、故障、推进、就绪的业务优先级避免第二份可变状态源。
func DerivePhase(instance *monitoringv1alpha1.KubeStateMetrics) monitoringv1alpha1.KubeStateMetricsPhase {
	if !instance.DeletionTimestamp.IsZero() {
		return monitoringv1alpha1.KubeStateMetricsPhaseDeleting
	}
	progressing := apiMeta.FindStatusCondition(instance.Status.Conditions, "Progressing")
	if progressing != nil && progressing.Status == metav1.ConditionTrue && progressing.Reason == ReasonDisabling {
		return monitoringv1alpha1.KubeStateMetricsPhaseDisabling
	}
	ready := apiMeta.FindStatusCondition(instance.Status.Conditions, "Ready")
	if ready != nil && ready.Reason == ReasonDisabled {
		return monitoringv1alpha1.KubeStateMetricsPhaseDisabled
	}
	if conditionIsTrue(instance.Status.Conditions, "Degraded") {
		return monitoringv1alpha1.KubeStateMetricsPhaseDegraded
	}
	if conditionIsTrue(instance.Status.Conditions, "Progressing") {
		return monitoringv1alpha1.KubeStateMetricsPhaseReconciling
	}
	if conditionIsTrue(instance.Status.Conditions, "Ready") {
		return monitoringv1alpha1.KubeStateMetricsPhaseReady
	}
	return monitoringv1alpha1.KubeStateMetricsPhasePending
}

func conditionIsTrue(conditions []metav1.Condition, conditionType string) bool {
	condition := apiMeta.FindStatusCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}
