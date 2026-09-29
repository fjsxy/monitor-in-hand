package controller_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/version"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	ksmcontroller "github.com/fjsxy/monitor-in-hand/internal/controller"
)

type fixedServerVersion struct{ gitVersion string }

func (f fixedServerVersion) ServerVersion() (*version.Info, error) {
	return &version.Info{GitVersion: f.gitVersion}, nil
}

var _ = Describe("KubeStateMetrics reconciliation", func() {
	ctx := context.Background()
	var suffix string

	BeforeEach(func() {
		suffix = fmt.Sprintf("%d", GinkgoRandomSeed()+int64(CurrentSpecReport().LeafNodeLocation.LineNumber))
	})

	reconcile := func(name, gitVersion string) {
		reconciler := &ksmcontroller.KubeStateMetricsReconciler{
			Client:        testClient,
			ServerVersion: fixedServerVersion{gitVersion: gitVersion},
		}
		_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
		Expect(err).NotTo(HaveOccurred())
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
	)

	It("IT-KSM-001 创建默认 Deployment 受管实例并从真实 workload status 报告可用性", func() {
		namespace := "it-ksm-success-" + suffix
		name := "success-" + suffix
		Expect(testClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		instance := &monitoringv1alpha1.KubeStateMetrics{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       monitoringv1alpha1.KubeStateMetricsSpec{TargetNamespace: namespace},
		}
		Expect(testClient.Create(ctx, instance)).To(Succeed())

		reconcile(name, "v1.28.15")
		resourceName := "ksm-" + name
		deployment := &appsv1.Deployment{}
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))
		Expect(deployment.OwnerReferences).To(ContainElement(HaveField("UID", instance.UID)))
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.Service{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, &corev1.ServiceAccount{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRole{})).To(Succeed())
		Expect(testClient.Get(ctx, client.ObjectKey{Name: resourceName}, &rbacv1.ClusterRoleBinding{})).To(Succeed())

		actual := &monitoringv1alpha1.KubeStateMetrics{}
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Spec.Enabled).NotTo(BeNil())
		Expect(*actual.Spec.Enabled).To(BeTrue())
		Expect(actual.Spec.Suspended).NotTo(BeNil())
		Expect(*actual.Spec.Suspended).To(BeFalse())
		Expect(actual.Spec.Deployment.Mode).To(Equal(monitoringv1alpha1.DeploymentModeDeployment))
		Expect(actual.Status).To(SatisfyAll(
			HaveField("ObservedGeneration", actual.Generation),
			HaveField("AppliedTargetNamespace", namespace),
			HaveField("KubeStateMetricsVersion", "2.11.0"),
			HaveField("DeploymentMode", monitoringv1alpha1.DeploymentModeDeployment),
			HaveField("DesiredPods", int32(1)),
			HaveField("AvailablePods", int32(0)),
			HaveField("ManagedResources", HaveLen(5)),
		))
		Expect(condition(actual, "Available").Status).To(Equal(metav1.ConditionFalse))
		resourceVersion := deployment.ResourceVersion
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resourceName}, deployment)).To(Succeed())
		Expect(deployment.ResourceVersion).To(Equal(resourceVersion), "已收敛的 Deployment 不应被重复写入")

		deployment.Status.Replicas = 1
		deployment.Status.ReadyReplicas = 1
		deployment.Status.AvailableReplicas = 1
		Expect(testClient.Status().Update(ctx, deployment)).To(Succeed())
		reconcile(name, "v1.28.15")
		Expect(testClient.Get(ctx, client.ObjectKey{Name: name}, actual)).To(Succeed())
		Expect(actual.Status.AvailablePods).To(Equal(int32(1)))
		Expect(actual.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseReady))
		Expect(condition(actual, "Ready").Status).To(Equal(metav1.ConditionTrue))
		Expect(condition(actual, "Progressing").Status).To(Equal(metav1.ConditionFalse))
		Expect(condition(actual, "Degraded").Status).To(Equal(metav1.ConditionFalse))
		Expect(condition(actual, "Available").Status).To(Equal(metav1.ConditionTrue))
	})

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

func boolPointer(value bool) *bool { return &value }
