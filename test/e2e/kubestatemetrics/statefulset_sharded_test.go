package kubestatemetrics_test

import (
	"fmt"
	"os"
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
)

const (
	shardedInstanceName = "statefulset-sharded"
	shardedManagedName  = "ksm-statefulset-sharded"
	invalidShardsName   = "invalid-statefulset-shards"
)

var _ = Describe("experimental StatefulSetSharded 部署模式", Serial, func() {
	It(fmt.Sprintf("[%s] 暴露全部 shard endpoints 并安全往返切换拓扑", statefulShardedScenarioID("001")), func(ctx SpecContext) {
		By("提交两分片声明，显式固定 collector、metric、namespace 和 resources")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  deployment:
    mode: StatefulSetSharded
    statefulSetSharded:
      shards: 2
  collectors:
    mode: Include
    names: [pods]
  metricFilter:
    mode: Allow
    patterns: [kube_pod_info]
  namespaceFilter:
    mode: Include
    names: [%s]
  podResources:
    requests:
      cpu: 15m
    limits:
      memory: 96Mi
`, shardedInstanceName, targetNamespace, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		By("等待 StatefulSet 和 status 的两个 shard 全部可用")
		waitForShardedTopology(ctx, shardedInstanceName, shardedManagedName, 2)

		By("验证自动分片参数、单一无头 Service 和稳定 selector")
		statefulSet, err := getObject[appsv1.StatefulSet](ctx, targetNamespace, "statefulset", shardedManagedName)
		Expect(err).NotTo(HaveOccurred())
		Expect(*statefulSet.Spec.Replicas).To(Equal(int32(2)))
		Expect(statefulSet.Spec.ServiceName).To(Equal(shardedManagedName))
		container := statefulSet.Spec.Template.Spec.Containers[0]
		expectedArgs := []string{
			"--resources=pods",
			"--metric-allowlist=kube_pod_info",
			"--namespaces=" + targetNamespace,
			"--pod=$(POD_NAME)",
			"--pod-namespace=$(POD_NAMESPACE)",
		}
		Expect(container.Args).To(Equal(expectedArgs))
		Expect(container.Resources.Requests).To(HaveKeyWithValue(corev1.ResourceCPU, resource.MustParse("15m")))
		Expect(container.Resources.Limits).To(HaveKeyWithValue(corev1.ResourceMemory, resource.MustParse("96Mi")))
		Expect(container.Env).To(ConsistOf(
			HaveField("Name", "POD_NAME"),
			HaveField("Name", "POD_NAMESPACE"),
		))
		service, err := getObject[corev1.Service](ctx, targetNamespace, "service", shardedManagedName)
		Expect(err).NotTo(HaveOccurred())
		Expect(service.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
		Expect(service.Spec.Selector).To(SatisfyAll(
			HaveKeyWithValue("app.kubernetes.io/instance", shardedInstanceName),
			HaveKeyWithValue("monitor-in-hand/endpoint-group", "sharded"),
			Not(HaveKey("app.kubernetes.io/version")),
		))

		By("验证每个 shard Pod 都继承同一份过滤与资源配置")
		for ordinal := 0; ordinal < 2; ordinal++ {
			podName := fmt.Sprintf("%s-%d", shardedManagedName, ordinal)
			pod, err := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.Spec.Containers).To(HaveLen(1))
			Expect(pod.Spec.Containers[0].Args).To(Equal(expectedArgs))
			Expect(pod.Spec.Containers[0].Resources).To(Equal(container.Resources))

			By(fmt.Sprintf("从 shard Pod %d 的 telemetry 验证唯一 ordinal 和 total shards", ordinal))
			assertTargetMetricsBody(ctx, "pod/"+podName, 8081, func(body []byte) error {
				ordinalMetric := fmt.Sprintf(`kube_state_metrics_shard_ordinal{shard_ordinal="%d"} %d`, ordinal, ordinal)
				if !strings.Contains(string(body), ordinalMetric) {
					return fmt.Errorf("telemetry 缺少 %s", ordinalMetric)
				}
				if !strings.Contains(string(body), "kube_state_metrics_total_shards 2") {
					return fmt.Errorf("telemetry 未报告 total shards=2")
				}
				return nil
			})
		}

		By("验证单一 Endpoints 对象同时包含两个 shard Pod")
		Eventually(func(g Gomega) {
			endpoints, getErr := getObject[corev1.Endpoints](ctx, targetNamespace, "endpoints", shardedManagedName)
			g.Expect(getErr).NotTo(HaveOccurred())
			var podNames []string
			for _, subset := range endpoints.Subsets {
				for _, address := range subset.Addresses {
					if address.TargetRef != nil && address.TargetRef.Kind == "Pod" {
						podNames = append(podNames, address.TargetRef.Name)
					}
				}
			}
			sort.Strings(podNames)
			g.Expect(podNames).To(Equal([]string{shardedManagedName + "-0", shardedManagedName + "-1"}))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("验证 Role/RoleBinding 只授予自身 Pod 与 StatefulSet 发现所需权限")
		role, err := getObject[rbacv1.Role](ctx, targetNamespace, "role", shardedManagedName)
		Expect(err).NotTo(HaveOccurred())
		Expect(canonicalRules(role.Rules)).To(Equal(canonicalRules([]rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
			{APIGroups: []string{"apps"}, Resources: []string{"statefulsets"}, ResourceNames: []string{shardedManagedName}, Verbs: []string{"get"}},
		})))
		roleBinding, err := getObject[rbacv1.RoleBinding](ctx, targetNamespace, "rolebinding", shardedManagedName)
		Expect(err).NotTo(HaveOccurred())
		Expect(roleBinding.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: shardedManagedName}))
		Expect(roleBinding.Subjects).To(Equal([]rbacv1.Subject{{Kind: "ServiceAccount", Name: shardedManagedName, Namespace: targetNamespace}}))
		instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", shardedInstanceName)
		Expect(err).NotTo(HaveOccurred())
		Expect(instance.Status).To(SatisfyAll(
			HaveField("DeploymentMode", monitoringv1alpha1.DeploymentModeStatefulSetSharded),
			HaveField("DesiredPods", int32(2)),
			HaveField("AvailablePods", int32(2)),
			HaveField("ManagedResources", ConsistOf(
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "v1", Kind: "ServiceAccount", Namespace: targetNamespace, Name: shardedManagedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: shardedManagedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: shardedManagedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "v1", Kind: "Service", Namespace: targetNamespace, Name: shardedManagedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role", Namespace: targetNamespace, Name: shardedManagedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding", Namespace: targetNamespace, Name: shardedManagedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "apps/v1", Kind: "StatefulSet", Namespace: targetNamespace, Name: shardedManagedName},
			)),
		))

		By("切回 Deployment，轮询期间拒绝两种 workload 同时存在")
		deploymentManifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  deployment:
    mode: Deployment
  collectors:
    mode: Include
    names: [pods]
  metricFilter:
    mode: Allow
    patterns: [kube_pod_info]
  namespaceFilter:
    mode: Include
    names: [%s]
  podResources:
    requests:
      cpu: 15m
    limits:
      memory: 96Mi
`, shardedInstanceName, targetNamespace, targetNamespace)
		runWithInput(ctx, repoRoot, deploymentManifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		waitForDeploymentTopology(ctx, shardedInstanceName, shardedManagedName)
		Expect(objectAbsent(ctx, targetNamespace, "role", shardedManagedName)).To(Succeed())
		Expect(objectAbsent(ctx, targetNamespace, "rolebinding", shardedManagedName)).To(Succeed())

		By("再切回 StatefulSetSharded，验证 delete-before-create 后恢复两个 endpoint")
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		waitForShardedTopology(ctx, shardedInstanceName, shardedManagedName, 2)
	}, SpecTimeout(20*time.Minute))

	It(fmt.Sprintf("[%s] 非法 shards 仅产生 InvalidSpec", statefulShardedScenarioID("002")), func(ctx SpecContext) {
		By("提交 shards=1 的非法判别 union 分支")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  deployment:
    mode: StatefulSetSharded
    statefulSetSharded:
      shards: 1
`, invalidShardsName, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		By("等待 controller 报告 InvalidSpec 并确认零资源写入")
		Eventually(func() error {
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", invalidShardsName)
			if err != nil {
				return err
			}
			if !conditionMatches(instance.Status.Conditions, "Degraded", "True", "InvalidSpec") {
				return fmt.Errorf("Degraded 尚未报告 InvalidSpec: %#v", instance.Status.Conditions)
			}
			if len(instance.Status.ManagedResources) != 0 {
				return fmt.Errorf("managedResources=%v，期望为空", instance.Status.ManagedResources)
			}
			return nil
		}, time.Minute, 2*time.Second).Should(Succeed())
		Expect(objectAbsent(ctx, targetNamespace, "statefulset", "ksm-"+invalidShardsName)).To(Succeed())
		Expect(objectAbsent(ctx, targetNamespace, "service", "ksm-"+invalidShardsName)).To(Succeed())
	}, SpecTimeout(5*time.Minute))
})

func statefulShardedScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"001": "E2E-119-KSM-GH56-001",
			"002": "E2E-119-KSM-GH56-002",
		},
		"1.28": {
			"001": "E2E-128-KSM-GH56-001",
			"002": "E2E-128-KSM-GH56-002",
		},
	}
	if versionIDs, supported := ids[os.Getenv("KUBERNETES_VERSION")]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-KSM-GH56-" + sequence
}

func waitForShardedTopology(ctx SpecContext, instanceName, managedName string, shards int32) {
	overlapObserved := false
	Eventually(func() error {
		statefulSet, err := getObject[appsv1.StatefulSet](ctx, targetNamespace, "statefulset", managedName)
		if err != nil {
			return err
		}
		if err := objectAbsent(ctx, targetNamespace, "deployment", managedName); err != nil {
			if strings.Contains(err.Error(), "仍然存在") {
				overlapObserved = true
			}
			return fmt.Errorf("确认旧 Deployment 已消失: %w", err)
		}
		if statefulSet.Status.ReadyReplicas != shards {
			return fmt.Errorf("StatefulSet readyReplicas=%d，期望 %d", statefulSet.Status.ReadyReplicas, shards)
		}
		instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", instanceName)
		if err != nil {
			return err
		}
		if instance.Status.DeploymentMode != monitoringv1alpha1.DeploymentModeStatefulSetSharded ||
			instance.Status.DesiredPods != shards || instance.Status.AvailablePods != shards ||
			instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady {
			return fmt.Errorf("StatefulSetSharded status 尚未收敛: mode=%s desired=%d available=%d phase=%s",
				instance.Status.DeploymentMode, instance.Status.DesiredPods, instance.Status.AvailablePods, instance.Status.Phase)
		}
		return nil
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	Expect(overlapObserved).To(BeFalse(), "模式切换轮询期间不得同时观察到 Deployment 与 StatefulSet")
}

func waitForDeploymentTopology(ctx SpecContext, instanceName, managedName string) {
	overlapObserved := false
	Eventually(func() error {
		deployment, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", managedName)
		if err != nil {
			return err
		}
		if err := objectAbsent(ctx, targetNamespace, "statefulset", managedName); err != nil {
			if strings.Contains(err.Error(), "仍然存在") {
				overlapObserved = true
			}
			return fmt.Errorf("确认旧 StatefulSet 已消失: %w", err)
		}
		if deployment.Status.AvailableReplicas != 1 {
			return fmt.Errorf("Deployment availableReplicas=%d，期望 1", deployment.Status.AvailableReplicas)
		}
		instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", instanceName)
		if err != nil {
			return err
		}
		if instance.Status.DeploymentMode != monitoringv1alpha1.DeploymentModeDeployment ||
			instance.Status.DesiredPods != 1 || instance.Status.AvailablePods != 1 ||
			instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady {
			return fmt.Errorf("Deployment status 尚未收敛: mode=%s desired=%d available=%d phase=%s",
				instance.Status.DeploymentMode, instance.Status.DesiredPods, instance.Status.AvailablePods, instance.Status.Phase)
		}
		return nil
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	Expect(overlapObserved).To(BeFalse(), "模式切换轮询期间不得同时观察到 StatefulSet 与 Deployment")
}
