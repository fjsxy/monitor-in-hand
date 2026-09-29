package kubestatemetrics_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	operatorImage     = "docker.io/fjsxy/monitor-in-hand:0.1.0"
	operatorNamespace = "monitor-in-hand-system"
	targetNamespace   = "monitor-in-hand-test"
	instanceName      = "default-deployment"
	managedName       = "ksm-default-deployment"
	lifecycleName     = "lifecycle"
	lifecycleManaged  = "ksm-lifecycle"
)

type scenario struct {
	id              string
	kubernetes      string
	version         string
	image           string
	expectedRules   []rbacv1.PolicyRule
	modernHardening bool
}

var (
	repoRoot    string
	kubectlBin  string
	helmBin     string
	kindBin     string
	clusterName string
	kubeContext string
	active      scenario
)

var _ = BeforeSuite(func(ctx SpecContext) {
	By("解析当前 Kubernetes 版本对应的稳定场景和期望契约")
	active = scenarioFor(os.Getenv("KUBERNETES_VERSION"))
	Expect(active.id).NotTo(BeEmpty(), "KUBERNETES_VERSION 必须是 1.19 或 1.28")
	runID := os.Getenv("TEST_RUN_ID")
	Expect(runID).To(MatchRegexp(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`), "TEST_RUN_ID 必须是非空 DNS label")

	var err error
	repoRoot, err = filepath.Abs(filepath.Join("..", "..", ".."))
	Expect(err).NotTo(HaveOccurred())
	kubectlBin = envOrDefault("KUBECTL", "kubectl")
	helmBin = envOrDefault("HELM_BIN", "helm")
	kindBin = envOrDefault("KIND", "kind")
	clusterName = fmt.Sprintf("mih-e2e-%s-%s", strings.ReplaceAll(active.kubernetes, ".", ""), runID)
	kubeContext = "kind-" + clusterName

	By("构建并导入 Operator 镜像，让测试只依赖当前工作树代码")
	run(ctx, repoRoot, []string{"BUILDKIT_PROGRESS=plain"}, "docker", "build", "--tag", operatorImage, repoRoot)
	run(ctx, repoRoot, nil, kindBin, "load", "docker-image", "--name", clusterName, operatorImage)

	By("通过 Helm 安装 Operator，环境编排不承担业务断言")
	run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "create", "namespace", operatorNamespace)
	run(ctx, repoRoot, nil, helmBin, "upgrade", "--install", "monitor-in-hand", filepath.Join(repoRoot, "charts", "monitor-in-hand"),
		"--kube-context", kubeContext, "--namespace", operatorNamespace, "--wait", "--timeout", "180s")
}, NodeTimeout(10*time.Minute))

var _ = Describe("默认 KubeStateMetrics Deployment", Serial, func() {
	It(fmt.Sprintf("[%s] 按 Kubernetes 版本生成完整资源并暴露真实指标", stableScenarioID()), func(ctx SpecContext) {
		By(fmt.Sprintf("[%s] 提交最小 KubeStateMetrics 声明", active.id))
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
`, instanceName, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		By("等待 Deployment 可用和 CR status 收敛，避免使用无界轮询")
		Eventually(func() error {
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", managedName)
			if err != nil {
				return err
			}
			if deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("availableReplicas=%d，期望 1", deployment.Status.AvailableReplicas)
			}
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", instanceName)
			if err != nil {
				return err
			}
			if instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady ||
				instance.Status.ObservedGeneration != instance.Generation || instance.Status.AvailablePods != 1 {
				return fmt.Errorf("status 尚未 Ready: phase=%s observed=%d generation=%d available=%d",
					instance.Status.Phase, instance.Status.ObservedGeneration, instance.Generation, instance.Status.AvailablePods)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("验证 CR 默认值、版本选择、受管资源清单和 Ready/Available 条件")
		instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", instanceName)
		Expect(err).NotTo(HaveOccurred())
		Expect(instance.Spec.Enabled).NotTo(BeNil())
		Expect(*instance.Spec.Enabled).To(BeTrue())
		Expect(instance.Spec.Suspended).NotTo(BeNil())
		Expect(*instance.Spec.Suspended).To(BeFalse())
		Expect(instance.Spec.Deployment.Mode).To(Equal(monitoringv1alpha1.DeploymentModeDeployment))
		Expect(instance.Status.AppliedTargetNamespace).To(Equal(targetNamespace))
		Expect(instance.Status.KubeStateMetricsVersion).To(Equal(active.version))
		Expect(instance.Status.DeploymentMode).To(Equal(monitoringv1alpha1.DeploymentModeDeployment))
		Expect(instance.Status.DesiredPods).To(Equal(int32(1)))
		Expect(instance.Status.AvailablePods).To(Equal(int32(1)))
		Expect(instance.Status.ManagedResources).To(HaveLen(5))
		Expect(conditionIsTrue(instance.Status.Conditions, "Ready", instance.Generation)).To(BeTrue())
		Expect(conditionIsTrue(instance.Status.Conditions, "Available", instance.Generation)).To(BeTrue())

		By("验证 Deployment 镜像、端口、探针和版本相关安全上下文")
		deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", managedName)
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Spec.Replicas).NotTo(BeNil())
		Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))
		podSpec := deployment.Spec.Template.Spec
		Expect(podSpec.ServiceAccountName).To(Equal(managedName))
		Expect(podSpec.NodeSelector).To(HaveKeyWithValue("kubernetes.io/os", "linux"))
		Expect(podSpec.Containers).To(HaveLen(1))
		container := podSpec.Containers[0]
		Expect(container.Image).To(Equal(active.image))
		Expect(container.Args).To(BeEmpty())
		Expect(container.Ports).To(ContainElements(
			SatisfyAll(HaveField("Name", "http-metrics"), HaveField("ContainerPort", int32(8080))),
			SatisfyAll(HaveField("Name", "telemetry"), HaveField("ContainerPort", int32(8081))),
		))
		assertHTTPProbe(container.LivenessProbe, "/healthz", "http-metrics")
		assertHTTPProbe(container.ReadinessProbe, "/", "telemetry")
		Expect(container.TerminationMessagePath).To(Equal("/dev/termination-log"))
		Expect(container.TerminationMessagePolicy).To(Equal(corev1.TerminationMessageReadFile))
		Expect(container.SecurityContext).NotTo(BeNil())
		Expect(container.SecurityContext.RunAsUser).NotTo(BeNil())
		Expect(*container.SecurityContext.RunAsUser).To(Equal(int64(65534)))
		assertVersionSecurity(podSpec, container.SecurityContext)

		By("验证 ServiceAccount、无头 Service 和所有受管对象的归属标签")
		serviceAccount, err := getObject[corev1.ServiceAccount](ctx, targetNamespace, "serviceaccount", managedName)
		Expect(err).NotTo(HaveOccurred())
		if active.modernHardening {
			Expect(serviceAccount.AutomountServiceAccountToken).NotTo(BeNil())
			Expect(*serviceAccount.AutomountServiceAccountToken).To(BeFalse())
		} else {
			Expect(serviceAccount.AutomountServiceAccountToken).To(BeNil())
		}
		service, err := getObject[corev1.Service](ctx, targetNamespace, "service", managedName)
		Expect(err).NotTo(HaveOccurred())
		Expect(service.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
		Expect(service.Spec.Ports).To(ContainElements(
			SatisfyAll(HaveField("Name", "http-metrics"), HaveField("Port", int32(8080)), HaveField("TargetPort.StrVal", "http-metrics")),
			SatisfyAll(HaveField("Name", "telemetry"), HaveField("Port", int32(8081)), HaveField("TargetPort.StrVal", "telemetry")),
		))
		Expect(service.Spec.Ports).To(HaveLen(2))
		Expect(service.Spec.Selector).To(SatisfyAll(
			HaveKeyWithValue("app.kubernetes.io/name", "kube-state-metrics"),
			HaveKeyWithValue("app.kubernetes.io/instance", instanceName),
			HaveKeyWithValue("monitor-in-hand/endpoint-group", "default"),
		))

		clusterRole, err := getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", managedName)
		Expect(err).NotTo(HaveOccurred())
		binding, err := getObject[rbacv1.ClusterRoleBinding](ctx, "", "clusterrolebinding", managedName)
		Expect(err).NotTo(HaveOccurred())
		for _, object := range []metav1.Object{deployment, service, serviceAccount, clusterRole, binding} {
			assertManagedMetadata(object, string(instance.UID))
		}

		By("验证 ClusterRole 权限集合和 ClusterRoleBinding 绑定目标完全匹配契约")
		Expect(canonicalRules(clusterRole.Rules)).To(Equal(canonicalRules(active.expectedRules)))
		Expect(binding.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: managedName}))
		Expect(binding.Subjects).To(Equal([]rbacv1.Subject{{Kind: "ServiceAccount", Name: managedName, Namespace: targetNamespace}}))

		By("通过真实 Service 端口读取指标，确认 kube-state-metrics 可用")
		assertMetricsAvailable(ctx)
	}, SpecTimeout(15*time.Minute))

	It(fmt.Sprintf("[%s] 覆盖停用、挂起、恢复与 finalizer 删除阻塞", lifecycleScenarioID()), func(ctx SpecContext) {
		By("创建独立的生命周期受管实例并等待其就绪")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
`, lifecycleName, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		Eventually(func() error {
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", lifecycleName)
			if err != nil {
				return err
			}
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", lifecycleManaged)
			if err != nil {
				return err
			}
			if instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady || deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("生命周期实例尚未就绪: phase=%s available=%d", instance.Status.Phase, deployment.Status.AvailableReplicas)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("停用实例并确认全部五类直接受管资源消失，CR 保留为 Disabled")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "patch", "kubestatemetrics/"+lifecycleName,
			"--type=merge", "--patch", `{"spec":{"enabled":false}}`)
		Eventually(func() error {
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", lifecycleName)
			if err != nil {
				return err
			}
			if instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseDisabled || instance.Status.ObservedGeneration != instance.Generation {
				return fmt.Errorf("status 尚未 Disabled: phase=%s observed=%d generation=%d", instance.Status.Phase, instance.Status.ObservedGeneration, instance.Generation)
			}
			return allLifecycleResourcesAbsent(ctx)
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("重新启用并等待当前声明重建完整 workload")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "patch", "kubestatemetrics/"+lifecycleName,
			"--type=merge", "--patch", `{"spec":{"enabled":true}}`)
		Eventually(func() error {
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", lifecycleName)
			if err != nil {
				return err
			}
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", lifecycleManaged)
			if err != nil {
				return err
			}
			if instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady || deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("重新启用尚未就绪: phase=%s available=%d", instance.Status.Phase, deployment.Status.AvailableReplicas)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("挂起实例并确认只删除 Deployment，保留 Service、ServiceAccount 与 RBAC")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "patch", "kubestatemetrics/"+lifecycleName,
			"--type=merge", "--patch", `{"spec":{"suspended":true}}`)
		Eventually(func() error {
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", lifecycleName)
			if err != nil {
				return err
			}
			if instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady ||
				!conditionMatches(instance.Status.Conditions, "Ready", metav1.ConditionTrue, "Suspended") ||
				!conditionMatches(instance.Status.Conditions, "Available", metav1.ConditionFalse, "Suspended") {
				return fmt.Errorf("挂起 status 尚未收敛: phase=%s conditions=%v", instance.Status.Phase, instance.Status.Conditions)
			}
			return objectAbsent(ctx, targetNamespace, "deployment", lifecycleManaged)
		}, 2*time.Minute, time.Second).Should(Succeed())
		_, err := getObject[corev1.Service](ctx, targetNamespace, "service", lifecycleManaged)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[corev1.ServiceAccount](ctx, targetNamespace, "serviceaccount", lifecycleManaged)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", lifecycleManaged)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[rbacv1.ClusterRoleBinding](ctx, "", "clusterrolebinding", lifecycleManaged)
		Expect(err).NotTo(HaveOccurred())

		By("恢复 workload 后设置阻塞 finalizer，再删除 CR")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "patch", "kubestatemetrics/"+lifecycleName,
			"--type=merge", "--patch", `{"spec":{"suspended":false}}`)
		Eventually(func() error {
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", lifecycleManaged)
			if err != nil {
				return err
			}
			if deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("恢复后的 Deployment availableReplicas=%d", deployment.Status.AvailableReplicas)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
			"patch", "deployment/"+lifecycleManaged, "--type=merge", "--patch", `{"metadata":{"finalizers":["tests.monitor-in-hand/block-deletion"]}}`)
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "kubestatemetrics/"+lifecycleName, "--wait=false")

		By("确认受管资源阻塞期间 CR 保持 Deleting 和清理 finalizer")
		Eventually(func() error {
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", lifecycleName)
			if err != nil {
				return err
			}
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", lifecycleManaged)
			if err != nil {
				return err
			}
			if instance.DeletionTimestamp.IsZero() || instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseDeleting ||
				!contains(instance.Finalizers, "monitoring.fjsxy.github.io/kubestatemetrics-cleanup") || deployment.DeletionTimestamp.IsZero() {
				return fmt.Errorf("删除尚未被 finalizer 正确阻塞: phase=%s crFinalizers=%v deploymentDeleting=%t",
					instance.Status.Phase, instance.Finalizers, !deployment.DeletionTimestamp.IsZero())
			}
			return nil
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("解除受管资源阻塞，确认清理完成后 CR 才消失")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
			"patch", "deployment/"+lifecycleManaged, "--type=merge", "--patch", `{"metadata":{"finalizers":[]}}`)
		Eventually(func() error {
			return objectAbsent(ctx, "", "kubestatemetrics", lifecycleName)
		}, 2*time.Minute, time.Second).Should(Succeed())
	}, SpecTimeout(20*time.Minute))
})

func stableScenarioID() string {
	if selected := scenarioFor(os.Getenv("KUBERNETES_VERSION")); selected.id != "" {
		return selected.id
	}
	return "E2E-UNSUPPORTED-KSM-001"
}

func lifecycleScenarioID() string {
	if active.kubernetes == "1.19" {
		return "E2E-119-KSM-002"
	}
	if active.kubernetes == "1.28" {
		return "E2E-128-KSM-002"
	}
	return "E2E-UNSUPPORTED-KSM-002"
}

func scenarioFor(version string) scenario {
	// 两个受支持版本共享基础权限，差异项在各自场景中显式列出，防止测试复制生产实现。
	base := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets", "nodes", "pods", "services", "resourcequotas", "replicationcontrollers", "limitranges", "persistentvolumeclaims", "persistentvolumes", "namespaces", "endpoints"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"apps"}, Resources: []string{"statefulsets", "daemonsets", "deployments", "replicasets"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"batch"}, Resources: []string{"cronjobs", "jobs"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"autoscaling"}, Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
		{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
		{APIGroups: []string{"policy"}, Resources: []string{"poddisruptionbudgets"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"certificates.k8s.io"}, Resources: []string{"certificatesigningrequests"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"storageclasses", "volumeattachments"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"admissionregistration.k8s.io"}, Resources: []string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies", "ingresses"}, Verbs: []string{"list", "watch"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"list", "watch"}},
	}
	if version == "1.19" {
		return scenario{id: "E2E-119-KSM-001", kubernetes: version, version: "2.3.0", image: "k8s.gcr.io/kube-state-metrics/kube-state-metrics:v2.3.0", expectedRules: base}
	}
	if version == "1.28" {
		base[0].Resources = append(base[0].Resources, "serviceaccounts")
		base[10].Resources = append(base[10].Resources, "ingressclasses")
		base = append(base,
			rbacv1.PolicyRule{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{"rbac.authorization.k8s.io"}, Resources: []string{"clusterrolebindings", "clusterroles", "rolebindings", "roles"}, Verbs: []string{"list", "watch"}},
		)
		return scenario{id: "E2E-128-KSM-001", kubernetes: version, version: "2.11.0", image: "registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.11.0", expectedRules: base, modernHardening: true}
	}
	return scenario{}
}

func assertHTTPProbe(probe *corev1.Probe, path, port string) {
	Expect(probe).NotTo(BeNil())
	Expect(probe.HTTPGet).NotTo(BeNil())
	Expect(probe.HTTPGet.Path).To(Equal(path))
	Expect(probe.HTTPGet.Port.StrVal).To(Equal(port))
	Expect(probe.HTTPGet.Scheme).To(Equal(corev1.URISchemeHTTP))
	Expect(probe.InitialDelaySeconds).To(Equal(int32(5)))
	Expect(probe.TimeoutSeconds).To(Equal(int32(5)))
	Expect(probe.PeriodSeconds).To(Equal(int32(10)))
	Expect(probe.SuccessThreshold).To(Equal(int32(1)))
	Expect(probe.FailureThreshold).To(Equal(int32(3)))
}

func assertVersionSecurity(podSpec corev1.PodSpec, security *corev1.SecurityContext) {
	if active.modernHardening {
		Expect(podSpec.AutomountServiceAccountToken).NotTo(BeNil())
		Expect(*podSpec.AutomountServiceAccountToken).To(BeTrue())
		Expect(security.AllowPrivilegeEscalation).NotTo(BeNil())
		Expect(*security.AllowPrivilegeEscalation).To(BeFalse())
		Expect(security.ReadOnlyRootFilesystem).NotTo(BeNil())
		Expect(*security.ReadOnlyRootFilesystem).To(BeTrue())
		Expect(security.RunAsNonRoot).NotTo(BeNil())
		Expect(*security.RunAsNonRoot).To(BeTrue())
		Expect(security.Capabilities).NotTo(BeNil())
		Expect(security.Capabilities.Drop).To(Equal([]corev1.Capability{"ALL"}))
		Expect(security.SeccompProfile).NotTo(BeNil())
		Expect(security.SeccompProfile.Type).To(Equal(corev1.SeccompProfileTypeRuntimeDefault))
		return
	}
	Expect(podSpec.AutomountServiceAccountToken).To(BeNil())
	Expect(security.AllowPrivilegeEscalation).To(BeNil())
	Expect(security.ReadOnlyRootFilesystem).To(BeNil())
	Expect(security.RunAsNonRoot).To(BeNil())
	Expect(security.Capabilities).To(BeNil())
	Expect(security.SeccompProfile).To(BeNil())
}

func assertManagedMetadata(object metav1.Object, ownerUID string) {
	labels := object.GetLabels()
	Expect(labels).To(SatisfyAll(
		HaveKeyWithValue("app.kubernetes.io/name", "kube-state-metrics"),
		HaveKeyWithValue("app.kubernetes.io/instance", instanceName),
		HaveKeyWithValue("app.kubernetes.io/managed-by", "monitor-in-hand"),
		HaveKeyWithValue("app.kubernetes.io/version", active.version),
	))
	owners := object.GetOwnerReferences()
	Expect(owners).To(HaveLen(1))
	Expect(string(owners[0].UID)).To(Equal(ownerUID))
	Expect(owners[0].Controller).NotTo(BeNil())
	Expect(*owners[0].Controller).To(BeTrue())
}

func conditionIsTrue(conditions []metav1.Condition, conditionType string, generation int64) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation {
			return true
		}
	}
	return false
}

func conditionMatches(conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus, reason string) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType && condition.Status == status && condition.Reason == reason {
			return true
		}
	}
	return false
}

func allLifecycleResourcesAbsent(ctx context.Context) error {
	for _, resource := range []struct {
		namespace string
		kind      string
	}{
		{targetNamespace, "deployment"},
		{targetNamespace, "service"},
		{targetNamespace, "serviceaccount"},
		{"", "clusterrole"},
		{"", "clusterrolebinding"},
	} {
		if err := objectAbsent(ctx, resource.namespace, resource.kind, lifecycleManaged); err != nil {
			return err
		}
	}
	return nil
}

func objectAbsent(ctx context.Context, namespace, kind, name string) error {
	args := []string{"--context", kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", kind+"/"+name)
	output, err := exec.CommandContext(ctx, kubectlBin, args...).CombinedOutput()
	if err == nil {
		return fmt.Errorf("%s/%s 仍然存在", kind, name)
	}
	if !strings.Contains(string(output), "NotFound") && !strings.Contains(string(output), "not found") {
		return fmt.Errorf("检查 %s/%s 是否消失: %w: %s", kind, name, err, output)
	}
	return nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func canonicalRules(rules []rbacv1.PolicyRule) []string {
	canonical := make([]string, 0, len(rules))
	for _, rule := range rules {
		sort.Strings(rule.APIGroups)
		sort.Strings(rule.Resources)
		sort.Strings(rule.Verbs)
		encoded, err := json.Marshal(rule)
		Expect(err).NotTo(HaveOccurred())
		canonical = append(canonical, string(encoded))
	}
	sort.Strings(canonical)
	return canonical
}

func assertMetricsAvailable(ctx SpecContext) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := listener.Addr().(*net.TCPAddr).Port
	Expect(listener.Close()).To(Succeed())

	logBuffer := &bytes.Buffer{}
	command := exec.CommandContext(ctx, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
		"port-forward", "service/"+managedName, fmt.Sprintf("%d:8080", port))
	command.Stdout = logBuffer
	command.Stderr = logBuffer
	Expect(command.Start()).To(Succeed())
	DeferCleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	})

	client := &http.Client{Timeout: 2 * time.Second}
	Eventually(func() error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/metrics", port), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("metrics 尚不可用: %w; port-forward=%s", err, logBuffer.String())
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("\nkube_pod_info")) {
			return fmt.Errorf("metrics 未包含 kube_pod_info: status=%d", response.StatusCode)
		}
		return nil
	}, time.Minute, time.Second).Should(Succeed())
}

func getObject[T any](ctx context.Context, namespace, kind, name string) (*T, error) {
	args := []string{"--context", kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", kind+"/"+name, "--output", "json")
	command := exec.CommandContext(ctx, kubectlBin, args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl get %s/%s: %w: %s", kind, name, err, output)
	}
	object := new(T)
	if err := json.Unmarshal(output, object); err != nil {
		return nil, fmt.Errorf("解析 %s/%s: %w", kind, name, err)
	}
	return object, nil
}

func run(ctx context.Context, dir string, extraEnv []string, name string, args ...string) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Env = append(os.Environ(), extraEnv...)
	output, err := command.CombinedOutput()
	_, _ = fmt.Fprintf(GinkgoWriter, "$ %s %s\n%s", name, strings.Join(args, " "), output)
	Expect(err).NotTo(HaveOccurred(), "命令失败: %s %s\n%s", name, strings.Join(args, " "), output)
}

func runWithInput(ctx context.Context, dir, input, name string, args ...string) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	_, _ = fmt.Fprintf(GinkgoWriter, "$ %s %s\n%s", name, strings.Join(args, " "), output)
	Expect(err).NotTo(HaveOccurred(), "命令失败: %s %s\n%s", name, strings.Join(args, " "), output)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
