package controller

import (
	"context"
	"errors"
	"fmt"
	api "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksm "github.com/fjsxy/monitor-in-hand/internal/kubestatemetrics"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"time"
)

var serviceMonitorGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}

const monitorRetryInterval = 5 * time.Second

// monitorAPI 精确访问可选 API，不创建缺 CRD 时会阻塞 manager 启动的 informer。
func (r *KubeStateMetricsReconciler) monitorAPI(instance *api.KubeStateMetrics) dynamic.ResourceInterface {
	ns := instance.Status.AppliedTargetNamespace
	if ns == "" {
		ns = instance.Spec.TargetNamespace
	}
	return r.DynamicClient.Resource(serviceMonitorGVR).Namespace(ns)
}

func monitorReference(obj *unstructured.Unstructured) *api.ServiceMonitorReference {
	return &api.ServiceMonitorReference{APIVersion: "monitoring.coreos.com/v1", Kind: "ServiceMonitor", Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID())}
}

// observeServiceMonitor 只更新配套资源观察，workload Ready/Available 仍由原逻辑计算。
func (r *KubeStateMetricsReconciler) observeServiceMonitor(ctx context.Context, instance *api.KubeStateMetrics, desired *unstructured.Unstructured) error {
	if r.DynamicClient == nil && desired == nil && (instance.Status.ServiceMonitor == nil || instance.Status.ServiceMonitor.Resource == nil) {
		return nil
	}
	before := instance.Status.DeepCopy()
	observation := &api.ServiceMonitorObservation{ObservedGeneration: instance.Generation, State: "Disabled", Reason: "Disabled", Message: "未启用配套采集"}
	if desired != nil || (instance.Status.ServiceMonitor != nil && instance.Status.ServiceMonitor.Resource != nil) {
		observation.State = "Ready"
		observation.Reason = "ReconcileSucceeded"
		observation.Message = "配套配置已收敛；不代表实际采集成功"
		// 关键：首次子资源写入前保存意图身份，避免 create 响应丢失或进程重启后关闭采集漏清理。
		if desired != nil && (instance.Status.ServiceMonitor == nil || instance.Status.ServiceMonitor.Resource == nil) {
			instance.Status.ServiceMonitor = &api.ServiceMonitorObservation{ObservedGeneration: instance.Generation, State: "Unknown", Reason: "Reconciling", Message: "配套写入前固定目标身份", Resource: monitorReference(desired)}
			setCondition(&instance.Status, instance.Generation, "ServiceMonitorReady", metav1.ConditionUnknown, "Reconciling", "配套配置尚待核验")
			if err := r.writeStatusIfChanged(ctx, instance, *before); err != nil {
				return err
			}
		}
		obj, err := r.syncServiceMonitor(ctx, instance, desired)
		if obj != nil {
			observation.Resource = monitorReference(obj)
		}
		if err != nil {
			// 关键：读取失败不是删除证据，保留已记录身份才能在关闭采集后继续重试清理。
			if previous := instance.Status.ServiceMonitor; previous != nil && previous.Resource != nil && (obj == nil || obj.GetUID() == "") {
				reference := *previous.Resource
				observation.Resource = &reference
			}
			observation.State = "Degraded"
			observation.Reason = "ApplyFailed"
			observation.Message = err.Error()
			if apierrors.IsNotFound(err) {
				observation.Reason = "DependencyMissing"
			}
			if apierrors.IsForbidden(err) {
				observation.Reason = "PermissionDenied"
			}
			var conflict *ownershipConflictError
			if errors.As(err, &conflict) {
				observation.Reason = "ResourceConflict"
			}
		} else if desired == nil {
			observation = monitorCleanupObservation(instance, obj)
		}
	}
	instance.Status.ServiceMonitor = observation
	status := metav1.ConditionTrue
	if observation.State == "Degraded" || observation.State == "Deleting" {
		status = metav1.ConditionFalse
	}
	setCondition(&instance.Status, instance.Generation, "ServiceMonitorReady", status, observation.Reason, observation.Message)
	return r.writeStatusIfChanged(ctx, instance, *before)
}

// monitorCleanupObservation 统一关闭采集和 finalizer 的删除完成判定。
func monitorCleanupObservation(instance *api.KubeStateMetrics, pending *unstructured.Unstructured) *api.ServiceMonitorObservation {
	observation := &api.ServiceMonitorObservation{ObservedGeneration: instance.Generation, State: "Disabled", Reason: "Disabled", Message: "配套配置已清理"}
	if pending != nil {
		observation.State = "Deleting"
		observation.Reason = "WaitingForDeletion"
		observation.Message = "等待原配套对象消失"
		observation.Resource = monitorReference(pending)
	}
	return observation
}

// syncServiceMonitor 只修改 owner 精确匹配的对象；每次写入均携带实际 UID/RV。
func (r *KubeStateMetricsReconciler) syncServiceMonitor(ctx context.Context, instance *api.KubeStateMetrics, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if r.DynamicClient == nil {
		return desired, fmt.Errorf("配套 API client 不可用")
	}
	endpoint := r.monitorAPI(instance)
	current, err := endpoint.Get(ctx, ksm.ManagedResourceName(instance.Name), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		if desired == nil {
			return nil, nil
		}
		// 关键：create 响应丢失时下一轮按 controller owner 核验，不能凭 labels 接管同名对象。
		created, err := endpoint.Create(ctx, desired, metav1.CreateOptions{})
		if err != nil {
			return desired, err
		}
		return created, nil
	}
	if err != nil {
		return desired, err
	}
	if !managedByInstance(current, instance) {
		return nil, &ownershipConflictError{message: "同名 ServiceMonitor 不由当前 KubeStateMetrics UID 管理"}
	}
	if desired == nil {
		if current.GetDeletionTimestamp().IsZero() {
			uid, rv := current.GetUID(), current.GetResourceVersion()
			if err := endpoint.Delete(ctx, current.GetName(), metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
				return current, err
			}
		}
		return current, nil
	}
	if !current.GetDeletionTimestamp().IsZero() {
		return current, fmt.Errorf("配套 ServiceMonitor 正在删除，等待原身份消失")
	}
	if !apiequality.Semantic.DeepEqual(current.Object["spec"], desired.Object["spec"]) || !apiequality.Semantic.DeepEqual(current.GetLabels(), desired.GetLabels()) {
		current.Object["spec"] = desired.DeepCopy().Object["spec"]
		current.SetLabels(desired.GetLabels())
		updated, err := endpoint.Update(ctx, current, metav1.UpdateOptions{})
		if err != nil {
			return current, err
		}
		return updated, nil
	}
	return current, nil
}

// deleteCompanion 参与既有 finalizer，任何读取/删除失败都不能假报完整清理。
func (r *KubeStateMetricsReconciler) deleteCompanion(ctx context.Context, instance *api.KubeStateMetrics) ([]api.ManagedResourceReference, error) {
	// 从未声明或发送配套写入的纯 exporter，不因可选 API 读取权限缺失而阻塞清理。
	requested := instance.Spec.ServiceMonitor != nil && instance.Spec.ServiceMonitor.Enabled
	recorded := instance.Status.ServiceMonitor != nil && instance.Status.ServiceMonitor.Resource != nil
	if !requested && !recorded {
		return nil, nil
	}
	if r.DynamicClient == nil {
		return nil, fmt.Errorf("配套 API client 不可用，无法确认清理完成")
	}
	obj, err := r.syncServiceMonitor(ctx, instance, nil)
	var conflict *ownershipConflictError
	if errors.As(err, &conflict) {
		obj = nil
		err = nil
	}
	if err == nil {
		observation := monitorCleanupObservation(instance, obj)
		instance.Status.ServiceMonitor = observation
		condition := metav1.ConditionTrue
		if obj != nil {
			condition = metav1.ConditionFalse
		}
		setCondition(&instance.Status, instance.Generation, "ServiceMonitorReady", condition, observation.Reason, observation.Message)
	}
	if obj == nil {
		return nil, err
	}
	return []api.ManagedResourceReference{{APIVersion: "monitoring.coreos.com/v1", Kind: "ServiceMonitor", Name: obj.GetName(), Namespace: obj.GetNamespace()}}, err
}
