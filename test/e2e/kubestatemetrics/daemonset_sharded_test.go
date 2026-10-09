package kubestatemetrics_test

import (
	"context"
	"fmt"
	"os"
	"slices"
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
	defaultDaemonInstanceName = "default-daemonset-sharded"
	defaultDaemonManagedName  = "ksm-default-daemonset-sharded"
	defaultUnscheduledProbe   = "gh81-unscheduled-probe"
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
    patterns: ['kube_(pod_info|deployment_created)']
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
		sharedArgs := []string{"--metric-allowlist=kube_(pod_info|deployment_created)", "--namespaces=" + targetNamespace}
		capabilityArgs := []string{"--logtostderr=true", "--v=0", "--enable-gzip-encoding=true", "--use-apiserver-cache=true"}
		Expect(node.Spec.Template.Spec.Containers[0].Args).To(Equal(append(append([]string{"--resources=pods"}, sharedArgs...), append([]string{"--node=$(NODE_NAME)"}, capabilityArgs...)...)))
		Expect(cluster.Spec.Template.Spec.Containers[0].Args).To(Equal(append(append([]string{"--resources=deployments"}, sharedArgs...), capabilityArgs...)))
		Expect(unscheduled.Spec.Template.Spec.Containers[0].Args).To(Equal(append(append([]string{"--resources=pods"}, sharedArgs...), append([]string{`--node=""`}, capabilityArgs...)...)))
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
		endpointPodNames(ctx, daemonShardedManagedName, int(desiredNodes))
		controlPodNames := endpointPodNames(ctx, daemonShardedManagedName+"-control", 2,
			daemonShardedManagedName+"-cluster-",
			daemonShardedManagedName+"-unscheduled-",
		)

		By("直接读取两个 companion Pod，确认 control endpoints 暴露不同指标族")
		for _, podName := range controlPodNames {
			if strings.HasPrefix(podName, daemonShardedManagedName+"-cluster-") {
				assertTargetMetricsBody(ctx, "pod/"+podName, 8080, func(body []byte) error {
					if !strings.Contains(string(body), "kube_deployment_created") || strings.Contains(string(body), "kube_pod_info") {
						return fmt.Errorf("cluster companion 指标族不符合预期")
					}
					return nil
				})
			} else {
				assertTargetMetricsBody(ctx, "pod/"+podName, 8080, func(body []byte) error {
					if !strings.Contains(string(body), `pod="`+unscheduledProbeName+`"`) || strings.Contains(string(body), "kube_deployment_created") {
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

	It(fmt.Sprintf("[%s] Default 保持三角色显式职责边界与版本拒绝", defaultDaemonScenarioID()), func(ctx SpecContext) {
		By("提交显式 Default 的 DaemonSetSharded 受管实例")
		manifest := fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  deployment:
    mode: DaemonSetSharded
    daemonSetSharded: {}
  collectors:
    mode: Default
`, defaultDaemonInstanceName, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

		if active.kubernetes == "1.19" {
			By("等待 UnsupportedMode，并验证 Default 不改变零写入拒绝语义")
			Eventually(func() error {
				instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", defaultDaemonInstanceName)
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
				{kind: "daemonset", name: defaultDaemonManagedName},
				{kind: "deployment", name: defaultDaemonManagedName + "-cluster"},
				{kind: "deployment", name: defaultDaemonManagedName + "-unscheduled"},
				{kind: "service", name: defaultDaemonManagedName},
				{kind: "service", name: defaultDaemonManagedName + "-control"},
				{kind: "serviceaccount", name: defaultDaemonManagedName},
				{kind: "clusterrole", name: defaultDaemonManagedName},
				{kind: "clusterrolebinding", name: defaultDaemonManagedName},
			} {
				Expect(objectAbsent(ctx, targetNamespace, identity.kind, identity.name)).To(Succeed())
			}
			return
		}

		By("创建保持未调度的探测 Pod，并等待三角色 workload 与 status 就绪")
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
`, defaultUnscheduledProbe, targetNamespace)
		runWithInput(ctx, repoRoot, pendingPod, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		var desiredNodes int32
		Eventually(func() error {
			node, err := getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", defaultDaemonManagedName)
			if err != nil {
				return err
			}
			desiredNodes = node.Status.DesiredNumberScheduled
			if desiredNodes < 1 || node.Status.NumberAvailable != desiredNodes {
				return fmt.Errorf("DaemonSet available=%d desired=%d", node.Status.NumberAvailable, desiredNodes)
			}
			for _, name := range []string{defaultDaemonManagedName + "-cluster", defaultDaemonManagedName + "-unscheduled"} {
				deployment, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", name)
				if getErr != nil || deployment.Status.AvailableReplicas != 1 {
					return fmt.Errorf("Deployment %s 尚未可用: %v", name, getErr)
				}
			}
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", defaultDaemonInstanceName)
			if getErr != nil {
				return getErr
			}
			if instance.Status.DesiredPods != desiredNodes+2 || instance.Status.AvailablePods != desiredNodes+2 ||
				instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady {
				return fmt.Errorf("Default DaemonSetSharded status 尚未收敛: %#v", instance.Status)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())

		By("验证 node 与 unscheduled 只显式选择 pods，cluster 精确选择其余 27 项")
		node, err := getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", defaultDaemonManagedName)
		Expect(err).NotTo(HaveOccurred())
		cluster, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", defaultDaemonManagedName+"-cluster")
		Expect(err).NotTo(HaveOccurred())
		unscheduled, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", defaultDaemonManagedName+"-unscheduled")
		Expect(err).NotTo(HaveOccurred())
		Expect(node.Spec.Template.Spec.Containers[0].Image).To(Equal(active.image))
		Expect(cluster.Spec.Template.Spec.Containers[0].Image).To(Equal(active.image))
		Expect(unscheduled.Spec.Template.Spec.Containers[0].Image).To(Equal(active.image))
		Expect(node.Spec.Template.Spec.Containers[0].Args).To(SatisfyAll(
			HaveLen(3), HaveExactElements("--resources=pods", HavePrefix("--namespaces="), "--node=$(NODE_NAME)"),
		))
		Expect(cluster.Spec.Template.Spec.Containers[0].Args).To(SatisfyAll(
			HaveLen(2), HaveExactElements("--resources="+strings.Join(sharedCollectorCatalogWithoutPods(), ","), HavePrefix("--namespaces=")),
		))
		Expect(unscheduled.Spec.Template.Spec.Containers[0].Args).To(SatisfyAll(
			HaveLen(3), HaveExactElements("--resources=pods", HavePrefix("--namespaces="), `--node=""`),
		))
		var sharedNamespaces []string
		for _, args := range [][]string{
			node.Spec.Template.Spec.Containers[0].Args,
			cluster.Spec.Template.Spec.Containers[0].Args,
			unscheduled.Spec.Template.Spec.Containers[0].Args,
		} {
			effectiveNamespaces, namespaceErr := namespacesFromArgs(args)
			Expect(namespaceErr).NotTo(HaveOccurred())
			if sharedNamespaces == nil {
				sharedNamespaces = effectiveNamespaces
				continue
			}
			Expect(effectiveNamespaces).To(Equal(sharedNamespaces))
		}
		Expect(sharedNamespaces).To(ContainElement(targetNamespace))
		instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", defaultDaemonInstanceName)
		Expect(err).NotTo(HaveOccurred())
		assertOwnerReference(node, string(instance.UID))
		assertOwnerReference(cluster, string(instance.UID))
		assertOwnerReference(unscheduled, string(instance.UID))
		Expect(instance.Status.KubeStateMetricsVersion).To(Equal(active.version))
		Expect(instance.Status.DeploymentMode).To(Equal(monitoringv1alpha1.DeploymentModeDaemonSetSharded))
		Expect(instance.Status.EffectiveCollectors).To(Equal(sharedCollectorCatalog()))
		Expect(instance.Status.ManagedResources).To(HaveLen(8))

		By("从两组 endpoints 读取真实指标，验证三角色不重复采集")
		nodePodNames := endpointPodNames(ctx, defaultDaemonManagedName, int(desiredNodes))
		scheduledPodName := nodePodNames[0]
		for _, podName := range nodePodNames {
			assertTargetMetricsBody(ctx, "pod/"+podName, 8080, func(body []byte) error {
				metrics := string(body)
				if !strings.Contains(metrics, "kube_pod_info") || strings.Contains(metrics, `pod="`+defaultUnscheduledProbe+`"`) || strings.Contains(metrics, "kube_deployment_created") {
					return fmt.Errorf("node 角色 Pod %s 指标边界不符合预期", podName)
				}
				return nil
			})
		}
		controlPodNames := endpointPodNames(ctx, defaultDaemonManagedName+"-control", 2,
			defaultDaemonManagedName+"-cluster-", defaultDaemonManagedName+"-unscheduled-")
		for _, podName := range controlPodNames {
			assertTargetMetricsBody(ctx, "pod/"+podName, 8080, func(body []byte) error {
				metrics := string(body)
				if strings.HasPrefix(podName, defaultDaemonManagedName+"-cluster-") {
					if !strings.Contains(metrics, "kube_deployment_created") || strings.Contains(metrics, "kube_pod_info") {
						return fmt.Errorf("cluster 角色指标边界不符合预期")
					}
				} else if !strings.Contains(metrics, `pod="`+defaultUnscheduledProbe+`"`) ||
					strings.Contains(metrics, `pod="`+scheduledPodName+`"`) || strings.Contains(metrics, "kube_deployment_created") {
					return fmt.Errorf("unscheduled 角色指标边界不符合预期")
				}
				return nil
			})
		}
	}, SpecTimeout(20*time.Minute))

	if os.Getenv("KUBERNETES_VERSION") == "1.28" {
		It(fmt.Sprintf("[%s] DaemonSetSharded 三角色启用 gzip 与 API cache 并收敛旧模板", gh82ScenarioID("003")), func(ctx SpecContext) {
			const gh82InstanceName = "gh82-daemonset-sharded"
			const gh82ManagedName = "ksm-gh82-daemonset-sharded"
			const gh82ProbeName = "gh82-unscheduled-probe"
			By("提交显式过滤与三角色 resources 的 DaemonSetSharded 实例")
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
          memory: 80Mi
      clusterPodResources:
        requests:
          cpu: 29m
      unscheduledPodResources:
        requests:
          memory: 36Mi
  collectors:
    mode: Include
    names: [pods, deployments]
  metricFilter:
    mode: Allow
    patterns: ['kube_(pod_info|deployment_created)']
  namespaceFilter:
    mode: Include
    names: [%s]
`, gh82InstanceName, targetNamespace, targetNamespace)
			runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
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
`, gh82ProbeName, targetNamespace)
			runWithInput(ctx, repoRoot, pendingPod, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")

			By("等待 node、cluster、unscheduled 全部可用并记录 CR generation")
			var desiredNodes int32
			Eventually(func() error {
				node, err := getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", gh82ManagedName)
				if err != nil {
					return err
				}
				desiredNodes = node.Status.DesiredNumberScheduled
				if desiredNodes < 1 || node.Status.NumberAvailable != desiredNodes || node.Status.UpdatedNumberScheduled != desiredNodes {
					return fmt.Errorf("node DaemonSet available=%d updated=%d desired=%d", node.Status.NumberAvailable, node.Status.UpdatedNumberScheduled, desiredNodes)
				}
				for _, name := range []string{gh82ManagedName + "-cluster", gh82ManagedName + "-unscheduled"} {
					deployment, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", name)
					if getErr != nil || deployment.Status.AvailableReplicas != 1 || deployment.Status.UpdatedReplicas != 1 {
						return fmt.Errorf("Deployment %s 尚未可用: %v", name, getErr)
					}
				}
				instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", gh82InstanceName)
				if getErr != nil {
					return getErr
				}
				if instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady ||
					instance.Status.DesiredPods != desiredNodes+2 || instance.Status.AvailablePods != desiredNodes+2 {
					return fmt.Errorf("DaemonSetSharded status 尚未收敛: %#v", instance.Status)
				}
				return nil
			}, 5*time.Minute, 2*time.Second).Should(Succeed())

			By("验证三种 workload 模板按部署模式启用 cache，并保留角色职责与 resources")
			shardedCapabilityArgs := []string{"--logtostderr=true", "--v=0", "--enable-gzip-encoding=true", "--use-apiserver-cache=true"}
			wantNode := append([]string{"--resources=pods", "--metric-allowlist=kube_(pod_info|deployment_created)", "--namespaces=" + targetNamespace, "--node=$(NODE_NAME)"}, shardedCapabilityArgs...)
			wantCluster := append([]string{"--resources=deployments", "--metric-allowlist=kube_(pod_info|deployment_created)", "--namespaces=" + targetNamespace}, shardedCapabilityArgs...)
			wantUnscheduled := append([]string{"--resources=pods", "--metric-allowlist=kube_(pod_info|deployment_created)", "--namespaces=" + targetNamespace, `--node=""`}, shardedCapabilityArgs...)
			node, err := getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", gh82ManagedName)
			Expect(err).NotTo(HaveOccurred())
			cluster, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", gh82ManagedName+"-cluster")
			Expect(err).NotTo(HaveOccurred())
			unscheduled, err := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", gh82ManagedName+"-unscheduled")
			Expect(err).NotTo(HaveOccurred())
			Expect(node.Spec.Template.Spec.Containers[0].Args).To(Equal(wantNode))
			Expect(cluster.Spec.Template.Spec.Containers[0].Args).To(Equal(wantCluster))
			Expect(unscheduled.Spec.Template.Spec.Containers[0].Args).To(Equal(wantUnscheduled))
			for _, args := range [][]string{node.Spec.Template.Spec.Containers[0].Args, cluster.Spec.Template.Spec.Containers[0].Args, unscheduled.Spec.Template.Spec.Containers[0].Args} {
				assertGH82Args(args, true)
			}
			Expect(node.Spec.Template.Spec.Containers[0].Resources).To(Equal(corev1.ResourceRequirements{
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("80Mi")},
			}))
			Expect(cluster.Spec.Template.Spec.Containers[0].Resources).To(Equal(corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("29m")},
			}))
			Expect(unscheduled.Spec.Template.Spec.Containers[0].Resources).To(Equal(corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("36Mi")},
			}))
			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", gh82InstanceName)
			Expect(err).NotTo(HaveOccurred())
			generation := instance.Generation
			Expect(instance.Status).To(SatisfyAll(
				HaveField("ObservedGeneration", generation),
				HaveField("DeploymentMode", monitoringv1alpha1.DeploymentModeDaemonSetSharded),
				HaveField("EffectiveCollectors", []string{"pods", "deployments"}),
			))

			nodePodNames := endpointPodNames(ctx, gh82ManagedName, int(desiredNodes))
			controlPodNames := endpointPodNames(ctx, gh82ManagedName+"-control", 2,
				gh82ManagedName+"-cluster-", gh82ManagedName+"-unscheduled-")
			allInitialPods := append(append([]string(nil), nodePodNames...), controlPodNames...)
			initialUIDs := podUIDs(ctx, allInitialPods)
			expectedRoleArgs := func(podName string) []string {
				switch {
				case strings.HasPrefix(podName, gh82ManagedName+"-cluster-"):
					return wantCluster
				case strings.HasPrefix(podName, gh82ManagedName+"-unscheduled-"):
					return wantUnscheduled
				default:
					return wantNode
				}
			}
			verifyRoleMetrics := func(podName string) func([]byte) error {
				return func(body []byte) error {
					metrics := string(body)
					switch {
					case strings.HasPrefix(podName, gh82ManagedName+"-cluster-"):
						if !strings.Contains(metrics, "kube_deployment_created") || strings.Contains(metrics, "kube_pod_info") {
							return fmt.Errorf("cluster companion 指标族不符合职责")
						}
					case strings.HasPrefix(podName, gh82ManagedName+"-unscheduled-"):
						if !strings.Contains(metrics, `pod="`+gh82ProbeName+`"`) || strings.Contains(metrics, "kube_deployment_created") {
							return fmt.Errorf("unscheduled companion 未仅暴露未调度 Pod 指标")
						}
					default:
						if !strings.Contains(metrics, "kube_pod_info") || strings.Contains(metrics, "kube_deployment_created") {
							return fmt.Errorf("node 采集进程指标族不符合职责")
						}
					}
					return nil
				}
			}
			for _, podName := range allInitialPods {
				pod, getErr := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
				Expect(getErr).NotTo(HaveOccurred())
				Expect(pod.Spec.Containers[0].Args).To(Equal(expectedRoleArgs(podName)))
				assertGH82Args(pod.Spec.Containers[0].Args, true)
				assertPodMetricsNegotiation(ctx, podName, verifyRoleMetrics(podName))
			}

			By("暂停 Operator，同时构造三个缺少四个参数的升级前 workload 模板")
			operatorPaused := false
			DeferCleanup(func(cleanupCtx SpecContext) {
				if operatorPaused {
					resumeOperator(cleanupCtx)
				}
			}, NodeTimeout(5*time.Minute))
			operatorPaused = true
			pauseOperator(ctx)
			node, err = getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", gh82ManagedName)
			Expect(err).NotTo(HaveOccurred())
			cluster, err = getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", gh82ManagedName+"-cluster")
			Expect(err).NotTo(HaveOccurred())
			unscheduled, err = getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", gh82ManagedName+"-unscheduled")
			Expect(err).NotTo(HaveOccurred())
			node.Spec.Template.Spec.Containers[0].Args = withoutGH82E2EArgs(node.Spec.Template.Spec.Containers[0].Args)
			cluster.Spec.Template.Spec.Containers[0].Args = withoutGH82E2EArgs(cluster.Spec.Template.Spec.Containers[0].Args)
			unscheduled.Spec.Template.Spec.Containers[0].Args = withoutGH82E2EArgs(unscheduled.Spec.Template.Spec.Containers[0].Args)
			replaceObject(ctx, node)
			replaceObject(ctx, cluster)
			replaceObject(ctx, unscheduled)
			for _, target := range []string{"daemonset/" + gh82ManagedName, "deployment/" + gh82ManagedName + "-cluster", "deployment/" + gh82ManagedName + "-unscheduled"} {
				run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
					"rollout", "status", target, "--timeout=5m")
			}
			baselineNodePods := endpointPodNames(ctx, gh82ManagedName, int(desiredNodes))
			baselineControlPods := endpointPodNames(ctx, gh82ManagedName+"-control", 2,
				gh82ManagedName+"-cluster-", gh82ManagedName+"-unscheduled-")
			baselinePods := append(append([]string(nil), baselineNodePods...), baselineControlPods...)
			baselineUIDs := podUIDs(ctx, baselinePods)
			Expect(baselineUIDs).NotTo(Equal(initialUIDs))
			for _, podName := range baselinePods {
				pod, getErr := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
				Expect(getErr).NotTo(HaveOccurred())
				for _, removed := range []string{"--logtostderr=true", "--v=0", "--enable-gzip-encoding=true", "--use-apiserver-cache=true"} {
					Expect(pod.Spec.Containers[0].Args).NotTo(ContainElement(removed))
				}
			}

			By("恢复 Operator，验证三个角色在 CR generation 不变时更新全部 Pod")
			resumeOperator(ctx)
			operatorPaused = false
			Eventually(func() error {
				currentNode, getErr := getObject[appsv1.DaemonSet](ctx, targetNamespace, "daemonset", gh82ManagedName)
				if getErr != nil {
					return getErr
				}
				currentCluster, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", gh82ManagedName+"-cluster")
				if getErr != nil {
					return getErr
				}
				currentUnscheduled, getErr := getObject[appsv1.Deployment](ctx, targetNamespace, "deployment", gh82ManagedName+"-unscheduled")
				if getErr != nil {
					return getErr
				}
				if !slices.Equal(currentNode.Spec.Template.Spec.Containers[0].Args, wantNode) || currentNode.Status.NumberAvailable != desiredNodes || currentNode.Status.UpdatedNumberScheduled != desiredNodes ||
					!slices.Equal(currentCluster.Spec.Template.Spec.Containers[0].Args, wantCluster) || currentCluster.Status.AvailableReplicas != 1 || currentCluster.Status.UpdatedReplicas != 1 ||
					!slices.Equal(currentUnscheduled.Spec.Template.Spec.Containers[0].Args, wantUnscheduled) || currentUnscheduled.Status.AvailableReplicas != 1 || currentUnscheduled.Status.UpdatedReplicas != 1 {
					return fmt.Errorf("三角色 workload 尚未全部收敛")
				}
				currentInstance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", gh82InstanceName)
				if getErr != nil {
					return getErr
				}
				if currentInstance.Generation != generation || currentInstance.Status.ObservedGeneration != generation ||
					currentInstance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady || currentInstance.Status.AvailablePods != desiredNodes+2 ||
					!slices.Equal(currentInstance.Status.EffectiveCollectors, []string{"pods", "deployments"}) ||
					!conditionIsTrue(currentInstance.Status.Conditions, "Ready", generation) ||
					!conditionIsTrue(currentInstance.Status.Conditions, "Available", generation) {
					return fmt.Errorf("CR/status 尚未收敛: generation=%d status=%#v", currentInstance.Generation, currentInstance.Status)
				}
				return nil
			}, 5*time.Minute, 2*time.Second).Should(Succeed())
			currentNodePods := endpointPodNames(ctx, gh82ManagedName, int(desiredNodes))
			currentControlPods := endpointPodNames(ctx, gh82ManagedName+"-control", 2,
				gh82ManagedName+"-cluster-", gh82ManagedName+"-unscheduled-")
			currentPods := append(append([]string(nil), currentNodePods...), currentControlPods...)
			expectPodsReplaced(ctx, currentPods, baselineUIDs)
			for _, podName := range currentPods {
				pod, getErr := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
				Expect(getErr).NotTo(HaveOccurred())
				Expect(pod.Spec.Containers[0].Args).To(Equal(expectedRoleArgs(podName)))
				assertGH82Args(pod.Spec.Containers[0].Args, true)
				assertPodMetricsNegotiation(ctx, podName, verifyRoleMetrics(podName))
			}
		}, SpecTimeout(30*time.Minute))
	}
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

func defaultDaemonScenarioID() string {
	if os.Getenv("KUBERNETES_VERSION") == "1.19" {
		return "E2E-119-KSM-GH81-003"
	}
	if os.Getenv("KUBERNETES_VERSION") == "1.28" {
		return "E2E-128-KSM-GH81-003"
	}
	return "E2E-UNSUPPORTED-KSM-GH81-003"
}

func sharedCollectorCatalogWithoutPods() []string {
	collectors := make([]string, 0, len(sharedCollectorCatalog())-1)
	for _, collector := range sharedCollectorCatalog() {
		if collector != "pods" {
			collectors = append(collectors, collector)
		}
	}
	return collectors
}

func endpointPodNames(ctx SpecContext, serviceName string, expectedCount int, expectedPrefixes ...string) []string {
	var podNames []string
	Eventually(func() error {
		var err error
		podNames, err = readyEndpointPodNames(ctx, serviceName)
		if err != nil {
			return err
		}
		if len(podNames) != expectedCount {
			return fmt.Errorf("Service %s ready Pod endpoints=%v，期望数量 %d", serviceName, podNames, expectedCount)
		}
		for _, prefix := range expectedPrefixes {
			found := false
			for _, podName := range podNames {
				if strings.HasPrefix(podName, prefix) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("Service %s ready Pod endpoints=%v，缺少前缀 %q", serviceName, podNames, prefix)
			}
		}
		return nil
	}, 2*time.Minute, 2*time.Second).Should(Succeed())
	sort.Strings(podNames)
	return podNames
}

func readyEndpointPodNames(ctx context.Context, serviceName string) ([]string, error) {
	endpoints, err := getObject[corev1.Endpoints](ctx, targetNamespace, "endpoints", serviceName)
	if err != nil {
		return nil, err
	}
	podNames := make([]string, 0)
	for _, subset := range endpoints.Subsets {
		for _, address := range subset.Addresses {
			if address.TargetRef != nil && address.TargetRef.Kind == "Pod" {
				podNames = append(podNames, address.TargetRef.Name)
			}
		}
	}
	sort.Strings(podNames)
	return podNames, nil
}
