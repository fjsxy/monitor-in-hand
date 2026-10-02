package operator_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	monitoringv1alpha1 "github.com/fjsxy/monitor-in-hand/api/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	probeInstance       = "gh64-probe"
	probeManaged        = "ksm-gh64-probe"
	dataInstance        = "gh64-data-plane"
	dataManaged         = "ksm-gh64-data-plane"
	rolloutInstance     = "gh64-rollout"
	rolloutManaged      = "ksm-gh64-rollout"
	directInstance      = "gh64-direct-reinstall"
	directManaged       = "ksm-gh64-direct-reinstall"
	conflictInstance    = "gh64-old-owner"
	conflictManaged     = "ksm-gh64-old-owner"
	kubeStateMetricsCRD = "kubestatemetrics.monitoring.fjsxy.github.io"
	oldOwnerUID         = "00000000-0000-0000-0000-000000000064"
)

type directUninstallSnapshot struct {
	directCRUID  types.UID
	conflictUID  types.UID
	directUIDs   map[string]types.UID
	conflictUIDs map[string]types.UID
	crdUID       types.UID
}

var directSnapshot directUninstallSnapshot

var _ = BeforeSuite(func(ctx SpecContext) {
	By("读取双版本 CI 提供的隔离集群，并安装当前工作树 Operator")
	minor := os.Getenv("KUBERNETES_VERSION")
	runID := os.Getenv("TEST_RUN_ID")
	Expect([]string{"1.19", "1.28"}).To(ContainElement(minor))
	Expect(runID).To(MatchRegexp(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`))
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	Expect(err).NotTo(HaveOccurred())
	clusterName := "mih-e2e-" + strings.ReplaceAll(minor, ".", "") + "-" + runID
	config = e2eConfig{
		kubernetesMinor: minor,
		testRunID:       runID,
		repoRoot:        repoRoot,
		kubeContext:     "kind-" + clusterName,
		clusterName:     clusterName,
		kubectl:         envOrDefault("KUBECTL", "kubectl"),
		helm:            envOrDefault("HELM_BIN", "helm"),
		docker:          envOrDefault("CONTAINER_TOOL", "docker"),
	}
	installOperator(ctx)
}, NodeTimeout(5*time.Minute))

var _ = Describe("Operator 故障恢复、滚动升级与卸载", Ordered, Serial, func() {
	It(fmt.Sprintf("[%s] API 暂时不可达时 readiness 失败、liveness 成功且同一进程恢复调谐", scenarioID("001")), func(ctx SpecContext) {
		scenario := scenarioID("001")
		By(scenario + " 创建用于证明客户端恢复调谐的受管实例")
		applyManifest(ctx, kubeStateMetricsManifest(probeInstance))
		waitForInstanceReady(ctx, probeInstance, probeManaged)

		By(scenario + " 只阻断 active manager 到 API Server 的访问")
		active, _, err := activeOperatorPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		initialStatus, err := managerStatus(active)
		Expect(err).NotTo(HaveOccurred())
		forward := startPortForward(ctx, operatorNamespace, "pod/"+active.Name, 8081)
		unblock := blockPodAPIAccess(ctx, active.Status.PodIP)
		DeferCleanup(unblock)

		By(scenario + " 直接验证 /readyz 失败而 /healthz 继续成功")
		Eventually(func() (int, error) { return forward.status("/readyz") }, 5*time.Second, 200*time.Millisecond).Should(Equal(500))
		Eventually(func() (int, error) { return forward.status("/healthz") }, 3*time.Second, 200*time.Millisecond).Should(Equal(200))
		run(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", testNamespace,
			"delete", "service/"+probeManaged, "--wait=true")

		By(scenario + " 在 renew deadline 前恢复 API，并验证同一进程重新 list/watch 后修复停机漂移")
		unblock()
		forward.stop()
		Eventually(func() error {
			pod, getErr := getObject[corev1.Pod](ctx, operatorNamespace, "pod", active.Name)
			if getErr != nil {
				return getErr
			}
			status, statusErr := managerStatus(pod)
			if statusErr != nil {
				return statusErr
			}
			if pod.UID != active.UID || status.RestartCount != initialStatus.RestartCount || status.ContainerID != initialStatus.ContainerID {
				return fmt.Errorf("临时 API 故障触发了不应发生的进程替换")
			}
			_, getErr = getObject[corev1.Service](ctx, testNamespace, "service", probeManaged)
			return getErr
		}, 2*time.Minute, time.Second).Should(Succeed())
	}, SpecTimeout(5*time.Minute))

	It(fmt.Sprintf("[%s] Lease 无法续约时旧 manager 退出并由 Deployment 重启", scenarioID("002")), func(ctx SpecContext) {
		scenario := scenarioID("002")
		By(scenario + " 再次阻断 API 直到 leader renew 失败，验证旧 manager 退出并由 Deployment 重启")
		active, _, err := activeOperatorPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		beforeLoss, err := managerStatus(active)
		Expect(err).NotTo(HaveOccurred())
		unblockAfterLoss := blockPodAPIAccess(ctx, active.Status.PodIP)
		DeferCleanup(unblockAfterLoss)
		Eventually(func() error {
			pod, getErr := getObject[corev1.Pod](ctx, operatorNamespace, "pod", active.Name)
			if getErr != nil {
				return getErr
			}
			status, statusErr := managerStatus(pod)
			if statusErr != nil {
				return statusErr
			}
			if status.RestartCount <= beforeLoss.RestartCount || status.ContainerID == beforeLoss.ContainerID {
				return fmt.Errorf("manager 尚未因 Lease 续约失败退出: restartCount=%d", status.RestartCount)
			}
			return nil
		}, 90*time.Second, time.Second).Should(Succeed())
		unblockAfterLoss()

		By(scenario + " 保存旧进程退出原因，并等待重启进程重新取得 Lease")
		logs := run(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", operatorNamespace,
			"logs", active.Name, "--container", "manager", "--previous")
		Expect(strings.ToLower(logs)).To(ContainSubstring("leader election lost"))
		Eventually(func() error {
			pod, _, activeErr := activeOperatorPod(ctx)
			if activeErr != nil {
				return activeErr
			}
			if !podReady(*pod) {
				return fmt.Errorf("重启 manager 尚未 Ready")
			}
			return nil
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	}, SpecTimeout(5*time.Minute))

	It(fmt.Sprintf("[%s] active Pod 删除时保持既有组件与 remote-write 数据面", scenarioID("003")), func(ctx SpecContext) {
		scenario := scenarioID("003")
		By(scenario + " 创建由固定 Agent 采集并 remote-write 的受管实例")
		applyManifest(ctx, kubeStateMetricsManifest(dataInstance))
		applyManifest(ctx, serviceMonitorManifest(config.testRunID))
		waitForInstanceReady(ctx, dataInstance, dataManaged)
		baselineTimestamp := querySeriesTimestamp(ctx, config.testRunID)
		beforeUIDs, err := managedUIDs(ctx, dataManaged)
		Expect(err).NotTo(HaveOccurred())
		ksmPods, err := getList[corev1.PodList](ctx, testNamespace, "pods", "app.kubernetes.io/instance="+dataInstance)
		Expect(err).NotTo(HaveOccurred())
		Expect(ksmPods.Items).To(HaveLen(1))
		ksmPodUID := ksmPods.Items[0].UID
		agentPod, err := getObject[corev1.Pod](ctx, testNamespace, "pod", "prom-agent-fixture-0")
		Expect(err).NotTo(HaveOccurred())
		agentPodUID := agentPod.UID
		agentMetrics := startPortForward(ctx, testNamespace, "pod/prom-agent-fixture-0", 9090)
		DeferCleanup(agentMetrics.stop)
		var remoteWriteBefore float64
		Eventually(func() (float64, error) {
			return agentMetrics.metricSample("prometheus_remote_storage_samples_total", `remote_name="fixture-vm"`)
		}, time.Minute, time.Second).Should(BeNumerically(">", 0))
		remoteWriteBefore, err = agentMetrics.metricSample("prometheus_remote_storage_samples_total", `remote_name="fixture-vm"`)
		Expect(err).NotTo(HaveOccurred())

		By(scenario + " 删除 active Operator Pod，并持续验证既有 workload、Service、RBAC 与 Agent 不变")
		active, oldHolder, err := activeOperatorPod(ctx)
		Expect(err).NotTo(HaveOccurred())
		run(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", operatorNamespace,
			"delete", "pod/"+active.Name, "--wait=false")
		verifyDataPlane := func() error {
			currentUIDs, getErr := managedUIDs(ctx, dataManaged)
			if getErr != nil {
				return getErr
			}
			if !reflect.DeepEqual(beforeUIDs, currentUIDs) {
				return fmt.Errorf("受管资源 UID 在 Operator failover 中变化: %v", currentUIDs)
			}
			workload, getErr := getObject[appsv1.Deployment](ctx, testNamespace, "deployment", dataManaged)
			if getErr != nil || workload.Status.AvailableReplicas != 1 {
				return fmt.Errorf("KSM workload 不可用: %v", getErr)
			}
			currentKSMPods, getErr := getList[corev1.PodList](ctx, testNamespace, "pods", "app.kubernetes.io/instance="+dataInstance)
			if getErr != nil || len(currentKSMPods.Items) != 1 || currentKSMPods.Items[0].UID != ksmPodUID || !podReady(currentKSMPods.Items[0]) {
				return fmt.Errorf("KSM Pod 被替换或不可用: %v", getErr)
			}
			agent, getErr := getObject[appsv1.StatefulSet](ctx, testNamespace, "statefulset", "prom-agent-fixture")
			if getErr != nil || agent.Status.ReadyReplicas != 1 {
				return fmt.Errorf("Agent 数据面不就绪: %v", getErr)
			}
			currentAgentPod, getErr := getObject[corev1.Pod](ctx, testNamespace, "pod", "prom-agent-fixture-0")
			if getErr != nil || currentAgentPod.UID != agentPodUID || !podReady(*currentAgentPod) {
				return fmt.Errorf("Agent Pod 被替换或不可用: %v", getErr)
			}
			return nil
		}

		By(scenario + " 持续观测到替代 Pod 获取 Lease，期间 remote-write 计数不得停止前进")
		lastRemoteWrite := remoteWriteBefore
		lastAdvance := time.Now()
		var continuityErr error
		Eventually(func() bool {
			if continuityErr != nil {
				return true
			}
			if invariantErr := verifyDataPlane(); invariantErr != nil {
				continuityErr = invariantErr
				return true
			}
			currentRemoteWrite, sampleErr := agentMetrics.metricSample("prometheus_remote_storage_samples_total", `remote_name="fixture-vm"`)
			if sampleErr != nil {
				continuityErr = sampleErr
				return true
			}
			if currentRemoteWrite > lastRemoteWrite {
				lastRemoteWrite = currentRemoteWrite
				lastAdvance = time.Now()
			}
			if time.Since(lastAdvance) >= 20*time.Second {
				continuityErr = fmt.Errorf("remote-write sample 计数停止前进")
				return true
			}
			pod, holder, activeErr := activeOperatorPod(ctx)
			return activeErr == nil && pod.UID != active.UID && holder != oldHolder && podReady(*pod)
		}, 3*time.Minute, time.Second).Should(BeTrue())
		Expect(continuityErr).NotTo(HaveOccurred())
		Expect(querySeriesTimestamp(ctx, config.testRunID)).To(BeNumerically(">", baselineTimestamp))
		Expect(lastRemoteWrite).To(BeNumerically(">", remoteWriteBefore))
	}, SpecTimeout(7*time.Minute))

	It(fmt.Sprintf("[%s] 停机期间变化在恢复后只按最新 generation 与 namespace 集合收敛", scenarioID("004")), func(ctx SpecContext) {
		scenario := scenarioID("004")
		By(scenario + " 停止 Operator 后连续修改 CR、删除受管 Service 并改变 regex 匹配集合")
		run(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", operatorNamespace,
			"scale", "deployment/"+operatorDeployment, "--replicas=0")
		Eventually(func() (int, error) {
			pods, getErr := operatorPods(ctx)
			return len(pods), getErr
		}, 2*time.Minute, time.Second).Should(Equal(0))
		staleNamespace := "gh64-stale-" + config.testRunID
		currentNamespace := "gh64-current-" + config.testRunID
		applyManifest(ctx, namespaceManifest(staleNamespace))
		applyManifest(ctx, namespaceManifest(currentNamespace))
		run(ctx, config.kubectl, "--context", config.kubeContext, "patch", "kubestatemetrics/"+dataInstance,
			"--type=merge", "--patch", fmt.Sprintf(`{"spec":{"namespaceFilter":{"mode":"Include","namePatterns":["^%s$"]}}}`, staleNamespace))
		run(ctx, config.kubectl, "--context", config.kubeContext, "patch", "kubestatemetrics/"+dataInstance,
			"--type=merge", "--patch", fmt.Sprintf(`{"spec":{"namespaceFilter":{"mode":"Include","namePatterns":["^%s$"]}}}`, currentNamespace))
		run(ctx, config.kubectl, "--context", config.kubeContext, "delete", "namespace/"+staleNamespace, "--wait=true")
		run(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", testNamespace,
			"delete", "service/"+dataManaged, "--wait=true")
		latest, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", dataInstance)
		Expect(err).NotTo(HaveOccurred())

		By(scenario + " 恢复 Operator，验证重新 list/watch 后只按最新 generation 幂等收敛")
		run(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", operatorNamespace,
			"scale", "deployment/"+operatorDeployment, "--replicas=1")
		Eventually(func() error {
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", dataInstance)
			if getErr != nil {
				return getErr
			}
			if instance.Status.ObservedGeneration != latest.Generation || instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady {
				return fmt.Errorf("尚未收敛最新 generation: observed=%d latest=%d phase=%s",
					instance.Status.ObservedGeneration, latest.Generation, instance.Status.Phase)
			}
			deployment, getErr := getObject[appsv1.Deployment](ctx, testNamespace, "deployment", dataManaged)
			if getErr != nil {
				return getErr
			}
			namespaces, getErr := deploymentNamespaces(deployment)
			if getErr != nil {
				return getErr
			}
			if len(namespaces) != 1 || namespaces[0] != currentNamespace {
				return fmt.Errorf("实际 namespace 集合=%v，期望仅 %s", namespaces, currentNamespace)
			}
			_, getErr = getObject[corev1.Service](ctx, testNamespace, "service", dataManaged)
			return getErr
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
	}, SpecTimeout(12*time.Minute))

	It(fmt.Sprintf("[%s] rolling handoff 保持单 active reconciler 且不重复资源或状态 Event", scenarioID("005")), func(ctx SpecContext) {
		scenario := scenarioID("005")
		By(scenario + " 创建独立实例并记录旧 active Pod")
		applyManifest(ctx, kubeStateMetricsManifest(rolloutInstance))
		waitForInstanceReady(ctx, rolloutInstance, rolloutManaged)
		oldPod, oldHolder, err := activeOperatorPod(ctx)
		Expect(err).NotTo(HaveOccurred())

		By(scenario + " 发布不可调度的新 Pod，验证 maxUnavailable=0 保留旧 active Pod")
		run(ctx, config.helm, "upgrade", operatorRelease, filepath.Join(config.repoRoot, "charts", "monitor-in-hand"),
			"--kube-context", config.kubeContext, "--namespace", operatorNamespace,
			"--set-string", "nodeSelector.gh64-rollout=blocked")
		Eventually(func() error {
			deployment, getErr := getObject[appsv1.Deployment](ctx, operatorNamespace, "deployment", operatorDeployment)
			if getErr != nil {
				return getErr
			}
			pods, getErr := operatorPods(ctx)
			if getErr != nil {
				return getErr
			}
			oldPresent := false
			unreadyPresent := false
			for _, pod := range pods {
				oldPresent = oldPresent || (pod.UID == oldPod.UID && podReady(pod))
				unreadyPresent = unreadyPresent || (pod.UID != oldPod.UID && !podReady(pod))
			}
			if deployment.Status.AvailableReplicas != 1 || !oldPresent || !unreadyPresent {
				return fmt.Errorf("故障 rollout 未保持旧 Pod: available=%d pods=%v", deployment.Status.AvailableReplicas, pods)
			}
			active, holder, activeErr := activeOperatorPod(ctx)
			if activeErr != nil || active.UID != oldPod.UID || holder != oldHolder {
				return fmt.Errorf("故障 rollout 改变 active reconciler: holder=%s err=%v", holder, activeErr)
			}
			return nil
		}, 2*time.Minute, time.Second).Should(Succeed())

		By(scenario + " 恢复可调度模板，并紧随 Lease holder 切换触发一次状态转换")
		baselineEvents, err := eventCount(ctx, rolloutInstance, "Suspended")
		Expect(err).NotTo(HaveOccurred())
		run(ctx, config.helm, "upgrade", operatorRelease, filepath.Join(config.repoRoot, "charts", "monitor-in-hand"),
			"--kube-context", config.kubeContext, "--namespace", operatorNamespace, "--reset-values", "--set", "logLevel=debug")
		Eventually(func() error {
			active, holder, activeErr := activeOperatorPod(ctx)
			if activeErr != nil {
				return activeErr
			}
			if active.UID == oldPod.UID || holder == oldHolder || !podReady(*active) {
				return fmt.Errorf("正常 handoff 尚未完成: holder=%s", holder)
			}
			deployment, getErr := getObject[appsv1.Deployment](ctx, operatorNamespace, "deployment", operatorDeployment)
			if getErr != nil || deployment.Status.AvailableReplicas != 1 || deployment.Status.UpdatedReplicas != 1 {
				return fmt.Errorf("rollout 尚未完成: %v", getErr)
			}
			return nil
		}, 5*time.Minute, 200*time.Millisecond).Should(Succeed())
		run(ctx, config.kubectl, "--context", config.kubeContext, "patch", "kubestatemetrics/"+rolloutInstance,
			"--type=merge", "--patch", `{"spec":{"suspended":true}}`)
		Eventually(func() error {
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", rolloutInstance)
			if getErr != nil || !conditionMatches(instance, "Available", metav1.ConditionFalse, "Suspended") {
				return fmt.Errorf("Suspended 状态尚未收敛: %v", getErr)
			}
			return nil
		}, 2*time.Minute, time.Second).Should(Succeed())
		Eventually(func() (int32, error) { return eventCount(ctx, rolloutInstance, "Suspended") }, time.Minute, time.Second).
			Should(Equal(baselineEvents + 1))

		By(scenario + " 恢复实例并验证 handoff 未产生重复资源")
		run(ctx, config.kubectl, "--context", config.kubeContext, "patch", "kubestatemetrics/"+rolloutInstance,
			"--type=merge", "--patch", `{"spec":{"suspended":false}}`)
		waitForInstanceReady(ctx, rolloutInstance, rolloutManaged)
		for _, resource := range directManagedResourceKinds() {
			list, listErr := getList[metav1.PartialObjectMetadataList](ctx, resource.namespace, resource.kind,
				"app.kubernetes.io/instance="+rolloutInstance)
			Expect(listErr).NotTo(HaveOccurred())
			Expect(list.Items).To(HaveLen(1), resource.kind)
		}
	}, SpecTimeout(8*time.Minute))

	It(fmt.Sprintf("[%s] Helm rollback 只恢复模板且不回滚 CRD 或持久化 CR", scenarioID("006")), func(ctx SpecContext) {
		scenario := scenarioID("006")
		By(scenario + " 验证 Helm rollback 只恢复模板，不回滚 CRD 或持久化 CR")
		crdBefore, err := getObject[extensionsv1.CustomResourceDefinition](ctx, "", "crd", kubeStateMetricsCRD)
		Expect(err).NotTo(HaveOccurred())
		debugRevision := latestHelmRevision(ctx)
		run(ctx, config.helm, "upgrade", operatorRelease, filepath.Join(config.repoRoot, "charts", "monitor-in-hand"),
			"--kube-context", config.kubeContext, "--namespace", operatorNamespace, "--reset-values", "--set", "logLevel=warn", "--wait", "--timeout", "180s")
		run(ctx, config.kubectl, "--context", config.kubeContext, "patch", "kubestatemetrics/"+rolloutInstance,
			"--type=merge", "--patch", `{"spec":{"suspended":true}}`)
		persistentCR, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", rolloutInstance)
		Expect(err).NotTo(HaveOccurred())
		run(ctx, config.helm, "rollback", operatorRelease, fmt.Sprintf("%d", debugRevision),
			"--kube-context", config.kubeContext, "--namespace", operatorNamespace, "--wait", "--timeout", "180s")
		rolledBack, err := getObject[appsv1.Deployment](ctx, operatorNamespace, "deployment", operatorDeployment)
		Expect(err).NotTo(HaveOccurred())
		Expect(operatorLogLevel(rolledBack)).To(Equal("debug"))
		crdAfter, err := getObject[extensionsv1.CustomResourceDefinition](ctx, "", "crd", kubeStateMetricsCRD)
		Expect(err).NotTo(HaveOccurred())
		Expect(crdAfter.UID).To(Equal(crdBefore.UID))
		Expect(crdAfter.Spec.Versions).To(Equal(crdBefore.Spec.Versions))
		currentCR, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", rolloutInstance)
		Expect(err).NotTo(HaveOccurred())
		Expect(currentCR.UID).To(Equal(persistentCR.UID))
		Expect(currentCR.Generation).To(Equal(persistentCR.Generation))
		Expect(currentCR.Spec.Suspended).NotTo(BeNil())
		Expect(*currentCR.Spec.Suspended).To(BeTrue())
	}, SpecTimeout(6*time.Minute))

	It(fmt.Sprintf("[%s] 第二个 Helm release 因稳定集群级身份冲突失败", scenarioID("007")), func(ctx SpecContext) {
		scenario := scenarioID("007")
		By(scenario + " 安装第二个 release，验证稳定 ClusterRole 身份阻止第二套控制面")
		secondNamespace := "gh64-second-" + config.testRunID
		output, installErr := runResult(ctx, config.helm, "install", "gh64-second", filepath.Join(config.repoRoot, "charts", "monitor-in-hand"),
			"--kube-context", config.kubeContext, "--namespace", secondNamespace, "--create-namespace", "--atomic", "--wait", "--timeout", "90s")
		Expect(installErr).To(HaveOccurred(), output)
		Expect(output).To(ContainSubstring("monitor-in-hand-manager-role"))
		Expect(output).To(Or(ContainSubstring("already exists"), ContainSubstring("invalid ownership metadata")))
		Eventually(func() error {
			return objectAbsent(ctx, secondNamespace, "deployment", "gh64-second-monitor-in-hand")
		}, time.Minute, time.Second).Should(Succeed())
		managerRole, err := getObject[metav1.PartialObjectMetadata](ctx, "", "clusterrole", "monitor-in-hand-manager-role")
		Expect(err).NotTo(HaveOccurred())
		Expect(managerRole.Annotations).To(HaveKeyWithValue("meta.helm.sh/release-name", operatorRelease))
		lease, err := getObject[coordinationv1.Lease](ctx, operatorNamespace, "lease", operatorLease)
		Expect(err).NotTo(HaveOccurred())
		Expect(lease.Spec.HolderIdentity).NotTo(BeNil())
	}, SpecTimeout(4*time.Minute))

	It(fmt.Sprintf("[%s] 安全卸载先完成全部 finalizer 清理，且 Helm 保留 CRD", scenarioID("008")), func(ctx SpecContext) {
		scenario := scenarioID("008")
		By(scenario + " 创建带 namespaced RBAC 的 StatefulSet 分片实例，并在 1.28 创建 DaemonSet 分片实例")
		applyManifest(ctx, shardedKubeStateMetricsManifest("gh64-safe-stateful", monitoringv1alpha1.DeploymentModeStatefulSetSharded))
		waitForInstanceStatusReady(ctx, "gh64-safe-stateful")
		if config.kubernetesMinor == "1.28" {
			applyManifest(ctx, shardedKubeStateMetricsManifest("gh64-safe-daemon", monitoringv1alpha1.DeploymentModeDaemonSetSharded))
			waitForInstanceStatusReady(ctx, "gh64-safe-daemon")
		}

		By(scenario + " 删除全部受管实例并等待 finalizer 完成")
		run(ctx, config.kubectl, "--context", config.kubeContext, "delete", "kubestatemetrics", "--all", "--wait=true", "--timeout=300s")
		Eventually(func() (int, error) {
			instances, err := getList[monitoringv1alpha1.KubeStateMetricsList](ctx, "", "kubestatemetrics")
			return len(instances.Items), err
		}, time.Minute, time.Second).Should(Equal(0))
		for _, resource := range allManagedResourceKinds() {
			list, err := getList[metav1.PartialObjectMetadataList](ctx, resource.namespace, resource.kind,
				"app.kubernetes.io/name=kube-state-metrics")
			Expect(err).NotTo(HaveOccurred())
			Expect(list.Items).To(BeEmpty(), resource.kind)
		}

		By(scenario + " 卸载 release 并验证 CRD 仍由集群保留")
		uninstallOperator(ctx)
		_, err := getObject[extensionsv1.CustomResourceDefinition](ctx, "", "crd", kubeStateMetricsCRD)
		Expect(err).NotTo(HaveOccurred())
		Expect(objectAbsent(ctx, operatorNamespace, "deployment", operatorDeployment)).To(Succeed())
	}, SpecTimeout(8*time.Minute))

	It(fmt.Sprintf("[%s] 直接 uninstall 保留 CRD、CR 和既有 workload", scenarioID("009")), func(ctx SpecContext) {
		scenario := scenarioID("009")
		By(scenario + " 兼容重装后创建正常恢复与旧 owner 冲突两个实例")
		installOperator(ctx)
		applyManifest(ctx, kubeStateMetricsManifest(directInstance))
		applyManifest(ctx, kubeStateMetricsManifest(conflictInstance))
		directCR := waitForInstanceReady(ctx, directInstance, directManaged)
		conflictCR := waitForInstanceReady(ctx, conflictInstance, conflictManaged)
		directUIDs, err := managedUIDs(ctx, directManaged)
		Expect(err).NotTo(HaveOccurred())
		conflictUIDs, err := managedUIDs(ctx, conflictManaged)
		Expect(err).NotTo(HaveOccurred())
		crdBefore, err := getObject[extensionsv1.CustomResourceDefinition](ctx, "", "crd", kubeStateMetricsCRD)
		Expect(err).NotTo(HaveOccurred())
		directSnapshot = directUninstallSnapshot{
			directCRUID: directCR.UID, conflictUID: conflictCR.UID,
			directUIDs: directUIDs, conflictUIDs: conflictUIDs, crdUID: crdBefore.UID,
		}

		By(scenario + " 直接 uninstall，并验证 CRD、CR 和既有受管资源全部保留")
		uninstallOperator(ctx)
		crdDuring, err := getObject[extensionsv1.CustomResourceDefinition](ctx, "", "crd", kubeStateMetricsCRD)
		Expect(err).NotTo(HaveOccurred())
		Expect(crdDuring.UID).To(Equal(directSnapshot.crdUID))
		for _, expected := range []struct {
			instance string
			uid      types.UID
			managed  string
			uids     map[string]types.UID
		}{{directInstance, directSnapshot.directCRUID, directManaged, directSnapshot.directUIDs}, {conflictInstance, directSnapshot.conflictUID, conflictManaged, directSnapshot.conflictUIDs}} {
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", expected.instance)
			Expect(getErr).NotTo(HaveOccurred())
			Expect(instance.UID).To(Equal(expected.uid))
			actualUIDs, getErr := managedUIDs(ctx, expected.managed)
			Expect(getErr).NotTo(HaveOccurred())
			Expect(actualUIDs).To(Equal(expected.uids))
		}
	}, SpecTimeout(7*time.Minute))

	It(fmt.Sprintf("[%s] 兼容重装按最新 generation 恢复停机期间变化", scenarioID("010")), func(ctx SpecContext) {
		scenario := scenarioID("010")
		By(scenario + " 停机时更新正常 CR、删除其 Service，并把另一个 workload 改为旧 UID owner")
		run(ctx, config.kubectl, "--context", config.kubeContext, "patch", "kubestatemetrics/"+directInstance,
			"--type=merge", "--patch", `{"spec":{"collectors":{"mode":"Include","names":["pods"]}}}`)
		run(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", testNamespace,
			"delete", "service/"+directManaged, "--wait=true")
		run(ctx, config.kubectl, "--context", config.kubeContext, "--namespace", testNamespace,
			"patch", "deployment/"+conflictManaged, "--type=json", "--patch",
			fmt.Sprintf(`[{"op":"replace","path":"/metadata/ownerReferences/0/uid","value":%q}]`, oldOwnerUID))
		latestDirect, err := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", directInstance)
		Expect(err).NotTo(HaveOccurred())

		By(scenario + " 重装兼容 Chart，并验证正常实例按最新 generation 恢复")
		installOperator(ctx)
		Eventually(func() error {
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", directInstance)
			if getErr != nil {
				return getErr
			}
			if instance.Status.ObservedGeneration != latestDirect.Generation || instance.Status.Phase != monitoringv1alpha1.KubeStateMetricsPhaseReady {
				return fmt.Errorf("正常实例未按最新 generation 恢复: %+v", instance.Status)
			}
			service, getErr := getObject[corev1.Service](ctx, testNamespace, "service", directManaged)
			if getErr != nil {
				return getErr
			}
			if service.UID == directSnapshot.directUIDs["service"] {
				return fmt.Errorf("被删除 Service 未以新 UID 重建")
			}
			deployment, getErr := getObject[appsv1.Deployment](ctx, testNamespace, "deployment", directManaged)
			if getErr != nil {
				return getErr
			}
			arguments, argsErr := managerArguments(deployment)
			if argsErr != nil || !contains(arguments, "--resources=pods") {
				return fmt.Errorf("最新 collector 尚未应用: %v", argsErr)
			}
			return nil
		}, 5*time.Minute, 2*time.Second).Should(Succeed())
	}, SpecTimeout(8*time.Minute))

	It(fmt.Sprintf("[%s] 兼容重装仍拒绝旧 owner UID 且不接管 workload", scenarioID("011")), func(ctx SpecContext) {
		scenario := scenarioID("011")
		By(scenario + " 验证重装后的 controller 报告旧 owner UID 冲突并保持对象原样")
		Eventually(func() error {
			instance, getErr := getObject[monitoringv1alpha1.KubeStateMetrics](ctx, "", "kubestatemetrics", conflictInstance)
			if getErr != nil {
				return getErr
			}
			if !conditionMatches(instance, "Degraded", metav1.ConditionTrue, "ResourceConflict") {
				return fmt.Errorf("旧 UID 尚未报告 ResourceConflict: %+v", instance.Status)
			}
			deployment, getErr := getObject[appsv1.Deployment](ctx, testNamespace, "deployment", conflictManaged)
			if getErr != nil {
				return getErr
			}
			if deployment.UID != directSnapshot.conflictUIDs["deployment"] || len(deployment.OwnerReferences) != 1 || string(deployment.OwnerReferences[0].UID) != oldOwnerUID {
				return fmt.Errorf("旧 UID 冲突 workload 被修改或接管")
			}
			return nil
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
	}, SpecTimeout(4*time.Minute))
})

func kubeStateMetricsManifest(name string) string {
	return fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
`, name, testNamespace)
}

func serviceMonitorManifest(testRunID string) string {
	return fmt.Sprintf(`apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: gh64-data-plane
  namespace: %s
  labels:
    monitoring.fjsxy.github.io/test-fixture: "true"
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: kube-state-metrics
      app.kubernetes.io/instance: %s
  endpoints:
    - port: http-metrics
      interval: 2s
      metricRelabelings:
        - targetLabel: mih_gh64_run_id
          replacement: %s
`, testNamespace, dataInstance, testRunID)
}

func namespaceManifest(name string) string {
	return fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", name)
}

func shardedKubeStateMetricsManifest(name string, mode monitoringv1alpha1.DeploymentMode) string {
	branch := "statefulSetSharded:\n      shards: 2"
	if mode == monitoringv1alpha1.DeploymentModeDaemonSetSharded {
		branch = "daemonSetSharded: {}"
	}
	return fmt.Sprintf(`apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: %s
spec:
  targetNamespace: %s
  deployment:
    mode: %s
    %s
`, name, testNamespace, mode, branch)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
