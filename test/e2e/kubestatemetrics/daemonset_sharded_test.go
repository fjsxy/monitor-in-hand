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
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	daemonShardedInstanceName = "daemonset-sharded"
	daemonShardedManagedName  = "ksm-daemonset-sharded"
	unscheduledProbeName      = "gh57-unscheduled-probe"
)

var _ = Describe("experimental DaemonSetSharded 部署模式", Serial, func() {
	It(fmt.Sprintf("[%s] 按版本交付三职责拓扑或零写入拒绝", daemonShardedScenarioID("001")), func(ctx SpecContext) {
		By("提交 collectors 同时包含 Pod 与非 Pod、并带角色资源覆盖的 DaemonSetSharded 声明")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  deployment:
    mode: DaemonSetSharded
    daemonSetSharded:
      nodePodResources:
        limits:
          memory: 72Mi
      clusterPodResources:
        requests:
          cpu: 23m
  collectors:
    mode: Include
    names: [pods, deployments]
  metricFilter:
    mode: Allow
    patterns: ['kube_(pod|deployment)_info']
  namespaceFilter:
    mode: Include
    names: [%s]
  podResources:
    requests:
      cpu: 11m
`, daemonShardedInstanceName, targetNamespace, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		if active.kubernetes == "1.19" {
			By("等待 UnsupportedMode status，并确认全部 DaemonSetSharded 预期身份保持不存在")
			Eventually(func() error {
				instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", daemonShardedInstanceName)
				if err != nil {
					return err
				}
				if !conditionMatches(instance.Status.Conditions, "Degraded", "True", "UnsupportedMode") {
					return fmt.Errorf("Degraded 尚未报告 UnsupportedMode: %#v", instance.Status.Conditions)
				}
				if len(instance.Status.ManagedResources) != 0 {
					return fmt.Errorf("managedResources=%v，期望为空", instance.Status.ManagedResources)
				}
				return nil
			}, time.Minute, 2*time.Second).Should(Succeed())
			for _, identity := range []struct{ kind, name string }{
				{kind: "daemonset", name: daemonShardedManagedName},
				{kind: "deployment", name: daemonShardedManagedName + "-cluster"},
				{kind: "deployment", name: daemonShardedManagedName + "-unscheduled"},
				{kind: "service", name: daemonShardedManagedName},
				{kind: "service", name: daemonShardedManagedName + "-control"},
				{kind: "serviceaccount", name: daemonShardedManagedName},
				{kind: "clusterrole", name: daemonShardedManagedName},
				{kind: "clusterrolebinding", name: daemonShardedManagedName},
			} {
				Expect(objectAbsent(ctx, targetNamespace, identity.kind, identity.name)).To(Succeed())
			}
			return
		}

		By("创建一个保持未调度的 Pod，用真实指标验证 unscheduled 职责")
		pendingPod := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  namespace: %s
spec:
  nodeSelector:
    monitor-in-hand.example/never: scheduled
  containers:
  - name: pause
    image: registry.k8s.io/pause:3.9
`, unscheduledProbeName, targetNamespace)
		runWithInput(ctx, repoRoot, pendingPod, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		By("等待 node DaemonSet、两个 companion Deployment 和聚合 status 全部可用")
		var desiredNodes int32
		Eventually(func() error {
			node, err := getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", daemonShardedManagedName)
			if err != nil {
				return err
			}
			desiredNodes = node.Status.DesiredNumberScheduled
			if desiredNodes < 1 || node.Status.NumberAvailable != desiredNodes {
				return fmt.Errorf("DaemonSet available=%d desired=%d", node.Status.NumberAvailable, desiredNodes)
			}
			for _, name := range []string{daemonShardedManagedName + "-cluster", daemonShardedManagedName + "-unscheduled"} {
				deployment, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", name)
				if getErr != nil {
					return getErr
				}
				if deployment.Status.AvailableReplicas != 1 {
					return fmt.Errorf("Deployment %s available=%d", name, deployment.Status.AvailableReplicas)
				}
			}
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", daemonShardedInstanceName)
			if getErr != nil {
				return getErr
			}
			if instance.Status.DeploymentMode != monitoringv1alpha1.DeploymentModeDaemonSetSharded ||
				instance.Status.DesiredPods != desiredNodes+2 || instance.Status.AvailablePods != desiredNodes+2 ||
				instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady {
				return fmt.Errorf("DaemonSetSharded status 尚未收敛: %#v", instance.Status)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("验证三角色共享过滤参数、分拆 collector，并整体替换或继承 resources")
		node, err := getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", daemonShardedManagedName)
		Expect(err).NotTo(HaveOccurred())
		cluster, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", daemonShardedManagedName+"-cluster")
		Expect(err).NotTo(HaveOccurred())
		unscheduled, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", daemonShardedManagedName+"-unscheduled")
		Expect(err).NotTo(HaveOccurred())
		sharedArgs := []string{"--metric-allowlist=kube_(pod|deployment)_info", "--namespaces=" + targetNamespace}
		Expect(node.Spec.Template.Spec.Containers[0].Args).To(Equal(append([]string{"--resources=pods"}, append(sharedArgs, "--node=$(NODE_NAME)")...)))
		Expect(cluster.Spec.Template.Spec.Containers[0].Args).To(Equal(append([]string{"--resources=deployments"}, sharedArgs...)))
		Expect(unscheduled.Spec.Template.Spec.Containers[0].Args).To(Equal(append([]string{"--resources=pods"}, append(sharedArgs, `--node=""`)...)))
		Expect(node.Spec.Template.Spec.Containers[0].Resources).To(Equal(corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("72Mi")},
		}))
		Expect(cluster.Spec.Template.Spec.Containers[0].Resources).To(Equal(corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("23m")},
		}))
		Expect(unscheduled.Spec.Template.Spec.Containers[0].Resources).To(Equal(corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("11m")},
		}))

		By("验证 node 与 control 两组 Endpoints，且 control 明确包含两个不同职责的 companion Pod")
		nodePodNames := endpointPodNames(ctx, daemonShardedManagedName)
		controlPodNames := endpointPodNames(ctx, daemonShardedManagedName+"-control")
		Expect(nodePodNames).To(HaveLen(int(desiredNodes)))
		Expect(controlPodNames).To(HaveLen(2))
		Expect(controlPodNames[0] + " " + controlPodNames[1]).To(SatisfyAll(
			ContainSubstring(daemonShardedManagedName+"-cluster-"),
			ContainSubstring(daemonShardedManagedName+"-unscheduled-"),
		))

		By("直接读取两个 companion Pod，确认 control endpoints 暴露不同指标族")
		for _, podName := range controlPodNames {
			if strings.HasPrefix(podName, daemonShardedManagedName+"-cluster-") {
				assertTargetMetricsBody(ctx, "pod/"+podName, 8080, func(body []byte) error {
					if !strings.Contains(string(body), "kube_deployment_info") || strings.Contains(string(body), "kube_pod_info") {
						return fmt.Errorf("cluster companion 指标族不符合预期")
					}
					return nil
				})
			} else {
				assertTargetMetricsBody(ctx, "pod/"+podName, 8080, func(body []byte) error {
					if !strings.Contains(string(body), `pod="`+unscheduledProbeName+`"`) || strings.Contains(string(body), "kube_deployment_info") {
						return fmt.Errorf("unscheduled companion 未仅暴露未调度 Pod 指标")
					}
					return nil
				})
			}
		}

		By("切回默认 Deployment，再切回 DaemonSetSharded，验证往返过程最终只保留目标拓扑")
		deploymentManifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
`, daemonShardedInstanceName, targetNamespace)
		runWithInput(ctx, repoRoot, deploymentManifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		Eventually(func() error {
			deployment, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", daemonShardedManagedName)
			if getErr != nil || deployment.Status.AvailableReplicas != 1 {
				return fmt.Errorf("默认 Deployment 尚未可用: %v", getErr)
			}
			if err := objectAbsent(ctx, targetNamespace, "daemonset", daemonShardedManagedName); err != nil {
				return err
			}
			return objectAbsent(ctx, targetNamespace, "service", daemonShardedManagedName+"-control")
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		Eventually(func() error {
			if _, getErr := getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", daemonShardedManagedName); getErr != nil {
				return getErr
			}
			return objectAbsent(ctx, targetNamespace, "deployment", daemonShardedManagedName)
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
	}, SpecTimeout(20*time.Minute))
})

func daemonShardedScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"001": "E2E-119-KSM-GH57-001",
		},
		"1.28": {
			"001": "E2E-128-KSM-GH57-001",
		},
	}
	if versionIDs, supported := ids[os.Getenv("KUBERNETES_VERSION")]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-KSM-GH57-" + sequence
}

func endpointPodNames(ctx SpecContext, serviceName string) []string {
	var podNames []string
	Eventually(func() error {
		endpoints, err := getObject[corev1.Endpoints](ctx, targetNamespace, "endpoints", serviceName)
		if err != nil {
			return err
		}
		podNames = podNames[:0]
		for _, subset := range endpoints.Subsets {
			for _, address := range subset.Addresses {
				if address.TargetRef != nil && address.TargetRef.Kind == "Pod" {
					podNames = append(podNames, address.TargetRef.Name)
				}
			}
		}
		if len(podNames) == 0 {
			return fmt.Errorf("Service %s 尚无 ready Pod endpoints", serviceName)
		}
		return nil
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	sort.Strings(podNames)
	return podNames
}
