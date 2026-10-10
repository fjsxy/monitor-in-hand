package kubestatemetrics_test

import (
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
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	shardedInstanceName = "statefulset-sharded"
	shardedManagedName  = "ksm-statefulset-sharded"
	invalidShardsName   = "invalid-statefulset-shards"
	defaultOmittedName  = "default-statefulset-omitted"
	defaultExplicitName = "default-statefulset-explicit"
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
			"--logtostderr=true",
			"--v=0",
			"--enable-gzip-encoding=true",
			"--use-apiserver-cache=true",
		}
		Expect(container.Args).To(Equal(expectedArgs))
		expectedTemplateResources := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("15m")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("96Mi")},
		}
		Expect(container.Resources).To(Equal(expectedTemplateResources))
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
		// Pod API 会把显式 limit 复制为同名的缺省 request；完整断言默认化后的有效资源，避免误认为 Operator 改写了声明。
		expectedPodResources := expectedTemplateResources.DeepCopy()
		expectedPodResources.Requests[corev1.ResourceMemory] = resource.MustParse("96Mi")
		for ordinal := 0; ordinal < 2; ordinal++ {
			podName := fmt.Sprintf("%s-%d", shardedManagedName, ordinal)
			pod, err := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
			Expect(err).NotTo(HaveOccurred())
			Expect(pod.Spec.Containers).To(HaveLen(1))
			Expect(pod.Spec.Containers[0].Args).To(Equal(expectedArgs))
			Expect(pod.Spec.Containers[0].Resources).To(Equal(*expectedPodResources))

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

	It(fmt.Sprintf("[%s] Default 与省略 collectors 均依赖上游默认集合", defaultStatefulScenarioID()), func(ctx SpecContext) {
		By("分别提交省略 collectors 与显式 Default 的两分片实例")
		manifests := []struct {
			name       string
			collectors string
		}{
			{name: defaultOmittedName},
			{name: defaultExplicitName, collectors: "  collectors:\n    mode: Default\n"},
		}
		for _, current := range manifests {
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
%s`, current.name, targetNamespace, current.collectors)
			runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		}

		By("等待两个 StatefulSet、全部 shard Pod、endpoints 和 status 收敛")
		for _, current := range manifests {
			waitForShardedTopology(ctx, current.name, "ksm-"+current.name, 2)
		}

		By("验证所有 shard 完全省略 resources，同时保留分片参数和固定有效集合")
		effectiveCollectorSets := make([][]string, 0, len(manifests))
		for _, current := range manifests {
			managedName := "ksm-" + current.name
			statefulSet, err := getObject[appsv1.StatefulSet](ctx, targetNamespace, "statefulset", managedName)
			Expect(err).NotTo(HaveOccurred())
			Expect(statefulSet.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(statefulSet.Spec.Template.Spec.Containers[0].Image).To(Equal(active.image))
			args := statefulSet.Spec.Template.Spec.Containers[0].Args
			Expect(args).NotTo(ContainElement(HavePrefix("--resources")))
			Expect(args).To(ContainElements("--pod=$(POD_NAME)", "--pod-namespace=$(POD_NAMESPACE)"))
			effectiveNamespaces, err := namespacesFromArgs(args)
			Expect(err).NotTo(HaveOccurred())
			Expect(effectiveNamespaces).To(ContainElement(targetNamespace))

			instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", current.name)
			Expect(err).NotTo(HaveOccurred())
			Expect(instance.Status.KubeStateMetricsVersion).To(Equal(active.version))
			Expect(instance.Status.EffectiveCollectors).To(Equal(sharedCollectorCatalog()))
			Expect(instance.Status.ManagedResources).To(ConsistOf(
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "v1", Kind: "ServiceAccount", Namespace: targetNamespace, Name: managedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole", Name: managedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding", Name: managedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "v1", Kind: "Service", Namespace: targetNamespace, Name: managedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role", Namespace: targetNamespace, Name: managedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding", Namespace: targetNamespace, Name: managedName},
				monitoringv1alpha1.ManagedResourceReference{APIVersion: "apps/v1", Kind: "StatefulSet", Namespace: targetNamespace, Name: managedName},
			))
			effectiveCollectorSets = append(effectiveCollectorSets, append([]string(nil), instance.Status.EffectiveCollectors...))

			By("读取七类直接受管资源并验证稳定身份与当前 CR 所有权")
			serviceAccount, err := getObject[corev1.ServiceAccount](ctx, targetNamespace, "serviceaccount", managedName)
			Expect(err).NotTo(HaveOccurred())
			clusterRole, err := getObject[rbacv1.ClusterRole](ctx, "", "clusterrole", managedName)
			Expect(err).NotTo(HaveOccurred())
			clusterRoleBinding, err := getObject[rbacv1.ClusterRoleBinding](ctx, "", "clusterrolebinding", managedName)
			Expect(err).NotTo(HaveOccurred())
			service, err := getObject[corev1.Service](ctx, targetNamespace, "service", managedName)
			Expect(err).NotTo(HaveOccurred())
			role, err := getObject[rbacv1.Role](ctx, targetNamespace, "role", managedName)
			Expect(err).NotTo(HaveOccurred())
			roleBinding, err := getObject[rbacv1.RoleBinding](ctx, targetNamespace, "rolebinding", managedName)
			Expect(err).NotTo(HaveOccurred())
			for _, object := range []metav1.Object{
				serviceAccount, clusterRole, clusterRoleBinding, service, role, roleBinding, statefulSet,
			} {
				assertOwnerReference(object, string(instance.UID))
			}

			podNames := endpointPodNames(ctx, managedName, 2, managedName+"-0", managedName+"-1")
			for ordinal, podName := range podNames {
				pod, err := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
				Expect(err).NotTo(HaveOccurred())
				Expect(pod.Spec.Containers[0].Args).NotTo(ContainElement(HavePrefix("--resources")))
				assertTargetMetricsBody(ctx, "pod/"+podName, 8081, func(body []byte) error {
					if !strings.Contains(string(body), fmt.Sprintf("kube_state_metrics_total_shards 2")) ||
						!strings.Contains(string(body), fmt.Sprintf(`kube_state_metrics_shard_ordinal{shard_ordinal="%d"} %d`, ordinal, ordinal)) {
						return fmt.Errorf("Pod %s 的分片 telemetry 不完整", podName)
					}
					return nil
				})
			}
		}
		Expect(effectiveCollectorSets).To(HaveLen(2))
		Expect(effectiveCollectorSets[0]).To(Equal(effectiveCollectorSets[1]))

		By("等待两种输入的实际采集指标族在 collector 异步启动后收敛为相同集合")
		Eventually(func() error {
			// 每轮都重新抓取两个实例的全部 shard，避免把前一轮的瞬时样本累加到当前集合。
			actualMetricFamilies := make([][]string, 0, len(manifests))
			for _, current := range manifests {
				managedName := "ksm-" + current.name
				var combinedMetrics strings.Builder
				podNames := endpointPodNames(ctx, managedName, 2, managedName+"-0", managedName+"-1")
				for _, podName := range podNames {
					assertTargetMetricsBody(ctx, "pod/"+podName, 8080, func(body []byte) error {
						combinedMetrics.Write(body)
						return nil
					})
				}
				if !strings.Contains(combinedMetrics.String(), "kube_pod_info") ||
					!strings.Contains(combinedMetrics.String(), "kube_deployment_created") {
					return fmt.Errorf("实例 %s 的默认指标尚未完整可见", current.name)
				}
				actualMetricFamilies = append(actualMetricFamilies, metricFamilyNames(combinedMetrics.String()))
			}

			omittedOnly := metricFamilyDifference(actualMetricFamilies[0], actualMetricFamilies[1])
			explicitOnly := metricFamilyDifference(actualMetricFamilies[1], actualMetricFamilies[0])
			if len(omittedOnly) != 0 || len(explicitOnly) != 0 {
				return fmt.Errorf("实际指标族尚未收敛: %s 独有=%v; %s 独有=%v",
					defaultOmittedName, omittedOnly, defaultExplicitName, explicitOnly)
			}
			return nil
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	}, SpecTimeout(20*time.Minute))

	It(fmt.Sprintf("[%s] StatefulSetSharded 全部分片启用 gzip 与 API cache 并收敛旧模板", gh82ScenarioID("002")), func(ctx SpecContext) {
		const gh82InstanceName = "gh82-statefulset-sharded"
		const gh82ManagedName = "ksm-gh82-statefulset-sharded"
		By("提交两分片且显式声明 collector、filter 与 resources 的实例")
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
      cpu: 17m
      memory: 32Mi
    limits:
      cpu: 170m
      memory: 128Mi
`, gh82InstanceName, targetNamespace, targetNamespace)
		runWithInput(ctx, repoRoot, manifest, kubectlBin, "--context", kubeContext, "apply", "--filename", "-")
		waitForShardedTopology(ctx, gh82InstanceName, gh82ManagedName, 2)

		By("枚举两个实际 shard，验证参数、资源、分片 telemetry 和 gzip 协商")
		wantArgs := []string{
			"--resources=pods", "--metric-allowlist=kube_pod_info", "--namespaces=" + targetNamespace,
			"--pod=$(POD_NAME)", "--pod-namespace=$(POD_NAMESPACE)",
			"--logtostderr=true", "--v=0", "--enable-gzip-encoding=true", "--use-apiserver-cache=true",
		}
		wantResources := corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("17m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("170m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		}
		statefulSet, err := getObject[appsv1.StatefulSet](ctx, targetNamespace, "statefulset", gh82ManagedName)
		Expect(err).NotTo(HaveOccurred())
		Expect(statefulSet.Spec.Template.Spec.Containers[0].Args).To(Equal(wantArgs))
		Expect(statefulSet.Spec.Template.Spec.Containers[0].Resources).To(Equal(wantResources))
		assertGH82Args(statefulSet.Spec.Template.Spec.Containers[0].Args, true)
		instance, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", gh82InstanceName)
		Expect(err).NotTo(HaveOccurred())
		generation := instance.Generation
		Expect(instance.Status).To(SatisfyAll(
			HaveField("ObservedGeneration", generation),
			HaveField("KubeStateMetricsVersion", active.version),
			HaveField("DeploymentMode", monitoringv1alpha1.DeploymentModeStatefulSetSharded),
			HaveField("DesiredPods", int32(2)),
			HaveField("AvailablePods", int32(2)),
			HaveField("EffectiveCollectors", []string{"pods"}),
		))
		podNames := endpointPodNames(ctx, gh82ManagedName, 2)
		initialUIDs := podUIDs(ctx, podNames)
		for ordinal, podName := range podNames {
			pod, getErr := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
			Expect(getErr).NotTo(HaveOccurred())
			Expect(pod.Spec.Containers[0].Args).To(Equal(wantArgs))
			assertGH82Args(pod.Spec.Containers[0].Args, true)
			assertPodMetricsNegotiation(ctx, podName, func(body []byte) error {
				if !strings.Contains(string(body), "kube_pod_info") {
					return fmt.Errorf("shard %s metrics 缺少 kube_pod_info", podName)
				}
				return nil
			})
			assertTargetMetricsBody(ctx, "pod/"+podName, 8081, func(body []byte) error {
				if !strings.Contains(string(body), fmt.Sprintf(`kube_state_metrics_shard_ordinal{shard_ordinal="%d"} %d`, ordinal, ordinal)) ||
					!strings.Contains(string(body), "kube_state_metrics_total_shards 2") {
					return fmt.Errorf("shard %s telemetry 不符合 ordinal=%d total=2", podName, ordinal)
				}
				return nil
			})
		}

		By("暂停 Operator 并将 StatefulSet 替换为缺少四个参数的升级前模板")
		operatorPaused := false
		DeferCleanup(func(cleanupCtx SpecContext) {
			if operatorPaused {
				restoreOperator(cleanupCtx)
			}
		}, NodeTimeout(5*time.Minute))
		operatorPaused = true
		pauseOperator(ctx)
		statefulSet, err = getObject[appsv1.StatefulSet](ctx, targetNamespace, "statefulset", gh82ManagedName)
		Expect(err).NotTo(HaveOccurred())
		statefulSet.Spec.Template.Spec.Containers[0].Args = withoutGH82E2EArgs(statefulSet.Spec.Template.Spec.Containers[0].Args)
		replaceObject(ctx, statefulSet)
		run(ctx, repoRoot, nil, kubectlBin, "--context", kubeContext, "--namespace", targetNamespace,
			"rollout", "status", "statefulset/"+gh82ManagedName, "--timeout=5m")
		baselinePodNames := endpointPodNames(ctx, gh82ManagedName, 2)
		baselineUIDs := podUIDs(ctx, baselinePodNames)
		Expect(baselineUIDs).NotTo(Equal(initialUIDs))
		for _, podName := range baselinePodNames {
			pod, getErr := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
			Expect(getErr).NotTo(HaveOccurred())
			Expect(pod.Spec.Containers[0].Args).To(Equal([]string{
				"--resources=pods", "--metric-allowlist=kube_pod_info", "--namespaces=" + targetNamespace,
				"--pod=$(POD_NAME)", "--pod-namespace=$(POD_NAMESPACE)",
			}))
		}

		By("恢复 Operator，验证两个 shard 在 CR generation 不变时全部更新")
		restoreOperator(ctx)
		operatorPaused = false
		Eventually(func() error {
			current, getErr := getObject[appsv1.StatefulSet](ctx, targetNamespace, "statefulset", gh82ManagedName)
			if getErr != nil {
				return getErr
			}
			if !slices.Equal(current.Spec.Template.Spec.Containers[0].Args, wantArgs) || current.Status.ReadyReplicas != 2 ||
				current.Status.CurrentRevision != current.Status.UpdateRevision {
				return fmt.Errorf("StatefulSet 尚未收敛: args=%v ready=%d current=%s update=%s",
					current.Spec.Template.Spec.Containers[0].Args, current.Status.ReadyReplicas, current.Status.CurrentRevision, current.Status.UpdateRevision)
			}
			currentInstance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", gh82InstanceName)
			if getErr != nil {
				return getErr
			}
			if currentInstance.Generation != generation || currentInstance.Status.ObservedGeneration != generation ||
				currentInstance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady || currentInstance.Status.DesiredPods != 2 ||
				currentInstance.Status.AvailablePods != 2 ||
				!slices.Equal(currentInstance.Status.EffectiveCollectors, []string{"pods"}) ||
				!conditionIsTrue(currentInstance.Status.Conditions, "Ready", generation) ||
				!conditionIsTrue(currentInstance.Status.Conditions, "Available", generation) {
				return fmt.Errorf("CR/status 尚未收敛: generation=%d status=%#v", currentInstance.Generation, currentInstance.Status)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
		currentPodNames := replacedEndpointPodNames(ctx, gh82ManagedName, 2, baselineUIDs)
		for ordinal, podName := range currentPodNames {
			pod, getErr := getObject[corev1.Pod](ctx, targetNamespace, "pod", podName)
			Expect(getErr).NotTo(HaveOccurred())
			Expect(pod.Spec.Containers[0].Args).To(Equal(wantArgs))
			Expect(pod.Spec.Containers[0].Resources).To(Equal(wantResources))
			assertGH82Args(pod.Spec.Containers[0].Args, true)
			assertPodMetricsNegotiation(ctx, podName, func(body []byte) error {
				if !strings.Contains(string(body), "kube_pod_info") {
					return fmt.Errorf("收敛后 shard %s metrics 缺少 kube_pod_info", podName)
				}
				return nil
			})
			assertTargetMetricsBody(ctx, "pod/"+podName, 8081, func(body []byte) error {
				if !strings.Contains(string(body), fmt.Sprintf(`kube_state_metrics_shard_ordinal{shard_ordinal="%d"} %d`, ordinal, ordinal)) ||
					!strings.Contains(string(body), "kube_state_metrics_total_shards 2") {
					return fmt.Errorf("收敛后 shard %s telemetry 不符合 ordinal=%d total=2", podName, ordinal)
				}
				return nil
			})
		}
		_, err = getObject[rbacv1.Role](ctx, targetNamespace, "role", gh82ManagedName)
		Expect(err).NotTo(HaveOccurred())
		_, err = getObject[rbacv1.RoleBinding](ctx, targetNamespace, "rolebinding", gh82ManagedName)
		Expect(err).NotTo(HaveOccurred())
	}, SpecTimeout(25*time.Minute))

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

func defaultStatefulScenarioID() string {
	if os.Getenv("KUBERNETES_VERSION") == "1.19" {
		return "E2E-119-KSM-GH81-002"
	}
	if os.Getenv("KUBERNETES_VERSION") == "1.28" {
		return "E2E-128-KSM-GH81-002"
	}
	return "E2E-UNSUPPORTED-KSM-GH81-002"
}

func metricFamilyNames(metrics string) []string {
	families := make(map[string]struct{})
	for _, line := range strings.Split(metrics, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.Fields(line)[0]
		if labelStart := strings.IndexByte(name, '{'); labelStart >= 0 {
			name = name[:labelStart]
		}
		families[name] = struct{}{}
	}
	result := make([]string, 0, len(families))
	for family := range families {
		result = append(result, family)
	}
	sort.Strings(result)
	return result
}

func metricFamilyDifference(left, right []string) []string {
	rightFamilies := make(map[string]struct{}, len(right))
	for _, family := range right {
		rightFamilies[family] = struct{}{}
	}
	difference := make([]string, 0)
	for _, family := range left {
		if _, found := rightFamilies[family]; !found {
			difference = append(difference, family)
		}
	}
	return difference
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
