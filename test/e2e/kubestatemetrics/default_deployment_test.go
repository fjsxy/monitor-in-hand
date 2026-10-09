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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	operatorNamespace   = "monitor-in-hand-system"
	targetNamespace     = "monitor-in-hand-test"
	instanceName        = "default-deployment"
	managedName         = "ksm-default-deployment"
	convergenceName     = "default-convergence"
	convergenceWorkload = "ksm-default-convergence"
	lifecycleName       = "lifecycle"
	lifecycleManaged    = "ksm-lifecycle"
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
	clusterName = fmt.Sprintf("mih-e2e-%s-%s", strings.ReplaceAll(active.kubernetes, ".", ""), runID)
	kubeContext = "kind-" + clusterName

	By("在本 Operator 启动前快照 Agent、runtime RBAC、采集配置与外部生成资源")
	gh60PreInstallSnapshots = captureGH60PreInstallSnapshots(ctx)

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
		Expect(instance.Spec.Collectors.Mode).To(Equal(monitoringv1alpha1.CollectorModeDefault))
		Expect(instance.Spec.Collectors.Names).To(BeEmpty())
		Expect(instance.Spec.MetricFilter.Mode).To(Equal(monitoringv1alpha1.MetricFilterModeAll))
		Expect(instance.Spec.MetricFilter.Patterns).To(BeEmpty())
		Expect(instance.Spec.PodResources).To(BeNil())
		Expect(instance.Status.AppliedTargetNamespace).To(Equal(targetNamespace))
		Expect(instance.Status.KubeStateMetricsVersion).To(Equal(active.version))
		Expect(instance.Status.DeploymentMode).To(Equal(monitoringv1alpha1.DeploymentModeDeployment))
		Expect(instance.Status.DesiredPods).To(Equal(int32(1)))
		Expect(instance.Status.AvailablePods).To(Equal(int32(1)))
		Expect(instance.Status.EffectiveCollectors).To(Equal(sharedCollectorCatalog()))
		Expect(instance.Status.EffectiveNamespaceCount).To(BeNumerically(">", 0))
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
		Expect(container.Args).NotTo(ContainElement(HavePrefix("--resources")))
		effectiveNamespaces, err := namespacesFromArgs(container.Args)
		Expect(err).NotTo(HaveOccurred())
		Expect(effectiveNamespaces).To(ContainElement(targetNamespace))
		Expect(container.Resources).To(Equal(corev1.ResourceRequirements{}))
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
		assertMetrics(ctx, managedName, []string{"kube_pod_info"}, nil)
	}, SpecTimeout(15*time.Minute))

	It(fmt.Sprintf("[%s] 已有 Default 实例不修改 CR 即收敛到上游默认参数", defaultConvergenceScenarioID()), func(ctx SpecContext) {
		By("提交显式 Default 实例并记录 CR generation、Pod UID 与所有权")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  collectors:
    mode: Default
`, convergenceName, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		waitForInstanceReady(ctx, convergenceName, convergenceWorkload)
		instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", convergenceName)
		Expect(err).NotTo(HaveOccurred())
		generation := instance.Generation
		ownerUID := instance.UID
		managedResources := append([]monitoringv1alpha1.ManagedResourceReference(nil), instance.Status.ManagedResources...)
		initialPodName := endpointPodNames(ctx, convergenceWorkload, 1)[0]
		initialPod, err := getObject[corev1.Pod](ctx, targetNamespace, "pod", initialPodName)
		Expect(err).NotTo(HaveOccurred())

		By("不修改 CR，将已拥有 Deployment 改成旧式显式 28 项 resources 参数")
		deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", convergenceWorkload)
		Expect(err).NotTo(HaveOccurred())
		initialDeploymentGeneration := deployment.Generation
		driftedArgs := append([]string{"--resources=" + strings.Join(sharedCollectorCatalog(), ",")}, deployment.Spec.Template.Spec.Containers[0].Args...)
		patch, err := json.Marshal([]map[string]any{{
			"op": "replace", "path": "/spec/template/spec/containers/0/args", "value": driftedArgs,
		}})
		Expect(err).NotTo(HaveOccurred())
		// 直接校验 PATCH 响应中的服务端对象，避免 owner watch 在后续 GET 前已恢复期望状态造成竞态。
		drifted, err := patchObject[appsv1.Deployment](ctx, targetNamespace, "deployment", convergenceWorkload, patch)
		Expect(err).NotTo(HaveOccurred())
		Expect(drifted.Spec.Template.Spec.Containers[0].Args).To(ContainElement("--resources=" + strings.Join(sharedCollectorCatalog(), ",")))
		Expect(drifted.Generation).To(BeNumerically(">", initialDeploymentGeneration))
		driftedGeneration := drifted.Generation

		By("等待 Operator 恢复期望 Pod template，并验证 Pod 更新、可用性和实例不变量")
		Eventually(func() error {
			current, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", convergenceWorkload)
			if getErr != nil {
				return getErr
			}
			if current.Generation <= driftedGeneration || current.Status.AvailableReplicas != 1 {
				return fmt.Errorf("Deployment 尚未在漂移后恢复: generation=%d drifted=%d available=%d", current.Generation, driftedGeneration, current.Status.AvailableReplicas)
			}
			for _, argument := range current.Spec.Template.Spec.Containers[0].Args {
				if strings.HasPrefix(argument, "--resources") {
					return fmt.Errorf("Deployment 仍包含 resources 参数: %v", current.Spec.Template.Spec.Containers[0].Args)
				}
			}
			currentInstance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", convergenceName)
			if getErr != nil {
				return getErr
			}
			if currentInstance.Generation != generation || currentInstance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady ||
				currentInstance.Status.ObservedGeneration != generation || currentInstance.Status.AvailablePods != 1 {
				return fmt.Errorf("CR 未在原 generation 上收敛: generation=%d observed=%d phase=%s available=%d",
					currentInstance.Generation, currentInstance.Status.ObservedGeneration, currentInstance.Status.Phase, currentInstance.Status.AvailablePods)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		finalPodName := endpointPodNames(ctx, convergenceWorkload, 1)[0]
		finalPod, err := getObject[corev1.Pod](ctx, targetNamespace, "pod", finalPodName)
		Expect(err).NotTo(HaveOccurred())
		Expect(finalPod.UID).NotTo(Equal(initialPod.UID))
		instance, err = getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", convergenceName)
		Expect(err).NotTo(HaveOccurred())
		Expect(instance.Status.KubeStateMetricsVersion).To(Equal(active.version))
		Expect(instance.Status.EffectiveCollectors).To(Equal(sharedCollectorCatalog()))
		Expect(instance.Status.ManagedResources).To(Equal(managedResources))
		deployment, err = getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", convergenceWorkload)
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal(active.image))
		effectiveNamespaces, err := namespacesFromArgs(deployment.Spec.Template.Spec.Containers[0].Args)
		Expect(err).NotTo(HaveOccurred())
		Expect(effectiveNamespaces).To(ContainElement(targetNamespace))
		assertOwnerReference(deployment, string(ownerUID))
		assertMetrics(ctx, convergenceWorkload, []string{"kube_pod_info"}, nil)
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
			for _, argument := range deployment.Spec.Template.Spec.Containers[0].Args {
				if strings.HasPrefix(argument, "--resources") {
					return fmt.Errorf("恢复后的 Default Deployment 仍包含 resources 参数: %v", deployment.Spec.Template.Spec.Containers[0].Args)
				}
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

	It(fmt.Sprintf("[%s] 显式 collectors、Allow filter 与 podResources 作用于真实 workload", configurationScenarioID("003")), func(ctx SpecContext) {
		const name = "configured-allow"
		managed := "ksm-" + name
		By("提交只采集 pods、使用未锚定 allow pattern 且显式声明 resources 的实例")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  collectors:
    mode: Include
    names: [pods]
  metricFilter:
    mode: Allow
    patterns: [pod_info, '^definitely_no_metric$']
  podResources:
    requests:
      cpu: 20m
      memory: 32Mi
    limits:
      cpu: 200m
      memory: 256Mi
`, name, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		By("等待真实 workload 和 status 收敛")
		waitForInstanceReady(ctx, name, managed)

		By("验证解析结果、参数与完整 ResourceRequirements 一致")
		instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", name)
		Expect(err).NotTo(HaveOccurred())
		Expect(instance.Status.EffectiveCollectors).To(Equal([]string{"pods"}))
		deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", managed)
		Expect(err).NotTo(HaveOccurred())
		container := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Args).To(ContainElements("--resources=pods", "--metric-allowlist=pod_info,^definitely_no_metric$"))
		Expect(container.Resources).To(Equal(corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		}))

		By("读取真实 metrics，证明 Go MatchString 未锚定语义允许 kube_pod_info 且过滤其他 pod metrics")
		assertMetrics(ctx, managed, []string{"kube_pod_info"}, []string{"kube_pod_status_phase"})
	}, SpecTimeout(10*time.Minute))

	It(fmt.Sprintf("[%s] Deny filter 从真实 workload 排除匹配指标", configurationScenarioID("004")), func(ctx SpecContext) {
		const name = "configured-deny"
		managed := "ksm-" + name
		By("提交 pods collector 和 kube_pod_info deny pattern")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  collectors:
    mode: Include
    names: [pods]
  metricFilter:
    mode: Deny
    patterns: [kube_pod_info]
`, name, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		By("等待真实 workload 和 status 收敛")
		waitForInstanceReady(ctx, name, managed)

		By("验证 denylist 参数并从真实 scrape 确认被拒指标缺失、其他 pod 指标仍存在")
		deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", managed)
		Expect(err).NotTo(HaveOccurred())
		Expect(deployment.Spec.Template.Spec.Containers[0].Args).To(ContainElements("--resources=pods", "--metric-denylist=kube_pod_info"))
		assertMetrics(ctx, managed, []string{"kube_pod_status_phase"}, []string{"kube_pod_info"})
	}, SpecTimeout(10*time.Minute))

	It(fmt.Sprintf("[%s] 非法配置只降级当前实例且不反复写资源", configurationScenarioID("005")), func(ctx SpecContext) {
		const name = "invalid-filter"
		By("提交包含非法 Go regexp 的实例")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  metricFilter:
    mode: Allow
    patterns: ['[']
`, name, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		By("等待 InvalidSpec status，并验证当前实例没有 workload")
		var invalidResourceVersion string
		Eventually(func() error {
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", name)
			if err != nil {
				return err
			}
			if instance.Status.ObservedGeneration != instance.Generation || !conditionHasReason(instance.Status.Conditions, "Degraded", "InvalidSpec") {
				return fmt.Errorf("InvalidSpec status 尚未收敛: generation=%d observed=%d", instance.Generation, instance.Status.ObservedGeneration)
			}
			invalidResourceVersion = instance.ResourceVersion
			return nil
		}, time.Minute, time.Second).Should(Succeed())
		_, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", "ksm-"+name)
		Expect(err).To(MatchError(ContainSubstring("NotFound")))

		By("确认相同 generation 不反复写 status，且 Operator 与既有合法实例保持可用")
		Consistently(func() string {
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", name)
			Expect(getErr).NotTo(HaveOccurred())
			return instance.ResourceVersion
		}, 5*time.Second, time.Second).Should(Equal(invalidResourceVersion))
		operator, err := getObject[appsv1.Deployment](ctx, operatorNamespace, "deployment", "monitor-in-hand-monitor-in-hand")
		Expect(err).NotTo(HaveOccurred())
		Expect(operator.Status.AvailableReplicas).To(Equal(int32(1)))
		valid, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", instanceName)
		Expect(err).NotTo(HaveOccurred())
		Expect(valid.Status.Phase).To(Equal(monitoringv1alpha1.KubeStateMetricsPhaseReady))
	}, SpecTimeout(5*time.Minute))

	It(fmt.Sprintf("[%s] 动态收敛 Namespace 采集范围", dynamicNamespaceScenarioID()), func(ctx SpecContext) {
		const (
			dynamicInstance = "dynamic-namespace-filter"
			dynamicManaged  = "ksm-dynamic-namespace-filter"
			literalName     = "mih-filter-literal"
			firstMatch      = "mih-filter-team-a"
			secondMatch     = "mih-filter-team-b"
			partialMatch    = "prefix-mih-filter-team-a"
			futureMatch     = "mih-filter-future"
		)

		By("创建字面、完整 regex 命中和仅 substring 命中的 Namespace")
		for _, namespace := range []string{literalName, firstMatch, partialMatch} {
			run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "create", "namespace", namespace)
			run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", namespace,
				"create", "configmap", "mih-filter-probe", "--from-literal=source=e2e")
		}
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  namespaceFilter:
    mode: Include
    names:
      - %s
    namePatterns:
      - mih-filter-team-[ab]
`, dynamicInstance, targetNamespace, literalName)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		By("验证字面名称与完整 regex 的去重并集统一下发到可用 workload")
		initialGeneration := int64(0)
		Eventually(func() error {
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", dynamicManaged)
			if err != nil {
				return err
			}
			actual, err := namespacesFromArgs(deployment.Spec.Template.Spec.Containers[0].Args)
			if err != nil {
				return err
			}
			if strings.Join(actual, ",") != literalName+","+firstMatch {
				return fmt.Errorf("effective namespaces=%v，期望字面与完整匹配并集", actual)
			}
			if deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("dynamic Deployment availableReplicas=%d", deployment.Status.AvailableReplicas)
			}
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", dynamicInstance)
			if err != nil {
				return err
			}
			if instance.Status.EffectiveNamespaceCount != 2 {
				return fmt.Errorf("effectiveNamespaceCount=%d，期望 2", instance.Status.EffectiveNamespaceCount)
			}
			initialGeneration = deployment.Generation
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		assertMetricLabels(ctx, dynamicManaged,
			[]metricExpectation{
				{name: "kube_configmap_info", labels: map[string]string{"namespace": literalName, "configmap": "mih-filter-probe"}},
				{name: "kube_configmap_info", labels: map[string]string{"namespace": firstMatch, "configmap": "mih-filter-probe"}},
			},
			[]metricExpectation{{name: "kube_configmap_info", labels: map[string]string{"namespace": partialMatch, "configmap": "mih-filter-probe"}}})

		By("新增 regex 命中 Namespace 后更新字面列表并触发 rollout")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "create", "namespace", secondMatch)
		Eventually(func() error {
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", dynamicManaged)
			if err != nil {
				return err
			}
			actual, err := namespacesFromArgs(deployment.Spec.Template.Spec.Containers[0].Args)
			if err != nil {
				return err
			}
			if strings.Join(actual, ",") != literalName+","+firstMatch+","+secondMatch {
				return fmt.Errorf("Namespace 展开列表尚未包含新增匹配项")
			}
			if deployment.Generation <= initialGeneration || deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("rollout 尚未完成: generation=%d available=%d", deployment.Generation, deployment.Status.AvailableReplicas)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("把过滤器改为当前零匹配，并验证只删除 workload")
		zeroManifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  namespaceFilter:
    mode: Include
    namePatterns:
      - mih-filter-future
`, dynamicInstance, targetNamespace)
		runWithInput(ctx, repoRoot, zeroManifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		Eventually(func() error {
			if err := objectAbsent(ctx, targetNamespace, "deployment", dynamicManaged); err != nil {
				return err
			}
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", dynamicInstance)
			if err != nil {
				return err
			}
			if instance.Status.EffectiveNamespaceCount != 0 || instance.Status.DesiredPods != 0 ||
				!conditionMatches(instance.Status.Conditions, "Ready", metav1.ConditionTrue, "NoNamespaceMatched") ||
				!conditionMatches(instance.Status.Conditions, "Available", metav1.ConditionFalse, "NoNamespaceMatched") {
				return fmt.Errorf("零匹配 status 尚未收敛: %+v", instance.Status)
			}
			return nil
		}, 2*time.Minute, time.Second).Should(Succeed())
		_, err := getObject[corev1.Service](ctx, targetNamespace, "service", dynamicManaged)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[corev1.ServiceAccount](ctx, targetNamespace, "serviceaccount", dynamicManaged)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", dynamicManaged)
		Expect(err).NotTo(HaveOccurred())

		By("匹配 Namespace 出现、删除并再次出现时自动恢复、归零和再恢复")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "create", "namespace", futureMatch)
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", futureMatch,
			"create", "configmap", "mih-filter-probe", "--from-literal=source=e2e")
		Eventually(func() error {
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", dynamicManaged)
			if err != nil {
				return err
			}
			if deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("恢复后的 Deployment 尚未可用")
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		assertMetricLabels(ctx, dynamicManaged,
			[]metricExpectation{{name: "kube_configmap_info", labels: map[string]string{"namespace": futureMatch, "configmap": "mih-filter-probe"}}},
			[]metricExpectation{{name: "kube_configmap_info", labels: map[string]string{"namespace": literalName, "configmap": "mih-filter-probe"}}})
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "namespace", futureMatch, "--wait=true")
		Eventually(func() error { return objectAbsent(ctx, targetNamespace, "deployment", dynamicManaged) }, 2*time.Minute, time.Second).Should(Succeed())
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "create", "namespace", futureMatch)
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", futureMatch,
			"create", "configmap", "mih-filter-probe", "--from-literal=source=e2e")
		Eventually(func() error {
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", dynamicManaged)
			if err != nil {
				return err
			}
			if deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("再次恢复后的 Deployment 尚未可用")
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
	}, SpecTimeout(20*time.Minute))

	It(fmt.Sprintf("[%s] 覆盖多实例隔离、漂移修复与冲突恢复", ownershipScenarioID()), func(ctx SpecContext) {
		const (
			primaryInstance   = "gh53.primary"
			primaryInstanceID = "gh53-primary-dc21f262"
			secondaryInstance = "gh53-secondary"
			primaryManaged    = "ksm-gh53-primary-dc21f262"
			secondaryManaged  = "ksm-gh53-secondary"
		)

		By("在同一 targetNamespace 创建两个独立实例并等待真实 workload 可用")
		applyKubeStateMetrics(ctx, primaryInstance, targetNamespace)
		applyKubeStateMetrics(ctx, secondaryInstance, targetNamespace)
		waitForInstanceReady(ctx, primaryInstance, primaryManaged)
		waitForInstanceReady(ctx, secondaryInstance, secondaryManaged)

		By("验证两套 Service、Pod endpoints 和 RBAC 只使用自己的稳定实例身份")
		for _, expected := range []struct {
			instance   string
			instanceID string
			managed    string
		}{{primaryInstance, primaryInstanceID, primaryManaged}, {secondaryInstance, secondaryInstance, secondaryManaged}} {
			service, err := getObject[corev1.Service](ctx, targetNamespace, "service", expected.managed)
			Expect(err).NotTo(HaveOccurred())
			assertStableLabels(service, expected.instanceID)
			Expect(service.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/name":         "kube-state-metrics",
				"app.kubernetes.io/instance":     expected.instanceID,
				"monitor-in-hand/endpoint-group": "default",
			}))
			Expect(service.Spec.Selector).NotTo(HaveKey("app.kubernetes.io/version"))
			Expect(service.Spec.Selector).NotTo(HaveKey("monitor-in-hand/role"))
			Eventually(func() error {
				endpoints, getErr := getObject[corev1.Endpoints](ctx, targetNamespace, "endpoints", expected.managed)
				if getErr != nil {
					return getErr
				}
				if len(endpoints.Subsets) != 1 || len(endpoints.Subsets[0].Addresses) != 1 || endpoints.Subsets[0].Addresses[0].TargetRef == nil {
					return fmt.Errorf("%s endpoints 尚未收敛: %+v", expected.managed, endpoints.Subsets)
				}
				pod, getErr := getObject[corev1.Pod](ctx, targetNamespace, "pod", endpoints.Subsets[0].Addresses[0].TargetRef.Name)
				if getErr != nil {
					return getErr
				}
				assertStableLabels(pod, expected.instanceID)
				if pod.Labels["app.kubernetes.io/instance"] != expected.instanceID || pod.Labels["monitor-in-hand/endpoint-group"] != "default" {
					return fmt.Errorf("%s endpoint Pod labels=%v", expected.managed, pod.Labels)
				}
				return nil
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
			role, err := getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", expected.managed)
			Expect(err).NotTo(HaveOccurred())
			assertStableLabels(role, expected.instanceID)
			Expect(role.OwnerReferences).To(HaveLen(1))
			binding, err := getObject[rbacv1.ClusterRoleBinding](ctx, "", "clusterrolebinding", expected.managed)
			Expect(err).NotTo(HaveOccurred())
			assertStableLabels(binding, expected.instanceID)
			Expect(binding.Subjects).To(ContainElement(SatisfyAll(HaveField("Name", expected.managed), HaveField("Namespace", targetNamespace))))
			deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", expected.managed)
			Expect(err).NotTo(HaveOccurred())
			assertStableLabels(deployment, expected.instanceID)
			serviceAccount, err := getObject[corev1.ServiceAccount](ctx, targetNamespace, "serviceaccount", expected.managed)
			Expect(err).NotTo(HaveOccurred())
			assertStableLabels(serviceAccount, expected.instanceID)
		}

		By("在不同 targetNamespace 创建同类实例，验证 namespaced 资源落点与 cluster-scoped RBAC 仍独立")
		crossNamespace := "mih-gh53-cross-" + strings.ReplaceAll(active.kubernetes, ".", "")
		const crossInstance = "gh53-cross-namespace"
		const crossManaged = "ksm-gh53-cross-namespace"
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "create", "namespace", crossNamespace)
		applyKubeStateMetrics(ctx, crossInstance, crossNamespace)
		Eventually(func() error {
			deployment, getErr := getObject[appsv1.Deployment](ctx, crossNamespace, "deployment", crossManaged)
			if getErr != nil {
				return getErr
			}
			if deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("跨 Namespace Deployment 尚未可用")
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		Expect(objectAbsent(ctx, targetNamespace, "deployment", crossManaged)).To(Succeed())
		crossBinding, err := getObject[rbacv1.ClusterRoleBinding](ctx, "", "clusterrolebinding", crossManaged)
		Expect(err).NotTo(HaveOccurred())
		Expect(crossBinding.Subjects).To(ContainElement(SatisfyAll(HaveField("Name", crossManaged), HaveField("Namespace", crossNamespace))))

		By("修改已拥有 Service 并删除已拥有 Deployment，验证 direct-resource watch 自动修复")
		primaryDeployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", primaryManaged)
		Expect(err).NotTo(HaveOccurred())
		oldDeploymentUID := primaryDeployment.UID
		driftPatch := `{"metadata":{"labels":{"drifted":"true"}},"spec":{"selector":{"drifted":"true"}}}`
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
			"patch", "service/"+primaryManaged, "--type=merge", "--patch", driftPatch)
		Eventually(func() error {
			service, getErr := getObject[corev1.Service](ctx, targetNamespace, "service", primaryManaged)
			if getErr != nil {
				return getErr
			}
			selector := service.Spec.Selector
			if _, drifted := service.Labels["drifted"]; drifted || len(selector) != 3 ||
				selector["app.kubernetes.io/name"] != "kube-state-metrics" ||
				selector["app.kubernetes.io/instance"] != primaryInstanceID ||
				selector["monitor-in-hand/endpoint-group"] != "default" {
				return fmt.Errorf("Service 漂移尚未修复: labels=%v selector=%v", service.Labels, service.Spec.Selector)
			}
			return nil
		}, time.Minute, time.Second).Should(Succeed())
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
			"delete", "deployment/"+primaryManaged, "--wait=true")
		Eventually(func() error {
			deployment, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", primaryManaged)
			if getErr != nil {
				return getErr
			}
			if deployment.UID == oldDeploymentUID || deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("重建 Deployment 尚未可用: oldUID=%s currentUID=%s available=%d", oldDeploymentUID, deployment.UID, deployment.Status.AvailableReplicas)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("分别以无 owner、错误 owner 和同名旧 UID 预占身份，验证完整所有权分类与冲突恢复")
		secondaryObject, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", secondaryInstance)
		Expect(err).NotTo(HaveOccurred())
		conflicts := []struct {
			class     string
			instance  string
			managed   string
			ownerYAML string
			ownerUID  string
		}{
			{class: "none", instance: "gh53-conflict-none", managed: "ksm-gh53-conflict-none"},
			{
				class: "wrong", instance: "gh53-conflict-wrong", managed: "ksm-gh53-conflict-wrong", ownerUID: string(secondaryObject.UID),
				ownerYAML: fmt.Sprintf("  ownerReferences:\n  - apiVersion: monitoring.fjsxy.github.io/v1alpha1\n    kind: KubeStateMetrics\n    name: %s\n    uid: %s\n    controller: true", secondaryInstance, secondaryObject.UID),
			},
			{
				class: "old", instance: "gh53-conflict-old", managed: "ksm-gh53-conflict-old", ownerUID: "uid-from-deleted-cr",
				ownerYAML: "  finalizers:\n  - tests.monitor-in-hand/preserve-old-uid\n  ownerReferences:\n  - apiVersion: monitoring.fjsxy.github.io/v1alpha1\n    kind: KubeStateMetrics\n    name: gh53-conflict-old\n    uid: uid-from-deleted-cr\n    controller: true",
			},
		}
		for _, conflict := range conflicts {
			By(fmt.Sprintf("预占 %s 分类的 ClusterRole 身份", conflict.class))
			conflictRole := fmt.Sprintf(`apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: %s
  labels:
    preserve: %q
%s
rules:
  - apiGroups: ["example.test"]
    resources: ["sentinels"]
    verbs: ["get"]
`, conflict.managed, conflict.class, conflict.ownerYAML)
			runWithInput(ctx, repoRoot, conflictRole, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
			applyKubeStateMetrics(ctx, conflict.instance, targetNamespace)
			Eventually(func() error {
				instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", conflict.instance)
				if getErr != nil {
					return getErr
				}
				if !conditionMatches(instance.Status.Conditions, "Degraded", metav1.ConditionTrue, "ResourceConflict") || len(instance.Status.ManagedResources) != 0 {
					return fmt.Errorf("%s 冲突 status 尚未收敛: %+v", conflict.class, instance.Status)
				}
				return nil
			}, time.Minute, time.Second).Should(Succeed())
			preservedRole, getErr := getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", conflict.managed)
			Expect(getErr).NotTo(HaveOccurred())
			Expect(preservedRole.Labels).To(Equal(map[string]string{"preserve": conflict.class}))
			Expect(preservedRole.Rules).To(ConsistOf(rbacv1.PolicyRule{APIGroups: []string{"example.test"}, Resources: []string{"sentinels"}, Verbs: []string{"get"}}))
			if conflict.ownerUID == "" {
				Expect(preservedRole.OwnerReferences).To(BeEmpty())
			} else {
				Expect(preservedRole.OwnerReferences).To(ContainElement(HaveField("UID", types.UID(conflict.ownerUID))))
			}
			Expect(objectAbsent(ctx, targetNamespace, "deployment", conflict.managed)).To(Succeed())
			Expect(objectAbsent(ctx, targetNamespace, "serviceaccount", conflict.managed)).To(Succeed())

			By(fmt.Sprintf("移除 %s 冲突后等待定时重试恢复", conflict.class))
			if conflict.class == "old" {
				run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "patch", "clusterrole/"+conflict.managed,
					"--type=merge", "--patch", `{"metadata":{"finalizers":[]}}`)
			}
			run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "clusterrole/"+conflict.managed,
				"--ignore-not-found=true", "--wait=true")
			waitForInstanceReady(ctx, conflict.instance, conflict.managed)

			if conflict.class == "old" {
				By("将恢复后的 Deployment 改为旧 CR UID owner，验证 finalizer 不删除它")
				oldOwnerPatch := fmt.Sprintf(`{"metadata":{"annotations":{"preserve":"old-uid"},"finalizers":["tests.monitor-in-hand/preserve-old-uid"],"ownerReferences":[{"apiVersion":"monitoring.fjsxy.github.io/v1alpha1","kind":"KubeStateMetrics","name":%q,"uid":"uid-from-deleted-cr","controller":true}]}}`, conflict.instance)
				run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
					"patch", "deployment/"+conflict.managed, "--type=merge", "--patch", oldOwnerPatch)
				run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "kubestatemetrics/"+conflict.instance, "--wait=true", "--timeout=180s")
				preservedDeployment, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", conflict.managed)
				Expect(getErr).NotTo(HaveOccurred())
				Expect(preservedDeployment.Annotations).To(HaveKeyWithValue("preserve", "old-uid"))
				run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
					"patch", "deployment/"+conflict.managed, "--type=merge", "--patch", `{"metadata":{"finalizers":[]}}`)
				run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
					"delete", "deployment/"+conflict.managed, "--ignore-not-found=true", "--wait=true")
			} else {
				run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "kubestatemetrics/"+conflict.instance, "--wait=true", "--timeout=180s")
			}
		}

		By("暂时撤销 Operator 的 Deployment 写权限，验证真实部分写入报告 ApplyFailed 且不回滚")
		const managerRoleName = "monitor-in-hand-manager-role"
		managerRole, err := getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", managerRoleName)
		Expect(err).NotTo(HaveOccurred())
		originalManagerRules := managerRole.DeepCopy().Rules
		restrictedManagerRules := managerRole.DeepCopy().Rules
		for ruleIndex := range restrictedManagerRules {
			rule := &restrictedManagerRules[ruleIndex]
			if !contains(rule.APIGroups, "apps") || !contains(rule.Resources, "deployments") {
				continue
			}
			verbs := make([]string, 0, len(rule.Verbs))
			for _, verb := range rule.Verbs {
				if verb != "create" && verb != "update" && verb != "patch" {
					verbs = append(verbs, verb)
				}
			}
			rule.Verbs = verbs
		}
		managerRoleRestricted := true
		DeferCleanup(func() {
			if managerRoleRestricted {
				replaceClusterRoleRules(context.Background(), managerRoleName, originalManagerRules)
			}
		})
		replaceClusterRoleRules(ctx, managerRoleName, restrictedManagerRules)
		const partialInstance = "gh53-partial-apply"
		const partialManaged = "ksm-gh53-partial-apply"
		applyKubeStateMetrics(ctx, partialInstance, targetNamespace)
		Eventually(func() error {
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", partialInstance)
			if getErr != nil {
				return getErr
			}
			if !conditionMatches(instance.Status.Conditions, "Degraded", metav1.ConditionTrue, "ApplyFailed") {
				return fmt.Errorf("部分写入尚未报告 ApplyFailed: %+v", instance.Status)
			}
			return nil
		}, time.Minute, time.Second).Should(Succeed())
		_, err = getObject[corev1.ServiceAccount](ctx, targetNamespace, "serviceaccount", partialManaged)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[corev1.Service](ctx, targetNamespace, "service", partialManaged)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", partialManaged)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[rbacv1.ClusterRoleBinding](ctx, "", "clusterrolebinding", partialManaged)
		Expect(err).NotTo(HaveOccurred())
		Expect(objectAbsent(ctx, targetNamespace, "deployment", partialManaged)).To(Succeed())

		By("恢复 Operator 写权限，验证后续调谐保留已成功资源并补齐 workload")
		partialService, err := getObject[corev1.Service](ctx, targetNamespace, "service", partialManaged)
		Expect(err).NotTo(HaveOccurred())
		partialServiceUID := partialService.UID
		replaceClusterRoleRules(ctx, managerRoleName, originalManagerRules)
		managerRoleRestricted = false
		waitForInstanceReady(ctx, partialInstance, partialManaged)
		partialService, err = getObject[corev1.Service](ctx, targetNamespace, "service", partialManaged)
		Expect(err).NotTo(HaveOccurred())
		Expect(partialService.UID).To(Equal(partialServiceUID))
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "kubestatemetrics/"+partialInstance, "--wait=true", "--timeout=180s")

		By("删除主实例，验证 finalizer 只清理其资源且不影响其他实例")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "kubestatemetrics/"+primaryInstance, "--wait=true", "--timeout=180s")
		for _, resource := range []struct {
			namespace string
			kind      string
		}{{targetNamespace, "deployment"}, {targetNamespace, "service"}, {targetNamespace, "serviceaccount"}, {"", "clusterrole"}, {"", "clusterrolebinding"}} {
			Expect(objectAbsent(ctx, resource.namespace, resource.kind, primaryManaged)).To(Succeed())
		}
		waitForInstanceReady(ctx, secondaryInstance, secondaryManaged)
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "kubestatemetrics/"+crossInstance, "--wait=true", "--timeout=180s")
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "delete", "namespace/"+crossNamespace, "--wait=true")
	}, SpecTimeout(20*time.Minute))
})

func stableScenarioID() string {
	if selected := scenarioFor(os.Getenv("KUBERNETES_VERSION")); selected.id != "" {
		return selected.id
	}
	return "E2E-UNSUPPORTED-KSM-001"
}

func lifecycleScenarioID() string {
	if os.Getenv("KUBERNETES_VERSION") == "1.19" {
		return "E2E-119-KSM-002"
	}
	if os.Getenv("KUBERNETES_VERSION") == "1.28" {
		return "E2E-128-KSM-002"
	}
	return "E2E-UNSUPPORTED-KSM-002"
}

func defaultConvergenceScenarioID() string {
	if os.Getenv("KUBERNETES_VERSION") == "1.19" {
		return "E2E-119-KSM-GH81-001"
	}
	if os.Getenv("KUBERNETES_VERSION") == "1.28" {
		return "E2E-128-KSM-GH81-001"
	}
	return "E2E-UNSUPPORTED-KSM-GH81-001"
}

func dynamicNamespaceScenarioID() string {
	if os.Getenv("KUBERNETES_VERSION") == "1.19" {
		return "E2E-119-KSM-006"
	}
	if os.Getenv("KUBERNETES_VERSION") == "1.28" {
		return "E2E-128-KSM-006"
	}
	return "E2E-UNSUPPORTED-KSM-006"
}

func ownershipScenarioID() string {
	ids := map[string]string{
		"1.19": "E2E-119-KSM-GH53-001",
		"1.28": "E2E-128-KSM-GH53-001",
	}
	if id, supported := ids[os.Getenv("KUBERNETES_VERSION")]; supported {
		return id
	}
	return "E2E-UNSUPPORTED-KSM-GH53-001"
}

func configurationScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"003": "E2E-119-KSM-003",
			"004": "E2E-119-KSM-004",
			"005": "E2E-119-KSM-005",
		},
		"1.28": {
			"003": "E2E-128-KSM-003",
			"004": "E2E-128-KSM-004",
			"005": "E2E-128-KSM-005",
		},
	}
	if versionIDs, supported := ids[os.Getenv("KUBERNETES_VERSION")]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-KSM-" + sequence
}

func sharedCollectorCatalog() []string {
	return []string{
		"certificatesigningrequests", "configmaps", "cronjobs", "daemonsets", "deployments", "endpoints",
		"horizontalpodautoscalers", "ingresses", "jobs", "leases", "limitranges", "mutatingwebhookconfigurations",
		"namespaces", "networkpolicies", "nodes", "persistentvolumeclaims", "persistentvolumes", "poddisruptionbudgets",
		"pods", "replicasets", "replicationcontrollers", "resourcequotas", "secrets", "services", "statefulsets",
		"storageclasses", "validatingwebhookconfigurations", "volumeattachments",
	}
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
	assertStableLabels(object, instanceName)
	assertOwnerReference(object, ownerUID)
}

func assertOwnerReference(object metav1.Object, ownerUID string) {
	owners := object.GetOwnerReferences()
	Expect(owners).To(HaveLen(1))
	Expect(string(owners[0].UID)).To(Equal(ownerUID))
	Expect(owners[0].Controller).NotTo(BeNil())
	Expect(*owners[0].Controller).To(BeTrue())
}

func assertStableLabels(object metav1.Object, expectedInstanceID string) {
	labels := object.GetLabels()
	Expect(labels).To(SatisfyAll(
		HaveKeyWithValue("app.kubernetes.io/name", "kube-state-metrics"),
		HaveKeyWithValue("app.kubernetes.io/instance", expectedInstanceID),
		HaveKeyWithValue("app.kubernetes.io/managed-by", "monitor-in-hand"),
		HaveKeyWithValue("app.kubernetes.io/version", active.version),
		HaveKeyWithValue("monitor-in-hand/role", "collector"),
		HaveKeyWithValue("monitor-in-hand/endpoint-group", "default"),
	))
}

func conditionIsTrue(conditions []metav1.Condition, conditionType string, generation int64) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType && condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == generation {
			return true
		}
	}
	return false
}

func conditionHasReason(conditions []metav1.Condition, conditionType, reason string) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType && condition.Status == metav1.ConditionTrue && condition.Reason == reason {
			return true
		}
	}
	return false
}

func waitForInstanceReady(ctx SpecContext, instance, deployment string) {
	Eventually(func() error {
		workload, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", deployment)
		if err != nil {
			return err
		}
		if workload.Status.AvailableReplicas != 1 {
			return fmt.Errorf("Deployment %s availableReplicas=%d，期望 1", deployment, workload.Status.AvailableReplicas)
		}
		managed, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", instance)
		if err != nil {
			return err
		}
		if managed.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady ||
			managed.Status.ObservedGeneration != managed.Generation || managed.Status.AvailablePods != 1 {
			return fmt.Errorf("实例 %s status 尚未 Ready: phase=%s observed=%d generation=%d available=%d",
				instance, managed.Status.Phase, managed.Status.ObservedGeneration, managed.Generation, managed.Status.AvailablePods)
		}
		return nil
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
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

func namespacesFromArgs(args []string) ([]string, error) {
	for _, argument := range args {
		if !strings.HasPrefix(argument, "--namespaces=") {
			continue
		}
		namespaces := strings.Split(strings.TrimPrefix(argument, "--namespaces="), ",")
		if !sort.StringsAreSorted(namespaces) {
			return nil, fmt.Errorf("namespace args 未排序: %v", namespaces)
		}
		return namespaces, nil
	}
	return nil, fmt.Errorf("args=%v，缺少 --namespaces 参数", args)
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

func assertMetrics(ctx SpecContext, serviceName string, present, absent []string) {
	assertMetricsBody(ctx, serviceName, func(body []byte) error {
		for _, metric := range present {
			if !bytes.Contains(body, []byte("\n"+metric)) {
				return fmt.Errorf("metrics 未包含 %s", metric)
			}
		}
		for _, metric := range absent {
			if bytes.Contains(body, []byte("\n"+metric)) {
				return fmt.Errorf("metrics 不应包含 %s", metric)
			}
		}
		return nil
	})
}

type metricExpectation struct {
	name   string
	labels map[string]string
}

func assertMetricLabels(ctx SpecContext, serviceName string, present, absent []metricExpectation) {
	assertMetricsBody(ctx, serviceName, func(body []byte) error {
		for _, expectation := range present {
			if !metricHasLabels(body, expectation) {
				return fmt.Errorf("metrics 未包含 %s labels=%v", expectation.name, expectation.labels)
			}
		}
		for _, expectation := range absent {
			if metricHasLabels(body, expectation) {
				return fmt.Errorf("metrics 不应包含 %s labels=%v", expectation.name, expectation.labels)
			}
		}
		return nil
	})
}

func metricHasLabels(body []byte, expectation metricExpectation) bool {
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, expectation.name+"{") {
			continue
		}
		matched := true
		for name, value := range expectation.labels {
			if !strings.Contains(line, fmt.Sprintf(`%s="%s"`, name, value)) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func assertMetricsBody(ctx SpecContext, serviceName string, verify func([]byte) error) {
	assertTargetMetricsBody(ctx, "service/"+serviceName, 8080, verify)
}

func assertTargetMetricsBody(ctx SpecContext, target string, remotePort int, verify func([]byte) error) {
	// 先预留本地端口并启动到指定受管 endpoint 的转发进程。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	port := listener.Addr().(*net.TCPAddr).Port
	Expect(listener.Close()).To(Succeed())

	logBuffer := &bytes.Buffer{}
	command := exec.CommandContext(ctx, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
		"port-forward", target, fmt.Sprintf("%d:%d", port, remotePort))
	command.Stdout = logBuffer
	command.Stderr = logBuffer
	Expect(command.Start()).To(Succeed())
	// 场景结束时清理转发进程，避免多个真实 workload 验收相互干扰。
	DeferCleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	})

	client := &http.Client{Timeout: 2 * time.Second}
	// 在有界时间内等待 metrics endpoint 可用，并验证允许与拒绝集合。
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
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("metrics HTTP status=%d", response.StatusCode)
		}
		return verify(body)
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

func patchObject[T any](ctx context.Context, namespace, kind, name string, patch []byte) (*T, error) {
	args := []string{"--context", kubeContext}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "patch", kind+"/"+name, "--type=json", "--patch", string(patch), "--output=json")
	command := exec.CommandContext(ctx, kubectlBin, args...)
	output, err := command.CombinedOutput()
	_, _ = fmt.Fprintf(GinkgoWriter, "$ %s %s\n%s", kubectlBin, strings.Join(args, " "), output)
	if err != nil {
		return nil, fmt.Errorf("kubectl patch %s/%s: %w: %s", kind, name, err, output)
	}
	object := new(T)
	if err := json.Unmarshal(output, object); err != nil {
		return nil, fmt.Errorf("解析 patch %s/%s 响应: %w", kind, name, err)
	}
	return object, nil
}

func replaceClusterRoleRules(ctx context.Context, name string, rules []rbacv1.PolicyRule) {
	role, err := getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", name)
	Expect(err).NotTo(HaveOccurred())
	role.Rules = rules
	manifest, err := json.Marshal(role)
	Expect(err).NotTo(HaveOccurred())
	runWithInput(ctx, repoRoot, string(manifest), kubectlBin, "--context", kubeContext, "replace", "--filename", "-")
}

func applyKubeStateMetrics(ctx context.Context, name, namespace string) {
	manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
`, name, namespace)
	runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
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
