package controller_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksmcontroller "github.com/fjsxy/monitor-in-hand/internal/controller"
	ksm "github.com/fjsxy/monitor-in-hand/internal/kubestatemetrics"
)

type fixedServerVersion struct{ gitVersion string }

type serviceCreateAfterWriteErrorClient struct {
	client.Client
	failed bool
}

func (f fixedServerVersion) ServerVersion() (*version.Info, error) {
	return &version.Info{GitVersion: f.gitVersion}, nil
}

func (c *serviceCreateAfterWriteErrorClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if _, ok := object.(*corev1.Service); ok && !c.failed {
		c.failed = true
		if err := c.Client.Create(ctx, object, options...); err != nil {
			return err
		}
		return fmt.Errorf("injected ambiguous service apply failure")
	}
	return c.Client.Create(ctx, object, options...)
}

var _ = Describe("KubeStateMetrics reconciliation", func() {
	ctx := context.Background()
	var suffix string

	BeforeEach(func() {
		suffix = fmt.Sprintf("%d", GinkgoRandomSeed()+int64(CurrentSpecReport().LeafNodeLocation.LineNumber))
	})

	reconcile := func(name, gitVersion string) ctrl.Result {
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client:        testClient,
			ServerVersion: fixedServerVersion{gitVersion: gitVersion},
		}
		result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	DescribeTable("IT-KSM-005 在 admission 阶段拒绝缺少必填声明的 CR",
		func(object map[string]any) {
			instance := &unstructured.Unstructured{Object: object}
			instance.SetAPIVersion("monitoring.fjsxy.github.io/v1alpha1")
			instance.SetKind("KubeStateMetrics")
			instance.SetName("invalid-" + suffix)

			err := testClient.Create(ctx, instance)
			Expect(err).To(HaveOccurred())
			Expect(apierrors.IsInvalid(err)).To(BeTrue())
		},
		Entry("整个 spec 缺失", map[string]any{}),
		Entry("spec 缺少 targetNamespace", map[string]any{"spec": map[string]any{}}),
		Entry("collectors mode 不在稳定枚举内", map[string]any{"spec": map[string]any{
			"targetNamespace": "default", "collectors": map[string]any{"mode": "Everything"},
		}}),
		Entry("metricFilter mode 不在稳定枚举内", map[string]any{"spec": map[string]any{
			"targetNamespace": "default", "metricFilter": map[string]any{"mode": "RawArgs"},
		}}),
	)

	DescribeTable("IT-KSM-001 创建默认 Deployment 受管实例并从真实 workload status 报告可用性", func(gitVersion, wantVersion, caseSuffix string) {
		By("准备最小受管实例声明")
		namespace := "it-ksm-success-" + caseSuffix + "-" + suffix
		name := "success-" + caseSuffix + "-" + suffix
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())

		By("执行目标 profile 的首次调谐")
		reconcile(name, gitVersion)

		By("验证受管资源符合默认配置")
		resourceName := "ksm-" + name
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))
		Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElements(
			HavePrefix("--resources="),
			SatisfyAll(HavePrefix("--namespaces="), ContainSubstring(namespace)),
		))
		Expect(deployment.Spec.Template.Spec.Containers[0].Resources).To(Equal(corev1.ResourceRequirements{}))
		Expect(deployment.OwnerReferences).To(ContainElement(HaveField("UID", instance.UID)))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.Service{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.ServiceAccount{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRoleBinding{})).To(Succeed())

		By("验证 API 默认值、status 和 effective collectors")
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Spec.Enabled).NotTo(BeNil())
		Expect(*actual.Spec.Enabled).To(BeTrue())
		Expect(actual.Spec.Suspended).NotTo(BeNil())
		Expect(*actual.Spec.Suspended).To(BeFalse())
		Expect(actual.Spec.Deployment.Mode).To(Equal(monitoringv1alpha1.DeploymentModeDeployment))
		Expect(actual.Spec.Collectors.Mode).To(Equal(monitoringv1alpha1.CollectorModeDefault))
		Expect(actual.Spec.Collectors.Names).To(BeEmpty())
		Expect(actual.Spec.MetricFilter.Mode).To(Equal(monitoringv1alpha1.MetricFilterModeAll))
		Expect(actual.Spec.MetricFilter.Patterns).To(BeEmpty())
		Expect(actual.Spec.PodResources).To(BeNil())
		Expect(actual.Status).To(SatisfyAll(
			HaveField("ObservedGeneration", actual.Generation),
			HaveField("AppliedTargetNamespace", namespace),
			HaveField("KubeStateMetricsVersion", wantVersion),
			HaveField("DeploymentMode", monitoringv1alpha1.DeploymentModeDeployment),
			HaveField("DesiredPods", int32(1)),
			HaveField("AvailablePods", int32(0)),
			HaveField("EffectiveCollectors", HaveLen(28)),
			HaveField("ManagedResources", HaveLen(5)),
		))
		Expect(condition(actual, "Available").Status).To(Equal(metav1.ConditionFalse))
		By("重复调谐并确认已收敛 Deployment 不被写入")
		resourceVersion := deployment.ResourceVersion
		reconcile(name, gitVersion)
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		Expect(deployment.ResourceVersion).To(Equal(resourceVersion), "已收敛的 Deployment 不应被重复写入")

		By("模拟 workload 可用并验证 Ready status")
		deployment.Status.Replicas = 1
		deployment.Status.ReadyReplicas = 1
		deployment.Status.AvailableReplicas = 1
		Expect(testClient.Status().Update(ctx, deployment)).To(Succeed())
		reconcile(name, gitVersion)
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.AvailablePods).To(Equal(int32(1)))
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseReady))
		Expect(condition(actual, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(actual, "Progressing").Status).To(Equal(metav1.ConditionFalse))
		Expect(condition(actual, "Degraded").Status).To(Equal(metav1.ConditionFalse))
		Expect(condition(actual, "Available").Status).To(Equal(metav1.ConditionTrue))
	},
		Entry("Kubernetes 1.19 profile", "v1.19.16", "2.3.0", "119"),
		Entry("Kubernetes 1.28 profile", "v1.28.15", "2.11.0", "128"),
	)

	DescribeTable("IT-KSM-002 在前置条件失败时只更新 status 且零写入",
		func(gitVersion string, createNamespace bool, wantReason string) {
			namespace := "it-ksm-rejected-" + suffix
			name := "rejected-" + suffix
			if createNamespace {
				Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
			}
			instance := &monitoringv1alpha1.KubeStateMetrics{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
			}
			Expect(testClient.Create(ctx, instance)).To(Succeed())

			reconcile(name, gitVersion)
			actual := &monitoringv1alpha1.KubeStateMetrics{}
			Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
			Expect(actual.Status.ObservedGeneration).To(Equal(actual.Generation))
			Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseDegraded))
			Expect(condition(actual, "Degraded").Reason).To(Equal(wantReason))
			Expect(actual.Status.ManagedResources).To(BeEmpty())
			Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "ksm-" + name}, &appsv1.Deployment{})).To(MatchError(ContainSubstring("not found")))
			Expect(testClient.Get(ctx, client.ObjectKey{Name: "ksm-" + name}, &rbacv1.ClusterRole{})).To(MatchError(ContainSubstring("not found")))
		},
		Entry("未知 Kubernetes minor", "v1.27.9", true, "UnsupportedKubernetesVersion"),
		Entry("目标 Namespace 缺失", "v1.28.15", false, "DependencyMissing"),
	)

	It("IT-KSM-003 首次成功后拒绝修改 targetNamespace", func() {
		originalNamespace := "it-ksm-original-" + suffix
		newNamespace := "it-ksm-new-" + suffix
		name := "immutable-" + suffix
		for _, namespace := range []string{originalNamespace, newNamespace} {
			Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		}
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: originalNamespace},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")

		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		actual.Spec.TargetNamespace = newNamespace
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, "v1.28.15")

		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.AppliedTargetNamespace).To(Equal(originalNamespace))
		Expect(condition(actual, "Degraded").Reason).To(Equal("InvalidSpec"))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: newNamespace, Name: "ksm-" + name}, &appsv1.Deployment{})).To(MatchError(ContainSubstring("not found")))
	})

	It("IT-KSM-004 同名非受管资源触发 ResourceConflict 且所有身份零写入", func() {
		namespace := "it-ksm-conflict-" + suffix
		name := "conflict-" + suffix
		resourceName := "ksm-" + name
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		occupied := &rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Labels: map[string]string{"preserve": "true"}},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{"example.test"}, Resources: []string{"sentinels"}, Verbs: []string{"get"}}},
		}
		Expect(testClient.Create(ctx, occupied)).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())

		reconcile(name, "v1.28.15")
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(condition(actual, "Degraded").Reason).To(Equal("ResourceConflict"))
		Expect(actual.Status.ManagedResources).To(BeEmpty())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, occupied)).To(Succeed())
		Expect(occupied.Labels).To(Equal(map[string]string{"preserve": "true"}))
		Expect(occupied.Rules).To(ConsistOf(rbacv1.PolicyRule{APIGroups: []string{"example.test"}, Resources: []string{"sentinels"}, Verbs: []string{"get"}}))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.ServiceAccount{})).To(MatchError(ContainSubstring("not found")))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(MatchError(ContainSubstring("not found")))
	})

	It("IT-KSM-GH53-001 同一与不同 targetNamespace 的多实例保持资源、状态与生命周期隔离", func() {
		By("在共享和独立 Namespace 中准备三个不同业务身份的受管实例")
		sharedNamespace := "it-gh53-shared-" + suffix
		isolatedNamespace := "it-gh53-isolated-" + suffix
		for _, namespace := range []string{sharedNamespace, isolatedNamespace} {
			Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		}
		instances := []*monitoringv1alpha1.KubeStateMetrics{
			{ObjectMeta: metav1.ObjectMeta{Name: "gh53-shared-a-" + suffix}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: sharedNamespace}},
			{ObjectMeta: metav1.ObjectMeta{Name: "gh53-shared-b-" + suffix}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: sharedNamespace}},
			{ObjectMeta: metav1.ObjectMeta{Name: "gh53-isolated-" + suffix}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: isolatedNamespace}},
		}
		for _, instance := range instances {
			Expect(testClient.Create(ctx, instance)).To(Succeed())
			reconcile(instance.Name, "v1.28.15")
		}

		By("验证每个实例拥有独立 workload、Service endpoints 选择身份、RBAC 和 status")
		for _, instance := range instances {
			resourceName := ksm.ManagedResourceName(instance.Name)
			deployment := &appsv1.Deployment{}
			Expect(testClient.Get(ctx, client.ObjectKey{Namespace: instance.Spec.TargetNamespace, Name: resourceName}, deployment)).To(Succeed())
			Expect(deployment.OwnerReferences).To(ContainElement(HaveField("UID", instance.UID)))
			expectStableManagedLabels(deployment.Labels, instance.Name, "2.11.0")
			expectStableManagedLabels(deployment.Spec.Template.Labels, instance.Name, "2.11.0")
			service := &corev1.Service{}
			Expect(testClient.Get(ctx, client.ObjectKey{Namespace: instance.Spec.TargetNamespace, Name: resourceName}, service)).To(Succeed())
			expectStableManagedLabels(service.Labels, instance.Name, "2.11.0")
			Expect(service.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/name":         "kube-state-metrics",
				"app.kubernetes.io/instance":     instance.Name,
				"monitor-in-hand/endpoint-group": "default",
			}))
			serviceAccount := &corev1.ServiceAccount{}
			Expect(testClient.Get(ctx, client.ObjectKey{Namespace: instance.Spec.TargetNamespace, Name: resourceName}, serviceAccount)).To(Succeed())
			expectStableManagedLabels(serviceAccount.Labels, instance.Name, "2.11.0")
			clusterRole := &rbacv1.ClusterRole{}
			Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, clusterRole)).To(Succeed())
			expectStableManagedLabels(clusterRole.Labels, instance.Name, "2.11.0")
			clusterRoleBinding := &rbacv1.ClusterRoleBinding{}
			Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, clusterRoleBinding)).To(Succeed())
			expectStableManagedLabels(clusterRoleBinding.Labels, instance.Name, "2.11.0")
			actual := &monitoringv1alpha1.KubeStateMetrics{}
			Expect(testClient.Get(ctx, client.ObjectKey{Name: instance.Name}, actual)).To(Succeed())
			Expect(actual.Status.ManagedResources).To(HaveLen(5))
		}

		By("停用共享 Namespace 中的一个实例，验证其他实例的资源和状态不受影响")
		first := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: instances[0].Name}, first)).To(Succeed())
		first.Spec.Enabled = boolPointer(false)
		Expect(testClient.Update(ctx, first)).To(Succeed())
		reconcile(first.Name, "v1.28.15")
		reconcile(first.Name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: first.Name}, first)).To(Succeed())
		Expect(first.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseDisabled))
		for _, sibling := range instances[1:] {
			resourceName := ksm.ManagedResourceName(sibling.Name)
			Expect(testClient.Get(ctx, client.ObjectKey{Namespace: sibling.Spec.TargetNamespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
			Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(Succeed())
			actual := &monitoringv1alpha1.KubeStateMetrics{}
			Expect(testClient.Get(ctx, client.ObjectKey{Name: sibling.Name}, actual)).To(Succeed())
			Expect(actual.Status.ManagedResources).To(HaveLen(5))
			Expect(condition(actual, "Degraded").Status).To(Equal(metav1.ConditionFalse))
		}
	})

	DescribeTable("IT-KSM-GH53-002 无 owner、错误 owner 与旧 CR UID 均报告 ResourceConflict 并可在冲突消失后恢复",
		func(ownerClass string) {
			By("准备当前 CR 和占用其期望 Service 身份的冲突对象")
			namespace := "it-gh53-conflict-" + ownerClass + "-" + suffix
			name := "gh53-conflict-" + ownerClass + "-" + suffix
			resourceName := ksm.ManagedResourceName(name)
			Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
			instance := &monitoringv1alpha1.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace}}
			Expect(testClient.Create(ctx, instance)).To(Succeed())
			owners := []metav1.OwnerReference(nil)
			controllerOwner := true
			switch ownerClass {
			case "wrong":
				owners = []metav1.OwnerReference{{APIVersion: monitoringv1alpha1.GroupVersion.String(), Kind: "KubeStateMetrics", Name: "another-instance", UID: types.UID("uid-another-instance"), Controller: &controllerOwner}}
			case "old":
				owners = []metav1.OwnerReference{{APIVersion: monitoringv1alpha1.GroupVersion.String(), Kind: "KubeStateMetrics", Name: name, UID: types.UID("uid-from-deleted-cr"), Controller: &controllerOwner}}
			}
			occupied := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: namespace, Annotations: map[string]string{"preserve": ownerClass}, OwnerReferences: owners}, Spec: corev1.ServiceSpec{Selector: map[string]string{"preserve": "true"}, Ports: []corev1.ServicePort{{Name: "sentinel", Port: 9090}}}}
			Expect(testClient.Create(ctx, occupied)).To(Succeed())
			beforeResourceVersion := occupied.ResourceVersion

			By("调谐并验证冲突对象未被修改、删除或接管，其余资源零写入")
			reconcile(name, "v1.28.15")
			actual := &monitoringv1alpha1.KubeStateMetrics{}
			Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
			Expect(condition(actual, "Degraded")).To(SatisfyAll(HaveField("Status", metav1.ConditionTrue), HaveField("Reason", ksm.ReasonResourceConflict)))
			Expect(testClient.Get(ctx, client.ObjectKeyFromObject(occupied), occupied)).To(Succeed())
			Expect(occupied.ResourceVersion).To(Equal(beforeResourceVersion))
			Expect(occupied.Annotations).To(HaveKeyWithValue("preserve", ownerClass))
			Expect(occupied.Spec.Selector).To(Equal(map[string]string{"preserve": "true"}))
			Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.ServiceAccount{})).To(Satisfy(apierrors.IsNotFound))
			Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(Satisfy(apierrors.IsNotFound))

			By("移除冲突对象后重试，验证当前实例创建全部直接受管资源")
			Expect(testClient.Delete(ctx, occupied)).To(Succeed())
			reconcile(name, "v1.28.15")
			Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
			Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
			Expect(actual.Status.ManagedResources).To(HaveLen(5))
			Expect(condition(actual, "Degraded").Status).To(Equal(metav1.ConditionFalse))
		},
		Entry("无 ownerReference", "none"),
		Entry("其他实例 owner", "wrong"),
		Entry("同名已删除 CR 的旧 UID", "old"),
	)

	It("IT-KSM-GH53-003 finalizer 只清理当前 CR UID 的直接受管资源并忽略 managedResources 污染", func() {
		By("准备已收敛实例，将 Deployment 改为同名旧 UID owner 并向 status 注入非受管引用")
		namespace := "it-gh53-finalizer-" + suffix
		name := "gh53-finalizer-" + suffix
		resourceName := ksm.ManagedResourceName(name)
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace}}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		controllerOwner := true
		deployment.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: monitoringv1alpha1.GroupVersion.String(), Kind: "KubeStateMetrics", Name: name,
			UID: types.UID("uid-from-deleted-cr"), Controller: &controllerOwner,
		}}
		deployment.Annotations = map[string]string{"preserve": "old-uid"}
		Expect(testClient.Update(ctx, deployment)).To(Succeed())
		unrelated := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "gh53-status-only-" + suffix, Namespace: namespace}}
		Expect(testClient.Create(ctx, unrelated)).To(Succeed())
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		actual.Status.ManagedResources = append(actual.Status.ManagedResources, monitoringv1alpha1.ManagedResourceReference{APIVersion: "v1", Kind: "ConfigMap", Namespace: namespace, Name: unrelated.Name})
		Expect(testClient.Status().Update(ctx, actual)).To(Succeed())

		By("删除 CR 并调谐到 finalizer 完成")
		Expect(testClient.Delete(ctx, actual)).To(Succeed())
		reconcile(name, "version-must-not-be-read")
		reconcile(name, "version-must-not-be-read")

		By("验证当前 UID 的四类资源被清理，旧 UID 对象与 status-only 对象保留")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Satisfy(apierrors.IsNotFound))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		Expect(deployment.Annotations).To(HaveKeyWithValue("preserve", "old-uid"))
		Expect(testClient.Get(ctx, client.ObjectKeyFromObject(unrelated), unrelated)).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.Service{})).To(Satisfy(apierrors.IsNotFound))
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(Satisfy(apierrors.IsNotFound))
	})

	It("IT-KSM-GH53-004 多资源写入部分失败时保留已写入资源并在后续调谐继续收敛", func() {
		By("使用真实 API Server 准备实例，在 Service 已持久化后注入一次模糊写入错误")
		namespace := "it-gh53-partial-" + suffix
		name := "gh53-partial-" + suffix
		resourceName := ksm.ManagedResourceName(name)
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace}}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		failingClient := &serviceCreateAfterWriteErrorClient{Client: testClient}
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{Client: failingClient, ServerVersion: fixedServerVersion{gitVersion: "v1.28.15"}}
		request := ctrl.Request{NamespacedName: types.NamespacedName{Name: name}}
		_, err := reconciler.Reconcile(ctx, request)
		Expect(err).To(MatchError(ContainSubstring("injected ambiguous service apply failure")))

		By("验证失败前与模糊成功的资源保留，未执行的 Deployment 不存在且 status 报告 ApplyFailed")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.ServiceAccount{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRoleBinding{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.Service{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(Satisfy(apierrors.IsNotFound))
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(condition(actual, "Degraded")).To(SatisfyAll(HaveField("Status", metav1.ConditionTrue), HaveField("Reason", ksm.ReasonApplyFailed)))

		By("重试同一 generation，验证不回滚已正确写入的资源并补齐 Deployment")
		service := &corev1.Service{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, service)).To(Succeed())
		serviceUID := service.UID
		_, err = reconciler.Reconcile(ctx, request)
		Expect(err).NotTo(HaveOccurred())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, service)).To(Succeed())
		Expect(service.UID).To(Equal(serviceUID))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.ManagedResources).To(HaveLen(5))
		Expect(condition(actual, "Degraded").Status).To(Equal(metav1.ConditionFalse))
	})

	It("IT-KSM-GH53-005 watch 直接受管资源并及时修复修改与删除漂移", func() {
		By("启动真实 manager 与全部直接受管资源 watch")
		manager, err := ctrl.NewManager(testConfig, ctrl.Options{Scheme: testScheme, Metrics: server.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
		Expect(err).NotTo(HaveOccurred())
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{Client: manager.GetClient(), ServerVersion: fixedServerVersion{gitVersion: "v1.28.15"}}
		Expect(reconciler.SetupWithManager(manager)).To(Succeed())
		managerContext, cancelManager := context.WithCancel(ctx)
		managerDone := make(chan error, 1)
		go func() { managerDone <- manager.Start(managerContext) }()
		Expect(manager.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
		DeferCleanup(func() {
			cancelManager()
			Eventually(managerDone, 5*time.Second).Should(Receive(BeNil()))
		})

		By("创建实例并等待 manager 自动创建资源")
		namespace := "it-gh53-watch-" + suffix
		name := "gh53-watch-" + suffix
		resourceName := ksm.ManagedResourceName(name)
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace}}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
			g.Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.Service{})).To(Succeed())
		}, 5*time.Second).Should(Succeed())

		By("修改已拥有 Service 的可变字段，验证 owner watch 自动恢复声明状态")
		service := &corev1.Service{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, service)).To(Succeed())
		service.Labels = map[string]string{"drifted": "true"}
		service.Spec.Selector = map[string]string{"drifted": "true"}
		Expect(testClient.Update(ctx, service)).To(Succeed())
		Eventually(func(g Gomega) {
			current := &corev1.Service{}
			g.Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, current)).To(Succeed())
			g.Expect(current.Labels).To(HaveKeyWithValue("app.kubernetes.io/managed-by", "monitor-in-hand"))
			g.Expect(current.Labels).NotTo(HaveKey("drifted"))
			g.Expect(current.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/name":         "kube-state-metrics",
				"app.kubernetes.io/instance":     name,
				"monitor-in-hand/endpoint-group": "default",
			}))
		}, 5*time.Second).Should(Succeed())

		By("删除已拥有 Deployment，验证 delete watch 以新 UID 重建同一稳定身份")
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		oldUID := deployment.UID
		Expect(testClient.Delete(ctx, deployment)).To(Succeed())
		Eventually(func(g Gomega) {
			current := &appsv1.Deployment{}
			g.Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, current)).To(Succeed())
			g.Expect(current.UID).NotTo(Equal(oldUID))
			g.Expect(current.OwnerReferences).To(ContainElement(HaveField("UID", instance.UID)))
		}, 5*time.Second).Should(Succeed())
	})

	DescribeTable("IT-KSM-010 将 collectors、metric filter 与 podResources 贯通到 workload 和 status", func(gitVersion, caseSuffix string) {
		By("准备一份包含显式 collectors、allow filter 和容器资源的声明")
		namespace := "it-ksm-config-" + caseSuffix + "-" + suffix
		name := "config-" + caseSuffix + "-" + suffix
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		resources := &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
		}
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: namespace,
				Collectors: monitoringv1alpha1.KubeStateMetricsCollectors{
					Mode: monitoringv1alpha1.CollectorModeInclude, Names: []string{"pods", "deployments"},
				},
				MetricFilter: monitoringv1alpha1.KubeStateMetricsMetricFilter{
					Mode: monitoringv1alpha1.MetricFilterModeAllow, Patterns: []string{"pod_info"},
				},
				PodResources: resources,
			},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())

		By("执行显式配置的调谐")
		reconcile(name, gitVersion)

		By("通过受管 Deployment 验证实际参数和资源")
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "ksm-" + name}, deployment)).To(Succeed())
		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Args).To(ContainElements("--resources=pods,deployments", "--metric-allowlist=pod_info"))
		Expect(container.Resources).To(Equal(*resources))

		By("验证 status 使用同一份解析后的 effective collectors")
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.EffectiveCollectors).To(Equal([]string{"pods", "deployments"}))

		By("将已应用实例改为非法配置，并确认既有 workload 与 effective status 保持一致")
		deploymentResourceVersion := deployment.ResourceVersion
		deploymentArgs := append([]string(nil), deployment.Spec.Template.Spec.Containers[0].Args...)
		actual.Spec.MetricFilter.Patterns = []string{"["}
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, gitVersion)
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "ksm-" + name}, deployment)).To(Succeed())
		Expect(deployment.ResourceVersion).To(Equal(deploymentResourceVersion))
		Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(Equal(deploymentArgs))
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(condition(actual, "Degraded").Reason).To(Equal(ksm.ReasonInvalidSpec))
		Expect(actual.Status.EffectiveCollectors).To(Equal([]string{"pods", "deployments"}))
	},
		Entry("Kubernetes 1.19 profile", "v1.19.16", "119"),
		Entry("Kubernetes 1.28 profile", "v1.28.15", "128"),
	)

	It("IT-KSM-011 InvalidSpec 隔离当前实例并等待 generation 变化", func() {
		By("创建非法实例并确认调谐只写失败 status、不写资源也不定时重试")
		namespace := "it-ksm-invalid-" + suffix
		name := "invalid-config-" + suffix
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: namespace,
				MetricFilter: monitoringv1alpha1.KubeStateMetricsMetricFilter{
					Mode: monitoringv1alpha1.MetricFilterModeAllow, Patterns: []string{"["},
				},
			},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client: testClient, ServerVersion: fixedServerVersion{gitVersion: "v1.28.15"},
		}
		result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())

		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.ObservedGeneration).To(Equal(actual.Generation))
		Expect(condition(actual, "Degraded").Reason).To(Equal(ksm.ReasonInvalidSpec))
		Expect(actual.Status.ManagedResources).To(BeEmpty())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "ksm-" + name}, &appsv1.Deployment{})).To(MatchError(ContainSubstring("not found")))

		By("重复调谐相同 generation 并确认 status 未被反复写入")
		statusResourceVersion := actual.ResourceVersion
		result, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeZero())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.ResourceVersion).To(Equal(statusResourceVersion))

		By("调谐另一个合法实例，确认错误不会扩散到其他实例")
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		siblingName := "valid-sibling-" + suffix
		sibling := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: siblingName},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
		}
		Expect(testClient.Create(ctx, sibling)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: siblingName}})
		Expect(err).NotTo(HaveOccurred())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "ksm-" + siblingName}, &appsv1.Deployment{})).To(Succeed())

		By("修正非法实例以产生新 generation，并确认调谐恢复资源创建")
		actual.Spec.MetricFilter.Patterns = []string{"pod_info"}
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		_, err = reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "ksm-" + name}, &appsv1.Deployment{})).To(Succeed())
	})

	DescribeTable("IT-KSM-012 将 Deny metric filter 映射到两个版本的 workload", func(gitVersion, caseSuffix string) {
		By("准备显式 pods collector 和 Deny filter")
		namespace := "it-ksm-deny-" + caseSuffix + "-" + suffix
		name := "deny-" + caseSuffix + "-" + suffix
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: namespace,
				Collectors: monitoringv1alpha1.KubeStateMetricsCollectors{
					Mode: monitoringv1alpha1.CollectorModeInclude, Names: []string{"pods"},
				},
				MetricFilter: monitoringv1alpha1.KubeStateMetricsMetricFilter{
					Mode: monitoringv1alpha1.MetricFilterModeDeny, Patterns: []string{"kube_pod_info"},
				},
			},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())

		By("执行 Deny 配置的调谐")
		reconcile(name, gitVersion)

		By("验证 denylist 参数和 effective collectors")
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "ksm-" + name}, deployment)).To(Succeed())
		Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElements("--resources=pods", "--metric-denylist=kube_pod_info"))
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.EffectiveCollectors).To(Equal([]string{"pods"}))
	},
		Entry("Kubernetes 1.19 profile", "v1.19.16", "119"),
		Entry("Kubernetes 1.28 profile", "v1.28.15", "128"),
	)
	It("IT-KSM-006 停用优先于无效声明并在 Disabled 后按最新声明恢复", func() {
		By("准备一个已创建完整资源的受管实例")
		namespace := "it-ksm-disable-" + suffix
		name := "disable-" + suffix
		resourceName := "ksm-" + name
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")

		By("提交本会因 immutable targetNamespace 被拒绝的声明，同时停用实例")
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		actual.Spec.TargetNamespace = "missing-" + suffix
		actual.Spec.Enabled = boolPointer(false)
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, "invalid-version-that-must-not-be-read")

		By("确认先报告 Disabling，且五类直接受管资源均已收到删除")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseDisabling))
		Expect(condition(actual, "Progressing")).To(SatisfyAll(
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", "Disabling"),
		))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(MatchError(ContainSubstring("not found")))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.Service{})).To(MatchError(ContainSubstring("not found")))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.ServiceAccount{})).To(MatchError(ContainSubstring("not found")))
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(MatchError(ContainSubstring("not found")))
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRoleBinding{})).To(MatchError(ContainSubstring("not found")))

		By("再次调谐确认全部资源消失后进入 Disabled")
		reconcile(name, "still-not-a-kubernetes-version")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseDisabled))
		Expect(actual.Status.ManagedResources).To(BeEmpty())
		Expect(condition(actual, "Ready")).To(SatisfyAll(HaveField("Status", metav1.ConditionFalse), HaveField("Reason", "Disabled")))

		By("恢复有效的最新声明并重建完整受管资源")
		actual.Spec.TargetNamespace = namespace
		actual.Spec.Enabled = boolPointer(true)
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.ObservedGeneration).To(Equal(actual.Generation))
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseReconciling))
		Expect(actual.Status.ManagedResources).To(HaveLen(5))
	})

	It("IT-KSM-007 挂起只停止 workload 并在恢复后重建 Deployment", func() {
		By("准备一个已创建完整资源的受管实例")
		namespace := "it-ksm-suspend-" + suffix
		name := "suspend-" + suffix
		resourceName := "ksm-" + name
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")

		By("请求挂起并等待 workload controller 的删除得到确认")
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		actual.Spec.Suspended = boolPointer(true)
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, "v1.28.15")
		reconcile(name, "v1.28.15")

		By("验证 Deployment 消失而 Service、ServiceAccount 与 RBAC 全部保留")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(MatchError(ContainSubstring("not found")))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.Service{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.ServiceAccount{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRoleBinding{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseReady))
		Expect(actual.Status.ManagedResources).To(HaveLen(4))
		Expect(actual.Status.DesiredPods).To(BeZero())
		Expect(actual.Status.AvailablePods).To(BeZero())
		Expect(condition(actual, "Ready")).To(SatisfyAll(HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "Suspended")))
		Expect(condition(actual, "Available")).To(SatisfyAll(HaveField("Status", metav1.ConditionFalse), HaveField("Reason", "Suspended")))

		By("取消挂起并按当前 generation 重建 workload controller")
		actual.Spec.Suspended = boolPointer(false)
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.ObservedGeneration).To(Equal(actual.Generation))
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseReconciling))
		Expect(actual.Status.ManagedResources).To(HaveLen(5))
	})

	It("IT-KSM-008 删除时由 finalizer 阻塞到全部直接受管资源消失", func() {
		By("准备一个已创建完整资源的受管实例")
		namespace := "it-ksm-delete-" + suffix
		name := "delete-" + suffix
		resourceName := "ksm-" + name
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")

		By("在 workload 上设置测试 finalizer，制造一个可观察的删除阻塞")
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		deployment.Finalizers = append(deployment.Finalizers, "tests.monitor-in-hand/block-deletion")
		Expect(testClient.Update(ctx, deployment)).To(Succeed())

		By("先写入无效普通字段再删除 CR，验证 deletionTimestamp 拥有最高优先级")
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		actual.Spec.TargetNamespace = "missing-" + suffix
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		Expect(testClient.Delete(ctx, actual)).To(Succeed())
		reconcile(name, "invalid-version-that-must-not-be-read")

		By("确认 CR 与 controller finalizer 保留，phase 为 Deleting，且阻塞资源仍在终止中")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(actual.Finalizers).To(ContainElement("monitoring.fjsxy.github.io/kubestatemetrics-cleanup"))
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseDeleting))
		Expect(condition(actual, "Progressing")).To(SatisfyAll(HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "Deleting")))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		Expect(deployment.DeletionTimestamp.IsZero()).To(BeFalse())

		By("解除受管资源阻塞并再次调谐，确认 finalizer 完成清理后 CR 消失")
		deployment.Finalizers = nil
		Expect(testClient.Update(ctx, deployment)).To(Succeed())
		reconcile(name, "still-invalid-and-must-not-be-read")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Satisfy(apierrors.IsNotFound))
	})

	It("IT-KSM-009 停用删除发出后重新启用会先等待旧资源全部消失", func() {
		By("准备一个已创建完整资源的受管实例")
		namespace := "it-ksm-interrupt-" + suffix
		name := "interrupt-" + suffix
		resourceName := "ksm-" + name
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")

		By("用 Service finalizer 拉长停用删除窗口")
		service := &corev1.Service{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, service)).To(Succeed())
		service.Finalizers = append(service.Finalizers, "tests.monitor-in-hand/block-disable")
		Expect(testClient.Update(ctx, service)).To(Succeed())
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		actual.Spec.Enabled = boolPointer(false)
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, "v1.28.15")

		By("删除尚未完成时立刻重新启用，并确认不会与旧资源清理交错")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		actual.Spec.Enabled = boolPointer(true)
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseReconciling))
		Expect(condition(actual, "Progressing").Reason).To(Equal("WaitingForDeletion"))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, service)).To(Succeed())
		Expect(service.DeletionTimestamp.IsZero()).To(BeFalse())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(MatchError(ContainSubstring("not found")))
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(MatchError(ContainSubstring("not found")))

		By("解除旧资源阻塞后，仅按最新 generation 重建完整资源")
		service.Finalizers = nil
		Expect(testClient.Update(ctx, service)).To(Succeed())
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.ObservedGeneration).To(Equal(actual.Generation))
		Expect(actual.Status.ManagedResources).To(HaveLen(5))
	})

	It("IT-KSM-013 从集群 Namespace 解析统一采集范围并报告实际数量", func() {
		By("准备字面项、完整 regex 命中和 substring 干扰 Namespace")
		targetNamespace := "it-ksm-filter-target-" + suffix
		literalNamespace := "it-ksm-filter-literal-" + suffix
		matchedNamespace := "it-ksm-filter-team-a-" + suffix
		partialNamespace := "prefix-it-ksm-filter-team-a-" + suffix
		for _, namespace := range []string{targetNamespace, matchedNamespace, partialNamespace} {
			Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		}
		name := "filter-" + suffix
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: targetNamespace,
				NamespaceFilter: monitoringv1alpha1.NamespaceFilter{
					Mode: monitoringv1alpha1.NamespaceFilterModeInclude, Names: []string{literalNamespace, matchedNamespace},
					NamePatterns: []string{"it-ksm-filter-team-[ab]-" + suffix},
				},
			},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())

		By("执行调谐并验证展开参数与 status count 来自同一解析结果")
		reconcile(name, "v1.28.15")
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: "ksm-" + name}, deployment)).To(Succeed())
		Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--namespaces=" + literalNamespace + "," + matchedNamespace))
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.EffectiveNamespaceCount).To(Equal(int32(2)))
	})

	It("IT-KSM-014 在零匹配与恢复之间只收敛 workload", func() {
		By("准备初始有匹配项的受管实例")
		targetNamespace := "it-ksm-empty-target-" + suffix
		matchedNamespace := "it-ksm-empty-match-" + suffix
		for _, namespace := range []string{targetNamespace, matchedNamespace} {
			Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		}
		name := "empty-" + suffix
		resourceName := "ksm-" + name
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: monitoringv1alpha1.KubeStateMetricsSpec{
				TargetNamespace: targetNamespace,
				NamespaceFilter: monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, NamePatterns: []string{"it-ksm-empty-match-.*"}},
			},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())

		By("删除唯一匹配 Namespace 后进入稳定空集合")
		deleteNamespaceInEnvtest(ctx, matchedNamespace)
		reconcile(name, "v1.28.15")
		reconcile(name, "v1.28.15")
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status).To(SatisfyAll(
			HaveField("Phase", monitoringv1alpha1.KubeStateMetricsPhaseReady),
			HaveField("DesiredPods", int32(0)),
			HaveField("AvailablePods", int32(0)),
			HaveField("EffectiveNamespaceCount", int32(0)),
			HaveField("ManagedResources", HaveLen(4)),
		))
		Expect(condition(actual, "Ready")).To(SatisfyAll(HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "NoNamespaceMatched")))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, &appsv1.Deployment{})).To(Satisfy(apierrors.IsNotFound))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, &corev1.Service{})).To(Succeed())

		By("匹配 Namespace 出现后恢复 workload，再次归零后重新进入稳定空集合")
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: matchedNamespace}})).To(Succeed())
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
		deleteNamespaceInEnvtest(ctx, matchedNamespace)
		reconcile(name, "v1.28.15")
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, &appsv1.Deployment{})).To(Satisfy(apierrors.IsNotFound))
	})

	It("IT-KSM-015 非法 regex 只更新 InvalidSpec status 而不部分修改受管资源", func() {
		By("准备已成功下发资源的 All 模式实例")
		targetNamespace := "it-ksm-invalid-regex-target-" + suffix
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: targetNamespace}})).To(Succeed())
		name := "invalid-regex-" + suffix
		resourceName := "ksm-" + name
		instance := &monitoringv1alpha1.KubeStateMetrics{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: targetNamespace}}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")
		before := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, before)).To(Succeed())

		By("更新为非法 regex，并验证既有 workload 未被部分修改")
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		actual.Spec.NamespaceFilter = monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, NamePatterns: []string{"["}}
		Expect(testClient.Update(ctx, actual)).To(Succeed())
		reconcile(name, "v1.28.15")
		after := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, after)).To(Succeed())
		Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(condition(actual, "Degraded")).To(SatisfyAll(HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "InvalidSpec")))
	})

	It("IT-KSM-016 watch Namespace 并自动收敛 rollout、零匹配与恢复", func() {
		By("启动真实 manager 和 Namespace watch")
		manager, err := ctrl.NewManager(testConfig, ctrl.Options{Scheme: testScheme, Metrics: server.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
		Expect(err).NotTo(HaveOccurred())
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{Client: manager.GetClient(), ServerVersion: fixedServerVersion{gitVersion: "v1.28.15"}}
		Expect(reconciler.SetupWithManager(manager)).To(Succeed())
		managerContext, cancelManager := context.WithCancel(ctx)
		managerDone := make(chan error, 1)
		go func() { managerDone <- manager.Start(managerContext) }()
		Expect(manager.GetCache().WaitForCacheSync(ctx)).To(BeTrue())
		DeferCleanup(func() {
			By("停止测试 manager 并等待退出")
			cancelManager()
			Eventually(managerDone, 5*time.Second).Should(Receive(BeNil()))
		})

		By("准备初始零匹配的受管实例")
		targetNamespace := "it-ksm-watch-target-" + suffix
		firstNamespace := "it-ksm-watch-team-a-" + suffix
		secondNamespace := "it-ksm-watch-team-b-" + suffix
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: targetNamespace}})).To(Succeed())
		name := "watch-" + suffix
		resourceName := "ksm-" + name
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: targetNamespace, NamespaceFilter: monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, NamePatterns: []string{"it-ksm-watch-team-[ab]-" + suffix}}},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega) {
			actual := &monitoringv1alpha1.KubeStateMetrics{}
			g.Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
			g.Expect(apiMeta.FindStatusCondition(actual.Status.Conditions, "Ready")).To(HaveField("Reason", "NoNamespaceMatched"))
		}, 5*time.Second).Should(Succeed())

		By("新增匹配 Namespace 后由 watch 创建 workload，集合扩展触发 rollout")
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: firstNamespace}})).To(Succeed())
		firstGeneration := int64(0)
		Eventually(func(g Gomega) {
			deployment := &appsv1.Deployment{}
			g.Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, deployment)).To(Succeed())
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--namespaces=" + firstNamespace))
			firstGeneration = deployment.Generation
		}, 5*time.Second).Should(Succeed())
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: secondNamespace}})).To(Succeed())
		Eventually(func(g Gomega) {
			deployment := &appsv1.Deployment{}
			g.Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, deployment)).To(Succeed())
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--namespaces=" + firstNamespace + "," + secondNamespace))
			g.Expect(deployment.Generation).To(BeNumerically(">", firstGeneration))
		}, 5*time.Second).Should(Succeed())

		By("删除全部匹配 Namespace 后自动归零，再次出现后自动恢复")
		deleteNamespaceInEnvtest(ctx, secondNamespace)
		deleteNamespaceInEnvtest(ctx, firstNamespace)
		Eventually(func(g Gomega) {
			g.Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, &appsv1.Deployment{})).To(Satisfy(apierrors.IsNotFound))
		}, 5*time.Second).Should(Succeed())
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: firstNamespace}})).To(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, &appsv1.Deployment{})).To(Succeed())
		}, 5*time.Second).Should(Succeed())
	})

	It("IT-KSM-017 workload 尚未真正删除时不提前报告稳定零匹配", func() {
		By("准备有匹配项且 workload 带删除 finalizer 的实例")
		targetNamespace := "it-ksm-delete-pending-target-" + suffix
		matchedNamespace := "it-ksm-delete-pending-match-" + suffix
		for _, namespace := range []string{targetNamespace, matchedNamespace} {
			Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		}
		name := "delete-pending-" + suffix
		resourceName := "ksm-" + name
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: targetNamespace, NamespaceFilter: monitoringv1alpha1.NamespaceFilter{Mode: monitoringv1alpha1.NamespaceFilterModeInclude, NamePatterns: []string{"it-ksm-delete-pending-match-.*"}}},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())
		reconcile(name, "v1.28.15")
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, deployment)).To(Succeed())
		deployment.Finalizers = []string{"test.monitor-in-hand/finalizer"}
		Expect(testClient.Update(ctx, deployment)).To(Succeed())

		By("让匹配归零，并验证 workload 仍终止中时不会提前报告 NoNamespaceMatched")
		deleteNamespaceInEnvtest(ctx, matchedNamespace)
		result := reconcile(name, "v1.28.15")
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, deployment)).To(Succeed())
		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(condition(actual, "Ready").Reason).NotTo(Equal("NoNamespaceMatched"))

		By("解除 finalizer，并在 workload 消失后报告稳定零匹配")
		deployment.Finalizers = nil
		Expect(testClient.Update(ctx, deployment)).To(Succeed())
		Eventually(func() error {
			return testClient.Get(ctx, client.ObjectKey{Namespace: targetNamespace, Name: resourceName}, &appsv1.Deployment{})
		}).Should(Satisfy(apierrors.IsNotFound))
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(condition(actual, "Ready").Reason).To(Equal("NoNamespaceMatched"))
	})
})

func condition(instance *monitoringv1alpha1.KubeStateMetrics, conditionType string) metav1.Condition {
	for _, current := range instance.Status.Conditions {
		if current.Type == conditionType {
			return current
		}
	}
	Fail("condition not found: " + conditionType)
	return metav1.Condition{}
}

func expectStableManagedLabels(labels map[string]string, instanceID, version string) {
	Expect(labels).To(SatisfyAll(
		HaveKeyWithValue("app.kubernetes.io/name", "kube-state-metrics"),
		HaveKeyWithValue("app.kubernetes.io/instance", instanceID),
		HaveKeyWithValue("app.kubernetes.io/managed-by", "monitor-in-hand"),
		HaveKeyWithValue("app.kubernetes.io/version", version),
		HaveKeyWithValue("monitor-in-hand/role", "collector"),
		HaveKeyWithValue("monitor-in-hand/endpoint-group", "default"),
	))
}

func boolPointer(value bool) *bool { return &value }

func deleteNamespaceInEnvtest(ctx context.Context, name string) {
	namespace := &corev1.Namespace{}
	Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, namespace)).To(Succeed())
	Expect(testClient.Delete(ctx, namespace)).To(Succeed())
	Eventually(func() error {
		current := &corev1.Namespace{}
		if err := testClient.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		current.Spec.Finalizers = nil
		return testClient.SubResource("finalize").Update(ctx, current)
	}).Should(Succeed())
}
