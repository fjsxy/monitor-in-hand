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
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksm "github.com/fjsxy/monitor-in-hand/internal/kubestatemetrics"
)

const (
	dependencyRetryInterval   = 30 * time.Second
	cleanupRetryInterval      = 2 * time.Second
	kubeStateMetricsFinalizer = "monitoring.fjsxy.github.io/kubestatemetrics-cleanup"
)

// ServerVersioner is the discovery capability required by KubeStateMetrics reconciliation.
type ServerVersioner interface {
	ServerVersion() (*version.Info, error)
}

// KubeStateMetricsReconciler reconciles one cluster-scoped KubeStateMetrics declaration.
type KubeStateMetricsReconciler struct {
	client.Client
	ServerVersion ServerVersioner
	Recorder      record.EventRecorder
}

// +kubebuilder:rbac:groups=monitoring.fjsxy.github.io,resources=kubestatemetrics,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=monitoring.fjsxy.github.io,resources=kubestatemetrics/finalizers,verbs=update
// +kubebuilder:rbac:groups=monitoring.fjsxy.github.io,resources=kubestatemetrics/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;nodes;pods;resourcequotas;replicationcontrollers;limitranges;persistentvolumeclaims;persistentvolumes;endpoints,verbs=list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=statefulsets;daemonsets;replicasets,verbs=list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs;jobs,verbs=list;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=list;watch
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=list;watch
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests,verbs=list;watch
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses;volumeattachments,verbs=list;watch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations;validatingwebhookconfigurations,verbs=list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies;ingresses;ingressclasses,verbs=list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=list;watch

func (r *KubeStateMetricsReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	instance := &monitoringv1alpha1.KubeStateMetrics{}
	if err := r.Get(ctx, request.NamespacedName, instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !instance.DeletionTimestamp.IsZero() {
		return r.reconcileDeletion(ctx, instance)
	}
	if !controllerutil.ContainsFinalizer(instance, kubeStateMetricsFinalizer) {
		controllerutil.AddFinalizer(instance, kubeStateMetricsFinalizer)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, fmt.Errorf("添加 KubeStateMetrics finalizer: %w", err)
		}
	}
	if instance.Spec.Enabled != nil && !*instance.Spec.Enabled {
		return r.reconcileDisabled(ctx, instance)
	}

	// 重新启用不能与上一次停用清理交错；先持续删除旧资源，确认全部消失后再按最新 generation 规划。
	if cleanupWasInterrupted(instance) {
		resources, err := r.deleteManagedResources(ctx, instance)
		if err != nil {
			statusErr := r.updateCleanupStatus(ctx, instance, resources, ksm.ReasonWaitingForDeletion, err)
			if statusErr != nil {
				return ctrl.Result{}, fmt.Errorf("继续停用清理失败: %v；并且更新失败 status: %w", err, statusErr)
			}
			return ctrl.Result{}, err
		}
		if len(resources) > 0 {
			if err := r.updateCleanupStatus(ctx, instance, resources, ksm.ReasonWaitingForDeletion, nil); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: cleanupRetryInterval}, nil
		}
	}

	serverVersion, err := r.ServerVersion.ServerVersion()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("读取 Kubernetes 版本: %w", err)
	}
	// 先完整读取目标落点和可采集 Namespace 集合，确保规划只依赖同一时刻的只读集群事实。
	namespace := &corev1.Namespace{}
	namespaceExists := true
	if err := r.Get(ctx, client.ObjectKey{Name: instance.Spec.TargetNamespace}, namespace); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("读取目标 Namespace %q: %w", instance.Spec.TargetNamespace, err)
		}
		namespaceExists = false
	}
	namespaces := &corev1.NamespaceList{}
	if err := r.List(ctx, namespaces); err != nil {
		return ctrl.Result{}, fmt.Errorf("列出 Namespace: %w", err)
	}
	namespaceNames := make([]string, 0, len(namespaces.Items))
	for index := range namespaces.Items {
		namespaceNames = append(namespaceNames, namespaces.Items[index].Name)
	}

	plan, problem := ksm.BuildPlan(instance, ksm.ClusterFacts{
		KubernetesVersion:     serverVersion.GitVersion,
		TargetNamespaceExists: namespaceExists,
		NamespaceNames:        namespaceNames,
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
	resources := plan.Resources
	suspended := instance.Spec.Suspended != nil && *instance.Spec.Suspended
	if suspended {
		resources = nonWorkloadResources(plan.Resources)
	}
	// 在任何写入前检查本轮所有目标身份；零匹配时也必须保护待删除 workload 的所有权。
	conflictCandidates := resources
	if len(plan.EffectiveNamespaces) == 0 {
		conflictCandidates = append(append([]client.Object(nil), resources...), plan.Workloads...)
	}
	if conflict, err := r.findOwnershipConflict(ctx, conflictCandidates); err != nil {
		return ctrl.Result{}, err
	} else if conflict != nil {
		if err := r.updateProblemStatus(ctx, instance, conflict); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: dependencyRetryInterval}, nil
	}
	// 恢复或普通调谐遇到终止中的旧资源时，等待 API 确认删除，避免 apply 与删除交错。
	if !suspended {
		if waiting, err := r.terminatingManagedResources(ctx, plan.Resources); err != nil {
			return ctrl.Result{}, err
		} else if len(waiting) > 0 {
			if err := r.updateCleanupStatus(ctx, instance, waiting, ksm.ReasonWaitingForDeletion, nil); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: cleanupRetryInterval}, nil
		}
	}
	// 在首次写入前固定清理所需的 Namespace；即使后续只成功创建部分资源，停用和删除也能找到它们。
	if instance.Status.AppliedTargetNamespace == "" {
		if err := r.checkpointAppliedTargetNamespace(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 所有写入都通过 owner 预检后才执行，保证同名非受管资源不会被部分接管。
	for _, desired := range resources {
		if err := r.apply(ctx, desired); err != nil {
			return ctrl.Result{}, r.updateWriteProblem(ctx, instance, "写入", desired, err)
		}
	}
	if suspended {
		return r.reconcileSuspended(ctx, instance, plan)
	}
	// 零匹配是稳定业务状态：先保持非 workload 资源，再安全删除全部已拥有 workload。
	if len(plan.EffectiveNamespaces) == 0 {
		workloadsGone := true
		for _, workload := range plan.Workloads {
			gone, err := r.deleteOwned(ctx, workload)
			if err != nil {
				return ctrl.Result{}, r.updateWriteProblem(ctx, instance, "删除", workload, err)
			}
			workloadsGone = workloadsGone && gone
		}
		if !workloadsGone {
			return ctrl.Result{RequeueAfter: dependencyRetryInterval}, nil
		}
		if err := r.updateNoNamespaceMatchedStatus(ctx, instance, plan); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
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

func (r *KubeStateMetricsReconciler) reconcileDisabled(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics) (ctrl.Result, error) {
	// 持续发出全部直接受管资源的删除，并把失败或仍存在的资源暴露为可重试状态。
	resources, err := r.deleteManagedResources(ctx, instance)
	if err != nil {
		statusErr := r.updateCleanupStatus(ctx, instance, resources, ksm.ReasonDisabling, err)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("停用清理失败: %v；并且更新失败 status: %w", err, statusErr)
		}
		return ctrl.Result{}, err
	}
	if len(resources) > 0 {
		if err := r.updateCleanupStatus(ctx, instance, resources, ksm.ReasonDisabling, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: cleanupRetryInterval}, nil
	}
	// 只有 API 已确认全部资源消失，停用才算完成。
	if err := r.updateDisabledStatus(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *KubeStateMetricsReconciler) reconcileSuspended(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, plan *ksm.ResourcePlan) (ctrl.Result, error) {
	// 挂起只收敛 workload controller 的删除，前序调谐已经确保非 workload 资源继续存在。
	workload := plan.Workloads
	resources, err := r.deleteOwnedResources(ctx, instance, workload)
	if err != nil {
		statusErr := r.updateSuspendedStatus(ctx, instance, plan, resources, true, err)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("挂起 workload 失败: %v；并且更新失败 status: %w", err, statusErr)
		}
		return ctrl.Result{}, err
	}
	if len(resources) > 0 {
		if err := r.updateSuspendedStatus(ctx, instance, plan, resources, true, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: cleanupRetryInterval}, nil
	}
	// 确认 workload 消失后才报告管理员可观察的 Suspended 契约。
	if err := r.updateSuspendedStatus(ctx, instance, plan, nil, false, nil); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *KubeStateMetricsReconciler) reconcileDeletion(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(instance, kubeStateMetricsFinalizer) {
		return ctrl.Result{}, nil
	}
	// finalizer 期间持续主动删除并确认全部直接受管资源，任何失败都保留 finalizer 供后续重试。
	resources, err := r.deleteManagedResources(ctx, instance)
	if err != nil {
		statusErr := r.updateDeletingStatus(ctx, instance, resources, err)
		if statusErr != nil {
			return ctrl.Result{}, fmt.Errorf("删除受管资源失败: %v；并且更新失败 status: %w", err, statusErr)
		}
		return ctrl.Result{}, err
	}
	if len(resources) > 0 {
		if err := r.updateDeletingStatus(ctx, instance, resources, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: cleanupRetryInterval}, nil
	}
	// 只有清理集合为空时才释放 CR；删除一旦开始，不再进入普通调谐路径。
	if err := r.updateDeletingStatus(ctx, instance, nil, nil); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(instance, kubeStateMetricsFinalizer)
	if err := r.Update(ctx, instance); err != nil {
		return ctrl.Result{}, fmt.Errorf("移除 KubeStateMetrics finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// deleteManagedResources 只按稳定身份查找资源，并再次核对 controller owner，避免删除同名非受管对象。
func (r *KubeStateMetricsReconciler) deleteManagedResources(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics) ([]monitoringv1alpha1.ManagedResourceReference, error) {
	return r.deleteOwnedResources(ctx, instance, directManagedResources(instance))
}

func (r *KubeStateMetricsReconciler) deleteOwnedResources(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, resources []client.Object) ([]monitoringv1alpha1.ManagedResourceReference, error) {
	var found []monitoringv1alpha1.ManagedResourceReference
	var deleteErrors []error
	// 逐个读取稳定身份并核对 controller owner；同名非受管对象必须跳过，各资源错误则聚合后统一重试。
	for _, identity := range resources {
		current, err := emptyObjectFor(identity)
		if err != nil {
			return found, err
		}
		key := client.ObjectKeyFromObject(identity)
		if err := r.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			deleteErrors = append(deleteErrors, fmt.Errorf("读取待删除受管资源 %T %q: %w", identity, key.String(), err))
			continue
		}
		if !managedByInstance(current, instance) {
			continue
		}
		found = append(found, managedResourceReference(current))
		if !current.GetDeletionTimestamp().IsZero() {
			continue
		}
		uid := current.GetUID()
		if err := r.Delete(ctx, current, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			deleteErrors = append(deleteErrors, fmt.Errorf("删除受管资源 %T %q: %w", current, key.String(), err))
		}
	}
	return found, errors.Join(deleteErrors...)
}

func (r *KubeStateMetricsReconciler) terminatingManagedResources(ctx context.Context, resources []client.Object) ([]monitoringv1alpha1.ManagedResourceReference, error) {
	var terminating []monitoringv1alpha1.ManagedResourceReference
	for _, identity := range resources {
		current, err := emptyObjectFor(identity)
		if err != nil {
			return nil, err
		}
		key := client.ObjectKeyFromObject(identity)
		if err := r.Get(ctx, key, current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("检查终止中的受管资源 %T %q: %w", identity, key.String(), err)
		}
		if !current.GetDeletionTimestamp().IsZero() {
			terminating = append(terminating, managedResourceReference(current))
		}
	}
	return terminating, nil
}

func (r *KubeStateMetricsReconciler) checkpointAppliedTargetNamespace(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics) error {
	before := instance.Status.DeepCopy()
	instance.Status.AppliedTargetNamespace = instance.Spec.TargetNamespace
	if err := r.writeStatusIfChanged(ctx, instance, *before); err != nil {
		return fmt.Errorf("在写入受管资源前记录目标 Namespace %q: %w", instance.Spec.TargetNamespace, err)
	}
	return nil
}

func directManagedResources(instance *monitoringv1alpha1.KubeStateMetrics) []client.Object {
	name := ksm.ManagedResourceName(instance.Name)
	namespace := instance.Status.AppliedTargetNamespace
	if namespace == "" {
		namespace = instance.Spec.TargetNamespace
	}
	return []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: name}},
	}
}

func nonWorkloadResources(resources []client.Object) []client.Object {
	result := make([]client.Object, 0, len(resources))
	for _, resource := range resources {
		if _, workload := resource.(*appsv1.Deployment); !workload {
			result = append(result, resource)
		}
	}
	return result
}

func workloadResources(resources []client.Object) []client.Object {
	result := make([]client.Object, 0, 1)
	for _, resource := range resources {
		if _, workload := resource.(*appsv1.Deployment); workload {
			result = append(result, resource)
		}
	}
	return result
}

func managedByInstance(resource client.Object, instance *monitoringv1alpha1.KubeStateMetrics) bool {
	return hasManagingOwner(resource, metav1.OwnerReference{
		APIVersion: monitoringv1alpha1.GroupVersion.String(),
		Kind:       "KubeStateMetrics",
		Name:       instance.Name,
		UID:        instance.UID,
	})
}

func cleanupWasInterrupted(instance *monitoringv1alpha1.KubeStateMetrics) bool {
	if instance.Status.Phase == monitoringv1alpha1.KubeStateMetricsPhaseDisabling {
		return true
	}
	condition := apiMeta.FindStatusCondition(instance.Status.Conditions, "Progressing")
	return condition != nil && condition.Status == metav1.ConditionTrue && (condition.Reason == ksm.ReasonDisabling || condition.Reason == ksm.ReasonWaitingForDeletion)
}

func (r *KubeStateMetricsReconciler) updateWriteProblem(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, action string, object client.Object, writeErr error) error {
	reason := ksm.ReasonApplyFailed
	var conflict *ownershipConflictError
	if errors.As(writeErr, &conflict) {
		reason = ksm.ReasonResourceConflict
	}
	problem := &ksm.Problem{Reason: reason, Message: fmt.Sprintf("%s %T %q: %v", action, object, object.GetName(), writeErr)}
	if statusErr := r.updateProblemStatus(ctx, instance, problem); statusErr != nil {
		return fmt.Errorf("%s %T %q 失败: %v；并且更新失败 status: %w", action, object, object.GetName(), writeErr, statusErr)
	}
	return fmt.Errorf("%s %T %q: %w", action, object, object.GetName(), writeErr)
}

func (r *KubeStateMetricsReconciler) deleteOwned(ctx context.Context, desired client.Object) (bool, error) {
	current, err := emptyObjectFor(desired)
	if err != nil {
		return false, err
	}
	key := client.ObjectKeyFromObject(desired)
	if err := r.Get(ctx, key, current); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !hasManagingOwner(current, desired.GetOwnerReferences()[0]) {
		return false, &ownershipConflictError{message: "待删除的同名 workload 不由当前 KubeStateMetrics UID 管理"}
	}
	uid := current.GetUID()
	if err := r.Delete(ctx, current, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
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

func (r *KubeStateMetricsReconciler) updateCleanupStatus(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, resources []monitoringv1alpha1.ManagedResourceReference, reason string, cleanupErr error) error {
	before := instance.Status.DeepCopy()
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.ManagedResources = resources
	instance.Status.DesiredPods = 0
	instance.Status.AvailablePods = 0
	message := "等待已发出删除的受管资源消失"
	if reason == ksm.ReasonDisabling {
		message = "正在删除全部直接受管资源"
	}
	setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, reason, message)
	setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionTrue, reason, message)
	setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, reason, "受管 workload 不可用")
	if cleanupErr != nil {
		setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionTrue, ksm.ReasonDeleteFailed, cleanupErr.Error())
	} else {
		setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionFalse, reason, "清理未发生故障")
	}
	return r.writeStatusIfChanged(ctx, instance, *before)
}

func (r *KubeStateMetricsReconciler) updateDisabledStatus(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics) error {
	before := instance.Status.DeepCopy()
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.ManagedResources = nil
	instance.Status.KubeStateMetricsVersion = ""
	instance.Status.DeploymentMode = ""
	instance.Status.DesiredPods = 0
	instance.Status.AvailablePods = 0
	setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, ksm.ReasonDisabled, "受管实例已停用")
	setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionFalse, ksm.ReasonDisabled, "全部直接受管资源已删除")
	setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, ksm.ReasonDisabled, "停用实例没有可用 workload")
	setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionFalse, ksm.ReasonDisabled, "停用未发生故障")
	return r.writeStatusIfChanged(ctx, instance, *before)
}

func (r *KubeStateMetricsReconciler) updateSuspendedStatus(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, plan *ksm.ResourcePlan, workloads []monitoringv1alpha1.ManagedResourceReference, progressing bool, cleanupErr error) error {
	before := instance.Status.DeepCopy()
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.AppliedTargetNamespace = instance.Spec.TargetNamespace
	instance.Status.KubeStateMetricsVersion = plan.Profile.Version
	instance.Status.DeploymentMode = monitoringv1alpha1.DeploymentModeDeployment
	instance.Status.DesiredPods = 0
	instance.Status.AvailablePods = 0
	instance.Status.ManagedResources = append(managedResourceReferences(nonWorkloadResources(plan.Resources)), workloads...)
	// 删除失败、等待删除和完成挂起是互斥阶段，各自保持 Ready/Available/Degraded 的一致组合。
	if cleanupErr != nil {
		setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, ksm.ReasonDeleteFailed, "删除 workload controller 失败")
		setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionTrue, ksm.ReasonSuspending, "持续重试删除 workload controller")
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, ksm.ReasonSuspending, "workload controller 正在停止")
		setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionTrue, ksm.ReasonDeleteFailed, cleanupErr.Error())
	} else if progressing {
		setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, ksm.ReasonSuspending, "等待 workload controller 消失")
		setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionTrue, ksm.ReasonSuspending, "已发出 workload controller 删除")
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, ksm.ReasonSuspending, "workload controller 正在停止")
		setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionFalse, ksm.ReasonSuspending, "挂起未发生故障")
	} else {
		setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionTrue, ksm.ReasonSuspended, "非 workload 受管资源已保留，workload 已停止")
		setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionFalse, ksm.ReasonSuspended, "挂起已完成")
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, ksm.ReasonSuspended, "挂起实例不运行 Pod")
		setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionFalse, ksm.ReasonSuspended, "挂起未发生故障")
	}
	return r.writeStatusIfChanged(ctx, instance, *before)
}

func (r *KubeStateMetricsReconciler) updateDeletingStatus(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, resources []monitoringv1alpha1.ManagedResourceReference, cleanupErr error) error {
	before := instance.Status.DeepCopy()
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.ManagedResources = resources
	instance.Status.DesiredPods = 0
	instance.Status.AvailablePods = 0
	setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, ksm.ReasonDeleting, "受管实例正在删除")
	setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionTrue, ksm.ReasonDeleting, "等待全部直接受管资源消失")
	setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, ksm.ReasonDeleting, "删除中的实例不可用")
	if cleanupErr != nil {
		setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionTrue, ksm.ReasonDeleteFailed, cleanupErr.Error())
	} else {
		setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionFalse, ksm.ReasonDeleting, "删除未发生故障")
	}
	return r.writeStatusIfChanged(ctx, instance, *before)
}

func (r *KubeStateMetricsReconciler) updateProblemStatus(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, problem *ksm.Problem) error {
	before := instance.Status.DeepCopy()
	instance.Status.ObservedGeneration = instance.Generation
	setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, problem.Reason, problem.Message)
	setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionFalse, problem.Reason, problem.Message)
	setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionTrue, problem.Reason, problem.Message)
	if instance.Status.AppliedTargetNamespace == "" {
		instance.Status.ManagedResources = nil
		instance.Status.KubeStateMetricsVersion = ""
		instance.Status.DeploymentMode = ""
		instance.Status.EffectiveCollectors = nil
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
	instance.Status.EffectiveCollectors = append([]string(nil), plan.EffectiveCollectors...)
	instance.Status.EffectiveNamespaceCount = int32(len(plan.EffectiveNamespaces))
	instance.Status.DesiredPods = 1
	instance.Status.AvailablePods = availablePods
	instance.Status.ManagedResources = managedResourceReferences(plan.Resources)
	if availablePods >= 1 {
		setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionTrue, "ReconcileSucceeded", "受管实例已就绪")
		setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionFalse, "ReconcileSucceeded", "无需继续推进")
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionTrue, "MinimumReplicasAvailable", "单副本 Deployment 可用")
	} else {
		setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionFalse, "DeploymentProgressing", "等待单副本 Deployment 可用")
		setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionTrue, "DeploymentProgressing", "等待单副本 Deployment 可用")
		setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, "MinimumReplicasUnavailable", "单副本 Deployment 尚不可用")
	}
	setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionFalse, "ReconcileSucceeded", "未发现调谐故障")
	return r.writeStatusIfChanged(ctx, instance, *before)
}

func (r *KubeStateMetricsReconciler) updateNoNamespaceMatchedStatus(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, plan *ksm.ResourcePlan) error {
	before := instance.Status.DeepCopy()
	message := "当前没有 Namespace 匹配 spec.namespaceFilter，已停止全部 workload"
	instance.Status.ObservedGeneration = instance.Generation
	instance.Status.Phase = monitoringv1alpha1.KubeStateMetricsPhaseReady
	instance.Status.AppliedTargetNamespace = instance.Spec.TargetNamespace
	instance.Status.KubeStateMetricsVersion = plan.Profile.Version
	instance.Status.DeploymentMode = monitoringv1alpha1.DeploymentModeDeployment
	instance.Status.EffectiveCollectors = append([]string(nil), plan.EffectiveCollectors...)
	instance.Status.EffectiveNamespaceCount = 0
	instance.Status.DesiredPods = 0
	instance.Status.AvailablePods = 0
	instance.Status.ManagedResources = managedResourceReferences(plan.Resources)
	setCondition(&instance.Status, instance.Generation, "Ready", metav1.ConditionTrue, ksm.ReasonNoNamespaceMatched, message)
	setCondition(&instance.Status, instance.Generation, "Progressing", metav1.ConditionFalse, ksm.ReasonNoNamespaceMatched, message)
	setCondition(&instance.Status, instance.Generation, "Available", metav1.ConditionFalse, ksm.ReasonNoNamespaceMatched, message)
	setCondition(&instance.Status, instance.Generation, "Degraded", metav1.ConditionFalse, ksm.ReasonNoNamespaceMatched, message)
	return r.writeStatusIfChanged(ctx, instance, *before)
}

func (r *KubeStateMetricsReconciler) writeStatusIfChanged(ctx context.Context, instance *monitoringv1alpha1.KubeStateMetrics, before monitoringv1alpha1.KubeStateMetricsStatus) error {
	instance.Status.Phase = ksm.DerivePhase(instance)
	if apiequality.Semantic.DeepEqual(before, instance.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, instance); err != nil {
		return fmt.Errorf("更新 KubeStateMetrics status: %w", err)
	}
	r.recordStatusEvent(instance, before)
	return nil
}

func (r *KubeStateMetricsReconciler) recordStatusEvent(instance *monitoringv1alpha1.KubeStateMetrics, before monitoringv1alpha1.KubeStateMetricsStatus) {
	if r.Recorder == nil {
		return
	}
	oldFailure := failureReason(before.Conditions)
	newFailure := failureReason(instance.Status.Conditions)
	// Event 采用失败、恢复、phase、condition reason 的固定优先级，每次 status 写入最多产生一条。
	if newFailure != "" && newFailure != oldFailure {
		condition := apiMeta.FindStatusCondition(instance.Status.Conditions, "Degraded")
		r.Recorder.Event(instance, corev1.EventTypeWarning, newFailure, condition.Message)
		return
	}
	if oldFailure != "" && newFailure == "" {
		r.Recorder.Event(instance, corev1.EventTypeNormal, ksm.ReasonRecovered, "受管实例已从故障状态恢复")
		return
	}
	if before.Phase != instance.Status.Phase {
		r.Recorder.Event(instance, corev1.EventTypeNormal, string(instance.Status.Phase), fmt.Sprintf("受管实例进入 %s", instance.Status.Phase))
		return
	}
	if condition := changedReasonCondition(before.Conditions, instance.Status.Conditions); condition != nil {
		r.Recorder.Event(instance, corev1.EventTypeNormal, condition.Reason, condition.Message)
	}
}

func changedReasonCondition(before, after []metav1.Condition) *metav1.Condition {
	for _, conditionType := range []string{"Ready", "Progressing", "Available", "Degraded"} {
		current := apiMeta.FindStatusCondition(after, conditionType)
		if current == nil || current.Reason == "" {
			continue
		}
		previous := apiMeta.FindStatusCondition(before, conditionType)
		if previous == nil || previous.Reason != current.Reason {
			return current
		}
	}
	return nil
}

func failureReason(conditions []metav1.Condition) string {
	condition := apiMeta.FindStatusCondition(conditions, "Degraded")
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return ""
	}
	return condition.Reason
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
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.requestsForNamespace)).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.ClusterRole{}).
		Owns(&rbacv1.ClusterRoleBinding{}).
		Complete(r)
}

func (r *KubeStateMetricsReconciler) requestsForNamespace(ctx context.Context, _ client.Object) []reconcile.Request {
	instances := &monitoringv1alpha1.KubeStateMetricsList{}
	if err := r.List(ctx, instances); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Namespace 变化后列出 KubeStateMetrics 失败")
		return nil
	}
	// Namespace 集合会影响 All 和 regex Include，因此每次变化都重新规划所有受管实例。
	requests := make([]reconcile.Request, 0, len(instances.Items))
	for index := range instances.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: instances.Items[index].Name}})
	}
	return requests
}
