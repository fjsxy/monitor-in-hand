package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksmcontroller "github.com/fjsxy/monitor-in-hand/internal/controller"
	ksm "github.com/fjsxy/monitor-in-hand/internal/kubestatemetrics"
)

type deleteFailingClient struct {
	client.Client
}

type partiallyFailingClient struct {
	client.Client
}

type fixedServerVersion struct{}

func (fixedServerVersion) ServerVersion() (*version.Info, error) {
	return &version.Info{GitVersion: "v1.28.15"}, nil
}

func (c deleteFailingClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if _, ok := object.(*appsv1.Deployment); ok {
		return errors.New("injected delete failure")
	}
	return c.Client.Delete(ctx, object, options...)
}

func (c partiallyFailingClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if _, ok := object.(*corev1.ServiceAccount); ok {
		if err := c.Client.Create(ctx, object, options...); err != nil {
			return err
		}
		return errors.New("injected ambiguous apply failure")
	}
	return c.Client.Create(ctx, object, options...)
}

func TestPartialApplyKeepsOriginalNamespaceForDisableCleanup(t *testing.T) {
	// 准备两个 Namespace，并模拟 ServiceAccount 已被 API 持久化但客户端收到写入失败。
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	instance := &monitoringv1alpha1.KubeStateMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "partial-apply", UID: types.UID("uid-partial-apply")},
		Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: "original"},
	}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(instance, &appsv1.Deployment{}).
		WithObjects(instance,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "original"}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "replacement"}},
		).
		Build()
	reconciler := &ksmcontroller.KubeStateMetricsReconciler{
		Client:        partiallyFailingClient{Client: baseClient},
		ServerVersion: fixedServerVersion{},
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}}

	// 部分写入失败后必须先持久化原 Namespace，确保清理身份不会随后续 spec 漂移。
	if _, err := reconciler.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "injected ambiguous apply failure") {
		t.Fatalf("reconcile error = %v, want injected ambiguous apply failure", err)
	}
	actual := getInstance(t, ctx, baseClient, instance.Name)
	if actual.Status.AppliedTargetNamespace != "original" {
		t.Fatalf("appliedTargetNamespace = %q, want original", actual.Status.AppliedTargetNamespace)
	}
	resourceKey := client.ObjectKey{Namespace: "original", Name: "ksm-" + instance.Name}
	if err := baseClient.Get(ctx, resourceKey, &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("partially applied ServiceAccount missing: %v", err)
	}

	// 即使普通字段改到另一 Namespace，停用也必须旁路校验并主动清理旧 generation 的部分资源。
	actual.Spec.TargetNamespace = "replacement"
	actual.Spec.Enabled = boolValue(false)
	if err := baseClient.Update(ctx, actual); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	actual = getInstance(t, ctx, baseClient, instance.Name)
	if actual.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseDisabled {
		t.Fatalf("phase = %q, want Disabled", actual.Status.Phase)
	}
	if err := baseClient.Get(ctx, resourceKey, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old ServiceAccount get error = %v, want NotFound", err)
	}
}

func TestDeletionFailureKeepsFinalizerAndDeduplicatesEvent(t *testing.T) {
	// 准备一个处于删除中的 CR 与直接受管 Deployment，并在 Kubernetes 删除边界注入稳定失败。
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	deletingAt := metav1.NewTime(time.Now())
	instance := &monitoringv1alpha1.KubeStateMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "delete-failure",
			UID:               types.UID("uid-delete-failure"),
			Finalizers:        []string{"monitoring.fjsxy.github.io/kubestatemetrics-cleanup"},
			DeletionTimestamp: &deletingAt,
		},
		Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: "missing-and-irrelevant"},
	}
	controllerOwner := true
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      "ksm-delete-failure",
		Namespace: "missing-and-irrelevant",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: monitoringv1alpha1.GroupVersion.String(),
			Kind:       "KubeStateMetrics",
			Name:       instance.Name,
			UID:        instance.UID,
			Controller: &controllerOwner,
		}},
	}}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(instance).
		WithObjects(instance, deployment).
		Build()
	recorder := record.NewFakeRecorder(10)
	reconciler := &ksmcontroller.KubeStateMetricsReconciler{
		Client:   deleteFailingClient{Client: baseClient},
		Recorder: recorder,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}}

	// 第一次失败必须写入可观察状态、保留 finalizer，并产生唯一的 Warning Event。
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("first reconcile error = %v, want injected delete failure", err)
	}
	actual := &monitoringv1alpha1.KubeStateMetrics{}
	if err := baseClient.Get(context.Background(), client.ObjectKey{Name: instance.Name}, actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseDeleting {
		t.Fatalf("phase = %q, want Deleting", actual.Status.Phase)
	}
	if !containsString(actual.Finalizers, "monitoring.fjsxy.github.io/kubestatemetrics-cleanup") {
		t.Fatalf("finalizers = %v, want cleanup finalizer", actual.Finalizers)
	}
	degraded := findCondition(actual.Status.Conditions, "Degraded")
	if degraded.Status != metav1.ConditionTrue || degraded.Reason != "DeleteFailed" {
		t.Fatalf("Degraded = %#v, want True/DeleteFailed", degraded)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Warning DeleteFailed") {
			t.Fatalf("event = %q, want Warning DeleteFailed", event)
		}
	default:
		t.Fatal("first delete failure did not emit an event")
	}

	// 相同 reason 的后续失败仍返回错误以重试，但不能制造 Event 风暴。
	if _, err := reconciler.Reconcile(context.Background(), request); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("second reconcile error = %v, want injected delete failure", err)
	}
	select {
	case event := <-recorder.Events:
		t.Fatalf("duplicate failure emitted event %q", event)
	default:
	}

	// 删除边界恢复后会确认资源消失、移除 finalizer，并只发一次恢复 Event。
	currentDeployment := &appsv1.Deployment{}
	if err := baseClient.Get(context.Background(), client.ObjectKeyFromObject(deployment), currentDeployment); err != nil {
		t.Fatal(err)
	}
	if err := baseClient.Delete(context.Background(), currentDeployment); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("recovery reconcile error = %v", err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Normal Recovered") {
			t.Fatalf("recovery event = %q, want Normal Recovered", event)
		}
	default:
		t.Fatal("delete recovery did not emit an event")
	}
}

func TestManagedInstanceLifecycle(t *testing.T) {
	// 准备公开 Reconcile seam 所需的 CR、Namespace、真实资源类型 scheme 与 Event recorder。
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	enabled, suspended := true, false
	instance := &monitoringv1alpha1.KubeStateMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: "lifecycle-unit", UID: types.UID("uid-lifecycle-unit")},
		Spec: monitoringv1alpha1.KubeStateMetricsSpec{
			TargetNamespace: "lifecycle-unit",
			Enabled:         &enabled,
			Suspended:       &suspended,
		},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.Spec.TargetNamespace}}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(instance, &appsv1.Deployment{}).
		WithObjects(instance, namespace).
		Build()
	recorder := record.NewFakeRecorder(30)
	reconciler := &ksmcontroller.KubeStateMetricsReconciler{
		Client:        baseClient,
		ServerVersion: fixedServerVersion{},
		Recorder:      recorder,
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	resourceKey := client.ObjectKey{Namespace: instance.Spec.TargetNamespace, Name: "ksm-" + instance.Name}

	// 停用必须先报告 Disabling，再在确认全部资源消失后报告 Disabled。
	actual := getInstance(t, ctx, baseClient, instance.Name)
	actual.Spec.Enabled = boolValue(false)
	if err := baseClient.Update(ctx, actual); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	actual = getInstance(t, ctx, baseClient, instance.Name)
	if actual.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseDisabling {
		t.Fatalf("disable phase = %q, want Disabling", actual.Status.Phase)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	actual = getInstance(t, ctx, baseClient, instance.Name)
	if actual.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseDisabled || len(actual.Status.ManagedResources) != 0 {
		t.Fatalf("disabled status = %#v", actual.Status)
	}

	// 重新启用按当前声明重建，随后挂起只删除 Deployment 并保留四类非 workload 资源。
	actual.Spec.Enabled = boolValue(true)
	if err := baseClient.Update(ctx, actual); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := baseClient.Get(ctx, resourceKey, &appsv1.Deployment{}); err != nil {
		t.Fatalf("deployment was not rebuilt: %v", err)
	}
	actual = getInstance(t, ctx, baseClient, instance.Name)
	actual.Spec.Suspended = boolValue(true)
	if err := baseClient.Update(ctx, actual); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	actual = getInstance(t, ctx, baseClient, instance.Name)
	if actual.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady ||
		findCondition(actual.Status.Conditions, "Ready").Reason != ksm.ReasonSuspended || len(actual.Status.ManagedResources) != 4 {
		t.Fatalf("suspended status = %#v", actual.Status)
	}
	if err := baseClient.Get(ctx, resourceKey, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("suspended deployment get error = %v, want NotFound", err)
	}
	for _, object := range []client.Object{
		&corev1.Service{},
		&corev1.ServiceAccount{},
	} {
		if err := baseClient.Get(ctx, resourceKey, object); err != nil {
			t.Fatalf("retained %T get error = %v", object, err)
		}
	}
	for _, object := range []client.Object{&rbacv1.ClusterRole{}, &rbacv1.ClusterRoleBinding{}} {
		if err := baseClient.Get(ctx, client.ObjectKey{Name: resourceKey.Name}, object); err != nil {
			t.Fatalf("retained %T get error = %v", object, err)
		}
	}

	// 预置已可用 workload，使恢复保持 Ready phase，单独验证 reason 变化仍产生 Event。
	plan, problem := ksm.BuildPlan(actual, ksm.ClusterFacts{
		KubernetesVersion:     "v1.28.15",
		TargetNamespaceExists: true,
		NamespaceNames:        []string{actual.Spec.TargetNamespace},
	})
	if problem != nil {
		t.Fatalf("build resume plan: %#v", problem)
	}
	for _, resource := range plan.Resources {
		deployment, ok := resource.(*appsv1.Deployment)
		if !ok {
			continue
		}
		deployment.Status.AvailableReplicas = 1
		if err := baseClient.Create(ctx, deployment); err != nil {
			t.Fatal(err)
		}
	}
	drainEvents(recorder)
	actual.Spec.Suspended = boolValue(false)
	if err := baseClient.Update(ctx, actual); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	actual = getInstance(t, ctx, baseClient, instance.Name)
	if actual.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady || findCondition(actual.Status.Conditions, "Ready").Reason != "ReconcileSucceeded" {
		t.Fatalf("resumed status = %#v", actual.Status)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Normal ReconcileSucceeded") {
			t.Fatalf("reason-change event = %q", event)
		}
	default:
		t.Fatal("Ready reason change did not emit an event")
	}

	// CR 删除必须在受管 Deployment finalizer 阻塞期间保持 Deleting，解除后才移除自身 finalizer。
	deployment := &appsv1.Deployment{}
	if err := baseClient.Get(ctx, resourceKey, deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Finalizers = []string{"tests.monitor-in-hand/block-deletion"}
	if err := baseClient.Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	if err := baseClient.Delete(ctx, actual); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	actual = getInstance(t, ctx, baseClient, instance.Name)
	if actual.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseDeleting || !containsString(actual.Finalizers, "monitoring.fjsxy.github.io/kubestatemetrics-cleanup") {
		t.Fatalf("blocked deleting object = %#v", actual)
	}
	if err := baseClient.Get(ctx, resourceKey, deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.DeletionTimestamp.IsZero() {
		t.Fatal("blocked deployment does not have deletionTimestamp")
	}
	deployment.Finalizers = nil
	if err := baseClient.Update(ctx, deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: instance.Name}, &monitoringv1alpha1.KubeStateMetrics{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleted instance get error = %v, want NotFound", err)
	}
}

func getInstance(t *testing.T, ctx context.Context, kubeClient client.Client, name string) *monitoringv1alpha1.KubeStateMetrics {
	t.Helper()
	instance := &monitoringv1alpha1.KubeStateMetrics{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Name: name}, instance); err != nil {
		t.Fatal(err)
	}
	return instance
}

func boolValue(value bool) *bool { return &value }

func drainEvents(recorder *record.FakeRecorder) {
	for {
		select {
		case <-recorder.Events:
		default:
			return
		}
	}
}

func findCondition(conditions []metav1.Condition, conditionType string) metav1.Condition {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition
		}
	}
	return metav1.Condition{}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
