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

type replacingBeforeDeleteClient struct {
	client.Client
	replaced bool
}

type transitionReadFailingClient struct {
	client.Client
}

type existingRoleReadFailingClient struct {
	client.Client
}

type existingStatefulSetReadFailingClient struct {
	client.Client
}

type servicePreflightReadFailingClient struct {
	client.Client
}

type secondServiceReadFailingClient struct {
	client.Client
	reads int
}

type deploymentObservationFailingClient struct {
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

func (c *replacingBeforeDeleteClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if _, ok := object.(*appsv1.Deployment); ok && !c.replaced {
		c.replaced = true
		// 在真正处理带 UID 前置条件的删除前，用同名新 UID 对象替换已分类的旧对象，复现读取与删除之间的竞态窗口。
		if err := c.Client.Delete(ctx, object); err != nil {
			return err
		}
		replacement := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:      object.GetName(),
			Namespace: object.GetNamespace(),
			UID:       types.UID("uid-conflicting-replacement"),
			Labels:    map[string]string{"preserve": "true"},
		}}
		if err := c.Client.Create(ctx, replacement); err != nil {
			return err
		}
		// fake client 不会模拟 API Server 的删除前置条件，因此显式拒绝针对旧 UID 的删除请求。
		deleteOptions := (&client.DeleteOptions{}).ApplyOptions(options)
		if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil && *deleteOptions.Preconditions.UID != replacement.UID {
			return errors.New("delete UID precondition rejected conflicting replacement")
		}
	}
	return c.Client.Delete(ctx, object, options...)
}

func TestFinalizerDeleteUsesUIDPrecondition(t *testing.T) {
	t.Run("UT-KSM-GH53-003 finalizer 不删除竞态替换的冲突对象", func(t *testing.T) {
		// 准备删除中的 CR 和当前 UID 拥有的 Deployment，在删除边界模拟同名对象被替换。
		ctx := context.Background()
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
				Name:              "uid-precondition",
				UID:               types.UID("uid-current-instance"),
				Finalizers:        []string{"monitoring.fjsxy.github.io/kubestatemetrics-cleanup"},
				DeletionTimestamp: &deletingAt,
			},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: "uid-precondition"},
			Status: monitoringv1alpha1.KubeStateMetricsStatus{
				AppliedTargetNamespace: "uid-precondition",
				ManagedResources: []monitoringv1alpha1.ManagedResourceReference{{
					APIVersion: "v1", Kind: "Service", Name: "poisoned-status", Namespace: "unrelated",
				}},
			},
		}
		controllerOwner := true
		deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:      "ksm-uid-precondition",
			Namespace: "uid-precondition",
			UID:       types.UID("uid-owned-deployment"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: monitoringv1alpha1.GroupVersion.String(), Kind: "KubeStateMetrics", Name: instance.Name,
				UID: instance.UID, Controller: &controllerOwner,
			}},
		}}
		baseClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(instance).WithObjects(instance, deployment).Build()
		racingClient := &replacingBeforeDeleteClient{Client: baseClient}
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{Client: racingClient}
		request := ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}}

		// 删除请求必须以已分类对象的 UID 为前置条件，不得删除竞态窗口中出现的冲突对象。
		_, _ = reconciler.Reconcile(ctx, request)
		replacement := &appsv1.Deployment{}
		if err := baseClient.Get(ctx, client.ObjectKeyFromObject(deployment), replacement); err != nil {
			t.Fatalf("conflicting replacement was deleted: %v", err)
		}
		if replacement.UID != types.UID("uid-conflicting-replacement") || replacement.Labels["preserve"] != "true" {
			t.Fatalf("replacement = %#v, want preserved conflicting object", replacement.ObjectMeta)
		}

		// 后续调谐只依据真实 ownerReference 分类，忽略被污染的 managedResources 并释放 finalizer。
		_, _ = reconciler.Reconcile(ctx, request)
		if err := baseClient.Get(ctx, client.ObjectKeyFromObject(deployment), replacement); err != nil {
			t.Fatalf("conflicting replacement did not survive finalization: %v", err)
		}
		if err := baseClient.Get(ctx, client.ObjectKey{Name: instance.Name}, &monitoringv1alpha1.KubeStateMetrics{}); !apierrors.IsNotFound(err) {
			t.Fatalf("instance get error = %v, want NotFound after owned resources are gone", err)
		}
	})
}

func TestMultiInstanceOwnershipAndDrift(t *testing.T) {
	t.Run("UT-KSM-GH53-005 多实例所有权、漂移与冲突恢复相互隔离", func(t *testing.T) {
		// 准备共享 Namespace 中的两个正常实例，以及期望 Service 被旧 UID 占用的冲突实例。
		ctx := context.Background()
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gh53-shared"}}
		first := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "gh53.first", UID: types.UID("uid-gh53-first")},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace.Name},
		}
		second := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "gh53-second", UID: types.UID("uid-gh53-second")},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace.Name},
		}
		conflict := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "gh53-conflict", UID: types.UID("uid-gh53-current")},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace.Name},
		}
		controllerOwner := true
		occupied := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name:      ksm.ManagedResourceName(conflict.Name),
			Namespace: namespace.Name,
			Labels:    map[string]string{"preserve": "true"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: monitoringv1alpha1.GroupVersion.String(), Kind: "KubeStateMetrics", Name: conflict.Name,
				UID: types.UID("uid-gh53-old"), Controller: &controllerOwner,
			}},
		}, Spec: corev1.ServiceSpec{Selector: map[string]string{"preserve": "true"}}}
		baseClient := fake.NewClientBuilder().WithScheme(scheme).
			WithStatusSubresource(first, second, conflict, &appsv1.Deployment{}).
			WithObjects(namespace, first, second, conflict, occupied).
			Build()
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{Client: baseClient, ServerVersion: fixedServerVersion{}}
		reconcile := func(name string) error {
			_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
			return err
		}

		// 调谐正常实例，验证同 Namespace 中的资源名、owner UID 和 Service selector 互不重叠。
		for _, instance := range []*monitoringv1alpha1.KubeStateMetrics{first, second} {
			if err := reconcile(instance.Name); err != nil {
				t.Fatal(err)
			}
			name := ksm.ManagedResourceName(instance.Name)
			deployment := &appsv1.Deployment{}
			if err := baseClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: name}, deployment); err != nil {
				t.Fatal(err)
			}
			if !ownerUIDExists(deployment.OwnerReferences, instance.UID) {
				t.Fatalf("deployment owners = %v, want UID %s", deployment.OwnerReferences, instance.UID)
			}
			service := &corev1.Service{}
			if err := baseClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: name}, service); err != nil {
				t.Fatal(err)
			}
			if len(service.Spec.Selector) != 3 || service.Spec.Selector["app.kubernetes.io/instance"] != deployment.Labels["app.kubernetes.io/instance"] {
				t.Fatalf("service selector = %v, deployment labels = %v", service.Spec.Selector, deployment.Labels)
			}
		}

		// 修改已拥有 Service 后通过公开 Reconcile seam 验证完整声明状态被恢复。
		firstService := &corev1.Service{}
		firstKey := client.ObjectKey{Namespace: namespace.Name, Name: ksm.ManagedResourceName(first.Name)}
		if err := baseClient.Get(ctx, firstKey, firstService); err != nil {
			t.Fatal(err)
		}
		firstService.Labels = map[string]string{"drifted": "true"}
		firstService.Spec.Selector = map[string]string{"drifted": "true"}
		if err := baseClient.Update(ctx, firstService); err != nil {
			t.Fatal(err)
		}
		if err := reconcile(first.Name); err != nil {
			t.Fatal(err)
		}
		if err := baseClient.Get(ctx, firstKey, firstService); err != nil {
			t.Fatal(err)
		}
		if len(firstService.Spec.Selector) != 3 || firstService.Labels["app.kubernetes.io/managed-by"] != "monitor-in-hand" || firstService.Labels["drifted"] != "" {
			t.Fatalf("service drift was not restored: labels=%v selector=%v", firstService.Labels, firstService.Spec.Selector)
		}
		firstDeployment := &appsv1.Deployment{}
		if err := baseClient.Get(ctx, firstKey, firstDeployment); err != nil {
			t.Fatal(err)
		}
		if err := baseClient.Delete(ctx, firstDeployment); err != nil {
			t.Fatal(err)
		}
		if err := reconcile(first.Name); err != nil {
			t.Fatal(err)
		}
		if err := baseClient.Get(ctx, firstKey, &appsv1.Deployment{}); err != nil {
			t.Fatalf("deleted deployment was not recreated: %v", err)
		}

		// 旧 UID 对象必须保持原样并报告冲突；移除占用后当前实例可继续收敛。
		if err := reconcile(conflict.Name); err != nil {
			t.Fatal(err)
		}
		actualConflict := getInstance(t, ctx, baseClient, conflict.Name)
		if degraded := findCondition(actualConflict.Status.Conditions, "Degraded"); degraded.Status != metav1.ConditionTrue || degraded.Reason != ksm.ReasonResourceConflict {
			t.Fatalf("Degraded = %#v, want True/ResourceConflict", degraded)
		}
		if err := baseClient.Get(ctx, client.ObjectKeyFromObject(occupied), occupied); err != nil {
			t.Fatal(err)
		}
		if occupied.Labels["preserve"] != "true" || !ownerUIDExists(occupied.OwnerReferences, types.UID("uid-gh53-old")) {
			t.Fatalf("occupied service was modified: %#v", occupied.ObjectMeta)
		}
		if err := baseClient.Delete(ctx, occupied); err != nil {
			t.Fatal(err)
		}
		if err := reconcile(conflict.Name); err != nil {
			t.Fatal(err)
		}
		if err := baseClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: ksm.ManagedResourceName(conflict.Name)}, &appsv1.Deployment{}); err != nil {
			t.Fatalf("conflict recovery did not create deployment: %v", err)
		}

		// 以新实例补齐无 owner 和其他实例 owner 分类，两者均不得被接管并可在占用消失后恢复。
		for _, conflictCase := range []struct {
			name   string
			owners []metav1.OwnerReference
		}{
			{name: "gh53-unowned"},
			{name: "gh53-wrong-owner", owners: []metav1.OwnerReference{{
				APIVersion: monitoringv1alpha1.GroupVersion.String(), Kind: "KubeStateMetrics", Name: "another-instance",
				UID: types.UID("uid-another-instance"), Controller: &controllerOwner,
			}}},
		} {
			instance := &monitoringv1alpha1.KubeStateMetrics{
				ObjectMeta: metav1.ObjectMeta{Name: conflictCase.name, UID: types.UID("uid-" + conflictCase.name)},
				Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace.Name},
			}
			occupiedService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: ksm.ManagedResourceName(instance.Name), Namespace: namespace.Name,
				Labels: map[string]string{"preserve": "true"}, OwnerReferences: conflictCase.owners,
			}}
			if err := baseClient.Create(ctx, instance); err != nil {
				t.Fatal(err)
			}
			if err := baseClient.Create(ctx, occupiedService); err != nil {
				t.Fatal(err)
			}
			if err := reconcile(instance.Name); err != nil {
				t.Fatal(err)
			}
			actual := getInstance(t, ctx, baseClient, instance.Name)
			if degraded := findCondition(actual.Status.Conditions, "Degraded"); degraded.Status != metav1.ConditionTrue || degraded.Reason != ksm.ReasonResourceConflict {
				t.Fatalf("%s Degraded = %#v, want True/ResourceConflict", instance.Name, degraded)
			}
			if err := baseClient.Get(ctx, client.ObjectKeyFromObject(occupiedService), occupiedService); err != nil {
				t.Fatal(err)
			}
			if occupiedService.Labels["preserve"] != "true" {
				t.Fatalf("%s conflict object was modified", instance.Name)
			}
			if err := baseClient.Delete(ctx, occupiedService); err != nil {
				t.Fatal(err)
			}
			if err := reconcile(instance.Name); err != nil {
				t.Fatal(err)
			}
			if err := baseClient.Get(ctx, client.ObjectKey{Namespace: namespace.Name, Name: ksm.ManagedResourceName(instance.Name)}, &appsv1.Deployment{}); err != nil {
				t.Fatalf("%s conflict recovery did not create deployment: %v", instance.Name, err)
			}
		}

		// 停用第一个实例只清理它的资源，第二个实例仍保持可发现。
		actualFirst := getInstance(t, ctx, baseClient, first.Name)
		actualFirst.Spec.Enabled = boolValue(false)
		if err := baseClient.Update(ctx, actualFirst); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := reconcile(first.Name); err != nil {
				t.Fatal(err)
			}
		}
		if err := baseClient.Get(ctx, firstKey, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
			t.Fatalf("disabled first deployment get error = %v, want NotFound", err)
		}
		secondKey := client.ObjectKey{Namespace: namespace.Name, Name: ksm.ManagedResourceName(second.Name)}
		if err := baseClient.Get(ctx, secondKey, &appsv1.Deployment{}); err != nil {
			t.Fatalf("second deployment was affected: %v", err)
		}
	})
}

func (c transitionReadFailingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*appsv1.Deployment); ok && key.Name == "ksm-switch-read-failure" {
		return errors.New("injected transition read failure")
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c existingRoleReadFailingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	err := c.Client.Get(ctx, key, object, options...)
	if err == nil {
		if _, ok := object.(*rbacv1.Role); ok && key.Name == "ksm-switch-rbac-read-failure" {
			return errors.New("injected RBAC read failure")
		}
	}
	return err
}

func (c existingStatefulSetReadFailingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	err := c.Client.Get(ctx, key, object, options...)
	if err == nil {
		if _, ok := object.(*appsv1.StatefulSet); ok && key.Name == "ksm-applied-workload-read-failure" {
			return errors.New("injected applied workload read failure")
		}
	}
	return err
}

func (c servicePreflightReadFailingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Service); ok && key.Name == "ksm-preflight-read-failure" {
		return errors.New("injected ownership preflight read failure")
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *secondServiceReadFailingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.Service); ok && key.Name == "ksm-terminating-read-failure" {
		c.reads++
		if c.reads == 2 {
			return errors.New("injected terminating resource read failure")
		}
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c deploymentObservationFailingClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*appsv1.Deployment); ok && key.Name == "ksm-problem-observation-failure" {
		return errors.New("injected problem-status observation failure")
	}
	return c.Client.Get(ctx, key, object, options...)
}

func TestPartialApplyKeepsOriginalNamespaceForDisableCleanup(t *testing.T) {
	t.Run("UT-KSM-GH53-004 部分写入失败保留成功资源并在重试后收敛", func(t *testing.T) {
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
		if degraded := findCondition(actual.Status.Conditions, "Degraded"); degraded.Status != metav1.ConditionTrue || degraded.Reason != ksm.ReasonApplyFailed {
			t.Fatalf("Degraded = %#v, want True/ApplyFailed", degraded)
		}

		// 后续调谐从已成功的 ServiceAccount 继续，不回滚它，并收敛其余四类资源。
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
		actual = getInstance(t, ctx, baseClient, instance.Name)
		if len(actual.Status.ManagedResources) != 5 || findCondition(actual.Status.Conditions, "Degraded").Status != metav1.ConditionFalse {
			t.Fatalf("recovered status = %#v", actual.Status)
		}
		if err := baseClient.Get(ctx, resourceKey, &corev1.ServiceAccount{}); err != nil {
			t.Fatalf("retained ServiceAccount missing after recovery: %v", err)
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
	})
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

func TestModeSwitchDeleteFailureReportsStage(t *testing.T) {
	t.Run("UT-KSM-GH56-003 模式切换删除失败报告当前阶段", func(t *testing.T) {
		// 准备已有 Deployment 的受管实例，并在切换到 StatefulSetSharded 的删除边界注入失败。
		ctx := context.Background()
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "switch-delete-failure", UID: types.UID("uid-switch-delete-failure")},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: "monitoring",
				Deployment: monitoringv1alpha1.KubeStateMetricsDeploymentSpec{
					Mode: monitoringv1alpha1.DeploymentModeStatefulSetSharded,
					StatefulSetSharded: &monitoringv1alpha1.KubeStateMetricsStatefulSetShardedSpec{
						Shards: 2,
					},
				},
			},
		}
		controllerOwner := true
		deployment := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ksm-switch-delete-failure", Namespace: "monitoring",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: monitoringv1alpha1.GroupVersion.String(), Kind: "KubeStateMetrics",
					Name: instance.Name, UID: instance.UID, Controller: &controllerOwner,
				}},
			},
			Spec:   appsv1.DeploymentSpec{Replicas: int32Value(1)},
			Status: appsv1.DeploymentStatus{AvailableReplicas: 1},
		}
		baseClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(instance, deployment, &appsv1.StatefulSet{}).
			WithObjects(instance, deployment, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}).
			Build()
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client: deleteFailingClient{Client: baseClient}, ServerVersion: fixedServerVersion{},
		}

		// 执行切换，验证失败 status 保留切换阶段、实际旧 workload 可用性与删除原因。
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}})
		if err == nil || !strings.Contains(err.Error(), "injected delete failure") {
			t.Fatalf("reconcile error = %v, want injected delete failure", err)
		}
		actual := getInstance(t, ctx, baseClient, instance.Name)
		progressing := findCondition(actual.Status.Conditions, "Progressing")
		if progressing.Status != metav1.ConditionTrue || progressing.Reason != ksm.ReasonSwitchingDeploymentMode {
			t.Fatalf("Progressing = %#v, want True/%s", progressing, ksm.ReasonSwitchingDeploymentMode)
		}
		degraded := findCondition(actual.Status.Conditions, "Degraded")
		if degraded.Status != metav1.ConditionTrue || degraded.Reason != ksm.ReasonDeleteFailed || !strings.Contains(degraded.Message, "删除旧 workload") {
			t.Fatalf("Degraded = %#v, want True/DeleteFailed with transition stage", degraded)
		}
		if actual.Status.DeploymentMode != monitoringv1alpha1.DeploymentModeDeployment || actual.Status.DesiredPods != 1 || actual.Status.AvailablePods != 1 {
			t.Fatalf("status workload = mode %s desired %d available %d, want existing Deployment 1/1",
				actual.Status.DeploymentMode, actual.Status.DesiredPods, actual.Status.AvailablePods)
		}
		if err := baseClient.Get(ctx, client.ObjectKey{Namespace: "monitoring", Name: "ksm-switch-delete-failure"}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
			t.Fatalf("StatefulSet get error = %v, want NotFound", err)
		}
	})
}

func TestModeSwitchReadFailureReportsStage(t *testing.T) {
	t.Run("UT-KSM-GH56-004 模式切换读取失败报告当前阶段", func(t *testing.T) {
		// 准备一个从已观测 Deployment 切换到 StatefulSetSharded 的实例，在旧 workload 读取边界注入失败。
		ctx := context.Background()
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "switch-read-failure", UID: types.UID("uid-switch-read-failure"), Generation: 2},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: "monitoring",
				Deployment: monitoringv1alpha1.KubeStateMetricsDeploymentSpec{
					Mode: monitoringv1alpha1.DeploymentModeStatefulSetSharded,
					StatefulSetSharded: &monitoringv1alpha1.KubeStateMetricsStatefulSetShardedSpec{
						Shards: 2,
					},
				},
			},
			Status: monitoringv1alpha1.KubeStateMetricsStatus{
				ObservedGeneration:     1,
				AppliedTargetNamespace: "monitoring",
				DeploymentMode:         monitoringv1alpha1.DeploymentModeDeployment,
				DesiredPods:            1,
				AvailablePods:          1,
			},
		}
		baseClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(instance, &appsv1.Deployment{}, &appsv1.StatefulSet{}).
			WithObjects(instance, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}).
			Build()
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client: transitionReadFailingClient{Client: baseClient}, ServerVersion: fixedServerVersion{},
		}

		// 执行切换并验证不能观测旧 workload 时仍保留上次事实，同时暴露切换阶段与读取故障。
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}})
		if err == nil || !strings.Contains(err.Error(), "injected transition read failure") {
			t.Fatalf("reconcile error = %v, want injected transition read failure", err)
		}
		actual := getInstance(t, ctx, baseClient, instance.Name)
		progressing := findCondition(actual.Status.Conditions, "Progressing")
		if progressing.Status != metav1.ConditionTrue || progressing.Reason != ksm.ReasonSwitchingDeploymentMode || !strings.Contains(progressing.Message, "读取旧 workload") {
			t.Fatalf("Progressing = %#v, want switching observation failure", progressing)
		}
		degraded := findCondition(actual.Status.Conditions, "Degraded")
		if degraded.Status != metav1.ConditionTrue || degraded.Reason != ksm.ReasonObservationFailed {
			t.Fatalf("Degraded = %#v, want True/%s", degraded, ksm.ReasonObservationFailed)
		}
		if actual.Status.ObservedGeneration != actual.Generation || actual.Status.DeploymentMode != monitoringv1alpha1.DeploymentModeDeployment || actual.Status.AvailablePods != 1 {
			t.Fatalf("status = %#v, want current generation with last observed Deployment availability", actual.Status)
		}
		if err := baseClient.Get(ctx, client.ObjectKey{Namespace: "monitoring", Name: "ksm-switch-read-failure"}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
			t.Fatalf("StatefulSet get error = %v, want NotFound", err)
		}
	})
}

func TestModeSwitchRBACReadFailureReportsStage(t *testing.T) {
	t.Run("UT-KSM-GH56-005 切换清理 RBAC 读取失败报告当前阶段", func(t *testing.T) {
		// 准备已应用的分片状态和受管 Role，在切回 Deployment 的清理预检注入读取失败。
		ctx := context.Background()
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "switch-rbac-read-failure", UID: types.UID("uid-switch-rbac-read-failure"), Generation: 2},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: "monitoring"},
			Status: monitoringv1alpha1.KubeStateMetricsStatus{
				ObservedGeneration: 1, AppliedTargetNamespace: "monitoring",
				DeploymentMode: monitoringv1alpha1.DeploymentModeStatefulSetSharded,
				DesiredPods:    2, AvailablePods: 2,
			},
		}
		controllerOwner := true
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{
			Name: "ksm-switch-rbac-read-failure", Namespace: "monitoring",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: monitoringv1alpha1.GroupVersion.String(), Kind: "KubeStateMetrics",
				Name: instance.Name, UID: instance.UID, Controller: &controllerOwner,
			}},
		}}
		baseClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(instance, &appsv1.Deployment{}, &appsv1.StatefulSet{}).
			WithObjects(instance, role, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}).
			Build()
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client: existingRoleReadFailingClient{Client: baseClient}, ServerVersion: fixedServerVersion{},
		}

		// 执行切换并验证清理预检失败也能落盘切换阶段和观测故障。
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}})
		if err == nil || !strings.Contains(err.Error(), "injected RBAC read failure") {
			t.Fatalf("reconcile error = %v, want injected RBAC read failure", err)
		}
		actual := getInstance(t, ctx, baseClient, instance.Name)
		progressing := findCondition(actual.Status.Conditions, "Progressing")
		if progressing.Status != metav1.ConditionTrue || progressing.Reason != ksm.ReasonSwitchingDeploymentMode {
			t.Fatalf("Progressing = %#v, want True/%s", progressing, ksm.ReasonSwitchingDeploymentMode)
		}
		degraded := findCondition(actual.Status.Conditions, "Degraded")
		if degraded.Status != metav1.ConditionTrue || degraded.Reason != ksm.ReasonObservationFailed || !strings.Contains(degraded.Message, "injected RBAC read failure") {
			t.Fatalf("Degraded = %#v, want observation failure with RBAC read error", degraded)
		}
	})
}

func TestAppliedWorkloadReadFailureReportsStage(t *testing.T) {
	t.Run("UT-KSM-GH56-006 已写入 workload 读取失败报告当前阶段", func(t *testing.T) {
		// 准备全新分片实例，只在 StatefulSet 创建成功后的可用副本读取中注入失败。
		ctx := context.Background()
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: "applied-workload-read-failure", UID: types.UID("uid-applied-workload-read-failure"), Generation: 1},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: "monitoring",
				Deployment: monitoringv1alpha1.KubeStateMetricsDeploymentSpec{
					Mode:               monitoringv1alpha1.DeploymentModeStatefulSetSharded,
					StatefulSetSharded: &monitoringv1alpha1.KubeStateMetricsStatefulSetShardedSpec{Shards: 2},
				},
			},
		}
		baseClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(instance, &appsv1.Deployment{}, &appsv1.StatefulSet{}).
			WithObjects(instance, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}).
			Build()
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client: existingStatefulSetReadFailingClient{Client: baseClient}, ServerVersion: fixedServerVersion{},
		}

		// 执行调谐并验证写入后观测失败不会留下空白或成功 status。
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}})
		if err == nil || !strings.Contains(err.Error(), "injected applied workload read failure") {
			t.Fatalf("reconcile error = %v, want injected applied workload read failure", err)
		}
		actual := getInstance(t, ctx, baseClient, instance.Name)
		progressing := findCondition(actual.Status.Conditions, "Progressing")
		if progressing.Status != metav1.ConditionTrue || progressing.Reason != ksm.ReasonObservationFailed {
			t.Fatalf("Progressing = %#v, want True/%s", progressing, ksm.ReasonObservationFailed)
		}
		degraded := findCondition(actual.Status.Conditions, "Degraded")
		if degraded.Status != metav1.ConditionTrue || degraded.Reason != ksm.ReasonObservationFailed || !strings.Contains(degraded.Message, "injected applied workload read failure") {
			t.Fatalf("Degraded = %#v, want observation failure with applied workload read error", degraded)
		}
	})
}

func TestOwnershipPreflightReadFailureReportsStage(t *testing.T) {
	t.Run("UT-KSM-GH56-007 topology 所有权预检读取失败报告当前阶段", func(t *testing.T) {
		// 准备从 Deployment 切换的声明状态，在任何写入之前的 Service 所有权预检中注入失败。
		ctx := context.Background()
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		instance := shardedFailureTestInstance("preflight-read-failure")
		baseClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(instance, &appsv1.Deployment{}, &appsv1.StatefulSet{}).
			WithObjects(instance, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}).
			Build()
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client: servicePreflightReadFailingClient{Client: baseClient}, ServerVersion: fixedServerVersion{},
		}

		// 执行切换并验证预检读取失败会落盘切换阶段，不会留下上一 generation。
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}})
		assertGH56ObservationFailure(t, ctx, baseClient, instance, err, "injected ownership preflight read failure")
	})
}

func TestTerminatingResourceReadFailureReportsStage(t *testing.T) {
	t.Run("UT-KSM-GH56-008 终止中 topology 读取失败报告当前阶段", func(t *testing.T) {
		// 准备从 Deployment 切换的声明状态，让 Service 所有权预检成功后在终止观测中注入失败。
		ctx := context.Background()
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		instance := shardedFailureTestInstance("terminating-read-failure")
		baseClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(instance, &appsv1.Deployment{}, &appsv1.StatefulSet{}).
			WithObjects(instance, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}).
			Build()
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client: &secondServiceReadFailingClient{Client: baseClient}, ServerVersion: fixedServerVersion{},
		}

		// 执行切换并验证终止中资源读取失败同样会落盘切换阶段。
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}})
		assertGH56ObservationFailure(t, ctx, baseClient, instance, err, "injected terminating resource read failure")
	})
}

func TestProblemStatusObservationFailureReportsStage(t *testing.T) {
	t.Run("UT-KSM-GH56-009 写入问题 status 时 topology 观测失败报告当前阶段", func(t *testing.T) {
		// 准备从 Deployment 切换的声明状态和一个外部同名 Service，再对问题 status 的实际 topology 观测注入失败。
		ctx := context.Background()
		scheme := runtime.NewScheme()
		if err := clientgoscheme.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		if err := monitoringv1alpha1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		instance := shardedFailureTestInstance("problem-observation-failure")
		foreignService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Name: "ksm-problem-observation-failure", Namespace: "monitoring",
		}}
		baseClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(instance, &appsv1.Deployment{}, &appsv1.StatefulSet{}).
			WithObjects(instance, foreignService, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}}).
			Build()
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client: deploymentObservationFailingClient{Client: baseClient}, ServerVersion: fixedServerVersion{},
		}

		// 执行切换并验证冲突 status 所需的二次观测失败会替换未落盘的 problem conditions。
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: instance.Name}})
		assertGH56ObservationFailure(t, ctx, baseClient, instance, err, "injected problem-status observation failure")
		if getErr := baseClient.Get(ctx, client.ObjectKey{Namespace: "monitoring", Name: "ksm-problem-observation-failure"}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(getErr) {
			t.Fatalf("StatefulSet get error = %v, want NotFound", getErr)
		}
	})
}

func shardedFailureTestInstance(name string) *monitoringv1alpha1.KubeStateMetrics {
	return &monitoringv1alpha1.KubeStateMetrics{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Generation: 2},
		Spec: monitoringv1alpha1.KubeStateMetricsSpec{
			TargetNamespace: "monitoring",
			Deployment: monitoringv1alpha1.KubeStateMetricsDeploymentSpec{
				Mode:               monitoringv1alpha1.DeploymentModeStatefulSetSharded,
				StatefulSetSharded: &monitoringv1alpha1.KubeStateMetricsStatefulSetShardedSpec{Shards: 2},
			},
		},
		Status: monitoringv1alpha1.KubeStateMetricsStatus{
			ObservedGeneration: 1, AppliedTargetNamespace: "monitoring",
			DeploymentMode: monitoringv1alpha1.DeploymentModeDeployment,
			DesiredPods:    1, AvailablePods: 1,
		},
	}
}

func assertGH56ObservationFailure(t *testing.T, ctx context.Context, baseClient client.Client, instance *monitoringv1alpha1.KubeStateMetrics, reconcileErr error, expectedError string) {
	t.Helper()
	if reconcileErr == nil || !strings.Contains(reconcileErr.Error(), expectedError) {
		t.Fatalf("reconcile error = %v, want %s", reconcileErr, expectedError)
	}
	actual := getInstance(t, ctx, baseClient, instance.Name)
	progressing := findCondition(actual.Status.Conditions, "Progressing")
	if progressing.Status != metav1.ConditionTrue || progressing.Reason != ksm.ReasonSwitchingDeploymentMode {
		t.Fatalf("Progressing = %#v, want True/%s", progressing, ksm.ReasonSwitchingDeploymentMode)
	}
	degraded := findCondition(actual.Status.Conditions, "Degraded")
	if degraded.Status != metav1.ConditionTrue || degraded.Reason != ksm.ReasonObservationFailed || !strings.Contains(degraded.Message, expectedError) {
		t.Fatalf("Degraded = %#v, want observation failure containing %q", degraded, expectedError)
	}
	if actual.Status.ObservedGeneration != actual.Generation {
		t.Fatalf("observedGeneration = %d, want %d", actual.Status.ObservedGeneration, actual.Generation)
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

func boolValue(value bool) *bool    { return &value }
func int32Value(value int32) *int32 { return &value }

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

func ownerUIDExists(owners []metav1.OwnerReference, wanted types.UID) bool {
	for _, owner := range owners {
		if owner.UID == wanted {
			return true
		}
	}
	return false
}
