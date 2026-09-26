package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksm "github.com/fjsxy/monitor-in-hand/internal/kubestatemetrics"
)

const dependencyRetryInterval = 30 * time.Second

// ServerVersioner is the discovery capability required by KubeStateMetrics reconciliation.
type ServerVersioner interface {
	ServerVersion() (*version.Info, error)
}

// KubeStateMetricsReconciler reconciles one cluster-scoped KubeStateMetrics declaration.
type KubeStateMetricsReconciler struct {
	client.Client
	ServerVersion ServerVersioner
}

// +kubebuilder:rbac:groups=monitoring.fjsxy.github.io,resources=kubestatemetrics,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=monitoring.fjsxy.github.io,resources=kubestatemetrics/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch

func (r *KubeStateMetricsReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	instance := &monitoringv1alpha1.KubeStateMetrics{}
	if err := r.Get(ctx, request.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	serverVersion, err := r.ServerVersion.ServerVersion()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("读取 Kubernetes 版本: %w", err)
	}
	namespace := &corev1.Namespace{}
	namespaceExists := true
	if err := r.Get(ctx, client.ObjectKey{Name: instance.Spec.TargetNamespace}, namespace); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("读取目标 Namespace %q: %w", instance.Spec.TargetNamespace, err)
		}
		namespaceExists = false
	}

	plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{
		KubernetesVersion:     serverVersion.GitVersion,
		TargetNamespaceExists: namespaceExists,
	})
	if problem != nil {
		if err := r.updateProblemStatus(ctx, instance, problem); err != nil {
			return ctrl.Result{}, err
		}
		if problem.Reason == ksm.ReasonDependencyMissing {
			return ctrl.Result{RequeueAfter: dependencyRetryInterval}, nil
		}
		return ctrl.Result{}, nil
	}
	if conflict, err := r.findOwnershipConflict(ctx, plan.Resources); err != nil {
		return ctrl.Result{}, err
	} else if conflict != nil {
		if err := r.updateProblemStatus(ctx, instance, conflict); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: dependencyRetryInterval}, nil
	}

	for _, desired := range plan.Resources {
		if err := r.apply(ctx, desired); err != nil {
			reason := ksm.ReasonApplyFailed
			var conflict *ownershipConflictError
			if errors.As(err, &conflict) {
				reason = ksm.ReasonResourceConflict
			}
			applyProblem := &ksm.Problem{Reason: reason, Message: fmt.Sprintf("写入 %T %q: %v", desired, desired.GetName(), err)}
			if statusErr := r.updateProblemStatus(ctx, instance, applyProblem); statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("写入 %T %q 失败: %v；并且更新失败 status: %w", desired, desired.GetName(), err, statusErr)
			}
			return ctrl.Result{}, fmt.Errorf("写入 %T %q: %w", desired, desired.GetName(), err)
		}
	}

	deployment := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: instance.Spec.TargetNamespace, Name: plan.Name}, deployment); err != nil {
		return ctrl.Result{}, fmt.Errorf("读取已应用 Deployment: %w", err)
	}
	if err := r.updateSuccessStatus(ctx, instance, plan, deployment.Status.AvailableReplicas); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

type ownershipConflictError struct{ message string }

func (e *ownershipConflictError) Error() string { return e.message }

func (r *KubeStateMetricsReconciler) findOwnershipConflict(ctx context.Context, resources []client.Object) (*ksm.Problem, error) {
	for _, desired := range resources {
		current, err := emptyObjectFor(desired)
		if err != nil {
			return nil, err
		}
		key := client.ObjectKeyFromObject(desired)
		if err := r.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("检查受管资源 %T %q 所有权: %w", desired, key.String(), err)
		}
		if !hasManagingOwner(current, desired.GetOwnerReferences()[0]) {
			return &ksm.Problem{
				Reason:  ksm.ReasonResourceConflict,
				Message: fmt.Sprintf("%s %q 已存在但不由当前 KubeStateMetrics UID 管理", managedResourceReference(desired).Kind, key.String()),
			}, nil
		}
	}
	return nil, nil
}

func (r *KubeStateMetricsReconciler) apply(ctx context.Context, desired client.Object) error {
	current, err := emptyObjectFor(desired)
	if err != nil {
		return err
	}
	current.SetName(desired.GetName())
	current.SetNamespace(desired.GetNamespace())
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, current, func() error {
		if current.GetUID() != "" && !hasManagingOwner(current, desired.GetOwnerReferences()[0]) {
			return &ownershipConflictError{message: "同名资源在预检后出现且不由当前 KubeStateMetrics UID 管理"}
		}
		current.SetLabels(copyStringMap(desired.GetLabels()))
		current.SetOwnerReferences(append([]metav1.OwnerReference(nil), desired.GetOwnerReferences()...))
		switch actual := current.(type) {
		case *corev1.ServiceAccount:
			actual.AutomountServiceAccountToken = desired.(*corev1.ServiceAccount).AutomountServiceAccountToken
		case *rbacv1.ClusterRole:
			actual.Rules = desired.(*rbacv1.ClusterRole).Rules
		case *rbacv1.ClusterRoleBinding:
			actual.RoleRef = desired.(*rbacv1.ClusterRoleBinding).RoleRef
			actual.Subjects = desired.(*rbacv1.ClusterRoleBinding).Subjects
		case *corev1.Service:
			wanted := desired.(*corev1.Service)
			if actual.Spec.ClusterIP != "" && actual.Spec.ClusterIP != corev1.ClusterIPNone {
				return fmt.Errorf("已拥有 Service 的 immutable clusterIP %q 不是 headless", actual.Spec.ClusterIP)
			}
			actual.Spec.Selector = copyStringMap(wanted.Spec.Selector)
			actual.Spec.Ports = wanted.Spec.Ports
			if actual.Spec.ClusterIP == "" {
				actual.Spec.ClusterIP = wanted.Spec.ClusterIP
				actual.Spec.ClusterIPs = append([]string(nil), wanted.Spec.ClusterIPs...)
			}
		case *appsv1.Deployment:
			wanted := desired.(*appsv1.Deployment)
			actual.Spec.Replicas = wanted.Spec.Replicas
			actual.Spec.Selector = wanted.Spec.Selector.DeepCopy()
			actual.Spec.Template.Labels = copyStringMap(wanted.Spec.Template.Labels)
			actual.Spec.Template.Spec.ServiceAccountName = wanted.Spec.Template.Spec.ServiceAccountName
			actual.Spec.Template.Spec.AutomountServiceAccountToken = wanted.Spec.Template.Spec.AutomountServiceAccountToken
			actual.Spec.Template.Spec.NodeSelector = copyStringMap(wanted.Spec.Template.Spec.NodeSelector)
			actual.Spec.Template.Spec.Containers = make([]corev1.Container, len(wanted.Spec.Template.Spec.Containers))
			for index := range wanted.Spec.Template.Spec.Containers {
				wanted.Spec.Template.Spec.Containers[index].DeepCopyInto(&actual.Spec.Template.Spec.Containers[index])
			}
		default:
			return fmt.Errorf("不支持的受管资源类型 %T", current)
		}
		return nil
	})
	return err
}

func emptyObjectFor(object client.Object) (client.Object, error) {
	current, ok := reflect.New(reflect.TypeOf(object).Elem()).Interface().(client.Object)
	if !ok {
		return nil, fmt.Errorf("不支持的受管资源类型 %T", object)
	}
	return current, nil
}

func hasManagingOwner(resource client.Object, expected metav1.OwnerReference) bool {
	for _, owner := range resource.GetOwnerReferences() {
		if owner.APIVersion == expected.APIVersion && owner.Kind == expected.Kind && owner.Name == expected.Name && owner.UID == expected.UID && owner.Controller != nil && *owner.Controller {
			return true
		}
	}
	return false
}

func (r *KubeStateMetricsReconciler) updateProblemStatus(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, problem *ksm.Problem) error {
	before := instance.Status.DeepCopy()
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.Phase = monitoringv1alpha1.KubeStateMetricsPhaseDegraded
	setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, problem.Reason, problem.Message)
	setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionFalse, problem.Reason, problem.Message)
	setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionTrue, problem.Reason, problem.Message)
	if instance.Status.AppliedTargetNamespace == "" {
		instance.Status.ManagedResources = nil
		instance.Status.KubeStateMetricsVersion = ""
		instance.Status.DeploymentMode = ""
		instance.Status.DesiredPods = 0
		instance.Status.AvailablePods = 0
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, problem.Reason, problem.Message)
	} else if instance.Status.DesiredPods > 0 && instance.Status.AvailablePods >= instance.Status.DesiredPods {
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionTrue, "MinimumReplicasAvailable", "已应用的受管 workload 仍然可用")
	} else {
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, "MinimumReplicasUnavailable", "已应用的受管 workload 当前不可用")
	}
	return r.writeStatusIfChanged(ctx, instance, *before)
}

func (r *KubeStateMetricsReconciler) updateSuccessStatus(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, plan *ksm.ResourcePlan, availablePods int32) error {
	before := instance.Status.DeepCopy()
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.AppliedTargetNamespace = instance.Spec.TargetNamespace
	instance.Status.KubeStateMetricsVersion = plan.Profile.Version
	instance.Status.DeploymentMode = monitoringv1alpha1.DeploymentModeDeployment
	instance.Status.DesiredPods = 1
	instance.Status.AvailablePods = availablePods
	instance.Status.ManagedResources = managedResourceReferences(plan.Resources)
	if availablePods >= 1 {
		instance.Status.Phase = monitoringv1alpha1.KubeStateMetricsPhaseReady
		setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionTrue, "ReconcileSucceeded", "受管实例已就绪")
		setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionFalse, "ReconcileSucceeded", "无需继续推进")
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionTrue, "MinimumReplicasAvailable", "单副本 Deployment 可用")
	} else {
		instance.Status.Phase = monitoringv1alpha1.KubeStateMetricsPhaseReconciling
		setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, "DeploymentProgressing", "等待单副本 Deployment 可用")
		setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionTrue, "DeploymentProgressing", "等待单副本 Deployment 可用")
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, "MinimumReplicasUnavailable", "单副本 Deployment 尚不可用")
	}
	setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionFalse, "ReconcileSucceeded", "未发现调谐故障")
	return r.writeStatusIfChanged(ctx, instance, *before)
}

func (r *KubeStateMetricsReconciler) writeStatusIfChanged(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, before monitoringv1alpha1.KubeStateMetricsStatus) error {
	if apiequality.Semantic.DeepEqual(before, instance.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, instance); err != nil {
		return fmt.Errorf("更新 KubeStateMetrics status: %w", err)
	}
	return nil
}

func setCondition(status *monitoringv1alpha1.KubeStateMetricsStatus, generation int64, conditionType string, conditionStatus metav1.ConditionStatus, reason, message string) {
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             conditionStatus,
		ObservedGeneration: generation,
		Reason:             reason,
		Message:            message,
	})
}

func managedResourceReferences(resources []client.Object) []monitoringv1alpha1.ManagedResourceReference {
	result := make([]monitoringv1alpha1.ManagedResourceReference, 0, len(resources))
	for _, resource := range resources {
		result = append(result, managedResourceReference(resource))
	}
	return result
}

func managedResourceReference(resource client.Object) monitoringv1alpha1.ManagedResourceReference {
	reference := monitoringv1alpha1.ManagedResourceReference{Name: resource.GetName(), Namespace: resource.GetNamespace()}
	switch resource.(type) {
	case *corev1.ServiceAccount:
		reference.APIVersion, reference.Kind = "v1", "ServiceAccount"
	case *corev1.Service:
		reference.APIVersion, reference.Kind = "v1", "Service"
	case *rbacv1.ClusterRole:
		reference.APIVersion, reference.Kind = rbacv1.SchemeGroupVersion.String(), "ClusterRole"
	case *rbacv1.ClusterRoleBinding:
		reference.APIVersion, reference.Kind = rbacv1.SchemeGroupVersion.String(), "ClusterRoleBinding"
	case *appsv1.Deployment:
		reference.APIVersion, reference.Kind = appsv1.SchemeGroupVersion.String(), "Deployment"
	}
	return reference
}

func copyStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (r *KubeStateMetricsReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&monitoringv1alpha1.KubeStateMetrics{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.ClusterRole{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Complete(r)
}
