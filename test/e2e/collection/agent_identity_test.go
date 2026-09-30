package collection_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const gh60OtherNamespace = "gh60-agent-other"

type objectIdentity struct {
	Metadata struct {
		Name            string `json:"name"`
		UID             string `json:"uid"`
		Generation      int64  `json:"generation"`
		OwnerReferences []struct {
			APIVersion         string `json:"apiVersion"`
			Kind               string `json:"kind"`
			Name               string `json:"name"`
			UID                string `json:"uid"`
			Controller         bool   `json:"controller"`
			BlockOwnerDeletion bool   `json:"blockOwnerDeletion"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Replicas int32 `json:"replicas"`
	} `json:"spec"`
}

type targetSnapshot struct {
	URLs []string
}

type resourceSnapshot struct {
	Namespace, Resource, Name, Fingerprint string
}

var _ = Describe("原生 PrometheusAgent 多实例与资源身份", Serial, func() {
	It(fmt.Sprintf("[%s] 隔离 namespace/name、分片、RBAC 与重叠 remote_write 数据路径", gh60CollectionScenarioID("001")), func(ctx SpecContext) {
		scenarioID := gh60CollectionScenarioID("001")
		clusterID := "gh60-" + config.testRunID

		By(scenarioID + " 确认当前工作树构建的本 Operator 正在运行，使反向边界覆盖真实控制器")
		waitForMonitorInHandOperator(ctx)

		By(scenarioID + " 创建跨 namespace 身份、每 Agent 独立 SA/CRB 和共享真实 targets")
		applyManifest(ctx, gh60RuntimeAndTargetManifest())
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", "gh60-a", testNamespace)
			deleteResource(cleanupCtx, "prometheusagent", "gh60-b", testNamespace)
			deleteResource(cleanupCtx, "namespace", gh60OtherNamespace, "")
			deleteResource(cleanupCtx, "scrapeconfig", "gh60-shared", testNamespace)
			for index := 0; index < 12; index++ {
				deleteResource(cleanupCtx, "service", fmt.Sprintf("gh60-target-%02d", index), testNamespace)
			}
			for _, name := range []string{"gh60-a-primary", "gh60-b-primary", "gh60-a-other"} {
				deleteResource(cleanupCtx, "clusterrolebinding", name, "")
			}
		})

		applyManifest(ctx, gh60AgentManifest("gh60-a", testNamespace, "gh60-a", 1, 1, clusterID, true))
		applyManifest(ctx, gh60AgentManifest("gh60-b", testNamespace, "gh60-b", 1, 2, clusterID, true))
		applyManifest(ctx, gh60AgentManifest("gh60-a", gh60OtherNamespace, "gh60-a", 1, 1, clusterID, false))

		By(scenarioID + " 等待三个 namespace/name 身份分别由 Prometheus Operator 收敛")
		waitForAgentConditionsInNamespace(ctx, testNamespace, "gh60-a")
		waitForAgentConditionsInNamespace(ctx, testNamespace, "gh60-b")
		waitForAgentConditionsInNamespace(ctx, gh60OtherNamespace, "gh60-a")

		By(scenarioID + " 验证同 namespace 不同名与跨 namespace 同名的 StatefulSet、Pod、Secrets 自动隔离")
		primaryA := verifyAgentShard(ctx, testNamespace, "gh60-a", 0)
		primaryB0 := verifyAgentShard(ctx, testNamespace, "gh60-b", 0)
		primaryB1 := verifyAgentShard(ctx, testNamespace, "gh60-b", 1)
		otherA := verifyAgentShard(ctx, gh60OtherNamespace, "gh60-a", 0)
		Expect(primaryA.Metadata.UID).NotTo(Equal(otherA.Metadata.UID))
		Expect(primaryA.Metadata.Name).To(Equal(otherA.Metadata.Name))
		Expect(primaryB0.Metadata.Name).NotTo(Equal(primaryB1.Metadata.Name))

		By(scenarioID + " 验证两个 shard 的 retained PVC 身份独立且不设置 managing owner")
		for _, pvcName := range gh60RetainedPVCNames() {
			pvc := readObject(ctx, testNamespace, "persistentvolumeclaim", pvcName)
			Expect(pvc.Metadata.OwnerReferences).To(BeEmpty())
		}

		By(scenarioID + " 验证默认 governing Service 在 namespace 内共享、在 namespace 间隔离")
		waitForGoverningServiceOwners(ctx, testNamespace, []string{"fixture", "gh60-a", "gh60-b"})
		waitForGoverningServiceOwners(ctx, gh60OtherNamespace, []string{"gh60-a"})

		By(scenarioID + " 验证每 Agent 独立 SA/CRB 绑定同一 runtime ClusterRole，且管理员资源无 owner")
		verifyRuntimeIdentity(ctx, testNamespace, "gh60-a", "gh60-a-primary")
		verifyRuntimeIdentity(ctx, testNamespace, "gh60-b", "gh60-b-primary")
		verifyRuntimeIdentity(ctx, gh60OtherNamespace, "gh60-a", "gh60-a-other")
		Expect(readObject(ctx, testNamespace, "prometheusagent", "gh60-a").Metadata.OwnerReferences).To(BeEmpty())
		Expect(readObject(ctx, testNamespace, "prometheusagent", "gh60-b").Metadata.OwnerReferences).To(BeEmpty())
		Expect(readObject(ctx, testNamespace, "scrapeconfig", "gh60-shared").Metadata.OwnerReferences).To(BeEmpty())

		By(scenarioID + " 通过两个 shard 的 targets API 验证默认分片键分配真实 targets")
		allTargets := waitForPodTargets(ctx, testNamespace, "prom-agent-gh60-a-0")
		shard0Targets := waitForPodTargets(ctx, testNamespace, "prom-agent-gh60-b-0")
		shard1Targets := waitForPodTargets(ctx, testNamespace, "prom-agent-gh60-b-shard-1-0")
		Expect(shard0Targets.URLs).NotTo(BeEmpty())
		Expect(shard1Targets.URLs).NotTo(BeEmpty())
		Expect(intersection(shard0Targets.URLs, shard1Targets.URLs)).To(BeEmpty())
		Expect(union(shard0Targets.URLs, shard1Targets.URLs)).To(Equal(allTargets.URLs))

		By(scenarioID + " 在 VictoriaMetrics 查询两个重叠选择 Agent 的独立远端 series")
		waitForRemoteAgentSeries(ctx, clusterID, []string{testNamespace + "/gh60-a", testNamespace + "/gh60-b"})

		By(scenarioID + " 将两分片 Agent 挂起，验证各 shard 缩零且其他 Agent 继续可用")
		kubectl(ctx, "--namespace", testNamespace, "patch", "prometheusagent", "gh60-b", "--type", "merge", "--patch", `{"spec":{"replicas":0}}`)
		waitForAgentStatefulSetReplicas(ctx, testNamespace, "prom-agent-gh60-b", 0)
		waitForAgentStatefulSetReplicas(ctx, testNamespace, "prom-agent-gh60-b-shard-1", 0)
		Eventually(func(g Gomega) {
			pods := kubectl(ctx, "--namespace", testNamespace, "get", "pods", "--selector", "operator.prometheus.io/name=gh60-b", "--output", "json")
			var list struct {
				Items []json.RawMessage `json:"items"`
			}
			g.Expect(json.Unmarshal([]byte(pods), &list)).To(Succeed())
			g.Expect(list.Items).To(BeEmpty())
		}, 3*time.Minute, 2*time.Second).Should(Succeed())
		waitForAgentStatefulSetReplicas(ctx, testNamespace, primaryA.Metadata.Name, 1)

		By(scenarioID + " 恢复分片 Agent 并等待两个 shard 重新收敛")
		kubectl(ctx, "--namespace", testNamespace, "patch", "prometheusagent", "gh60-b", "--type", "merge", "--patch", `{"spec":{"replicas":1}}`)
		waitForAgentConditionsInNamespace(ctx, testNamespace, "gh60-b")
		verifyAgentShard(ctx, testNamespace, "gh60-b", 0)
		verifyAgentShard(ctx, testNamespace, "gh60-b", 1)

		By(scenarioID + " 快照 Agent、管理员配置/RBAC 与外部生成资源的身份和可变内容")
		snapshots := captureGH60BoundarySnapshots(ctx)

		By(scenarioID + " 删除另一 Agent 并等待其 workload 清理，验证剩余实例不被联动清理")
		deleteResource(ctx, "prometheusagent", "gh60-a", testNamespace)
		Eventually(func(g Gomega) {
			_, err := kubectlResult(ctx, "--namespace", testNamespace, "get", "statefulset", primaryA.Metadata.Name, "--output", "json")
			g.Expect(err).To(HaveOccurred())
			currentB0 := readObject(ctx, testNamespace, "statefulset", primaryB0.Metadata.Name)
			currentB1 := readObject(ctx, testNamespace, "statefulset", primaryB1.Metadata.Name)
			g.Expect(currentB0.Metadata.UID).To(Equal(primaryB0.Metadata.UID))
			g.Expect(currentB1.Metadata.UID).To(Equal(primaryB1.Metadata.UID))
		}, 3*time.Minute, 2*time.Second).Should(Succeed())

		By(scenarioID + " 明确验证本 Operator 未修改、拥有或清理剩余 Agent 边界内的任何资源")
		Consistently(func(g Gomega) {
			for _, snapshot := range snapshots {
				fingerprint, err := resourceFingerprint(ctx, snapshot.Namespace, snapshot.Resource, snapshot.Name)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(fingerprint).To(Equal(snapshot.Fingerprint), snapshot.Resource+"/"+snapshot.Name)
			}
		}, 10*time.Second, 2*time.Second).Should(Succeed())
		verifyRuntimeIdentity(ctx, testNamespace, "gh60-a", "gh60-a-primary")
		waitForRemoteAgentSeries(ctx, clusterID, []string{testNamespace + "/gh60-b"})
	}, SpecTimeout(15*time.Minute))

	It(fmt.Sprintf("[%s] 上游运行双副本但一期预检明确拒绝", gh60CollectionScenarioID("002")), func(ctx SpecContext) {
		scenarioID := gh60CollectionScenarioID("002")
		By(scenarioID + " 创建独立 runtime 身份与 replicas=2 的原生 Agent")
		applyManifest(ctx, fmt.Sprintf(`
apiVersion: v1
kind: ServiceAccount
metadata: {name: gh60-replicated, namespace: %s}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: gh60-replicated-primary}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: monitor-in-hand-test-prometheus-agent}
subjects: [{kind: ServiceAccount, name: gh60-replicated, namespace: %s}]
`, testNamespace, testNamespace))
		manifest := gh60AgentManifest("gh60-replicated", testNamespace, "gh60-replicated", 2, 1, "gh60-replicated-"+config.testRunID, false)
		applyManifest(ctx, manifest)
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", "gh60-replicated", testNamespace)
			deleteResource(cleanupCtx, "clusterrolebinding", "gh60-replicated-primary", "")
			deleteResource(cleanupCtx, "serviceaccount", "gh60-replicated", testNamespace)
		})

		By(scenarioID + " 等待 Prometheus Operator v0.83.0 把同一 shard 收敛为两个 Ready Pod")
		waitForAgentConditionsInNamespace(ctx, testNamespace, "gh60-replicated")
		waitForAgentStatefulSetReplicas(ctx, testNamespace, "prom-agent-gh60-replicated", 2)
		waitForReadyAgentPods(ctx, testNamespace, "gh60-replicated", []string{"prom-agent-gh60-replicated-0", "prom-agent-gh60-replicated-1"})

		By(scenarioID + " 对同一份有效上游 manifest 执行一期严格预检")
		repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
		Expect(err).NotTo(HaveOccurred())
		manifestPath := filepath.Join(GinkgoT().TempDir(), "agent.yaml")
		Expect(os.WriteFile(manifestPath, []byte(manifest), 0o600)).To(Succeed())
		command := exec.CommandContext(ctx, filepath.Join(repoRoot, "hack", "preflight.sh"),
			"--context", config.kubeContext,
			"--agent-namespace", testNamespace,
			"--agent-service-account", "gh60-replicated",
			"--config-namespaces", testNamespace,
			"--discovery-namespaces", testNamespace,
			"--agent-manifest", manifestPath,
		)
		output, preflightErr := command.CombinedOutput()

		By(scenarioID + " 验证上游双副本保持可用，但一期以稳定原因非零拒绝")
		Expect(preflightErr).To(HaveOccurred(), string(output))
		Expect(preflightErr.(*exec.ExitError).ExitCode()).To(Equal(1))
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"check":"agent-topology"`),
			ContainSubstring(`"reason":"UnsupportedAgentReplicas"`),
		))
	}, SpecTimeout(8*time.Minute))
})

func gh60CollectionScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {"001": "E2E-119-COLLECTION-GH60-001", "002": "E2E-119-COLLECTION-GH60-002"},
		"1.28": {"001": "E2E-128-COLLECTION-GH60-001", "002": "E2E-128-COLLECTION-GH60-002"},
	}
	minor := config.kubernetesMinor
	if minor == "" {
		minor = os.Getenv("KUBERNETES_VERSION")
	}
	if versionIDs, supported := ids[minor]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-COLLECTION-GH60-" + sequence
}

func gh60RuntimeAndTargetManifest() string {
	var documents []string
	documents = append(documents, fmt.Sprintf(`
apiVersion: v1
kind: Namespace
metadata: {name: %s}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: gh60-a, namespace: %s}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: gh60-b, namespace: %s}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: gh60-a, namespace: %s}
`, gh60OtherNamespace, testNamespace, testNamespace, gh60OtherNamespace))
	for _, binding := range []struct{ name, serviceAccount, namespace string }{
		{name: "gh60-a-primary", serviceAccount: "gh60-a", namespace: testNamespace},
		{name: "gh60-b-primary", serviceAccount: "gh60-b", namespace: testNamespace},
		{name: "gh60-a-other", serviceAccount: "gh60-a", namespace: gh60OtherNamespace},
	} {
		documents = append(documents, fmt.Sprintf(`
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: %s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: monitor-in-hand-test-prometheus-agent}
subjects: [{kind: ServiceAccount, name: %s, namespace: %s}]
`, binding.name, binding.serviceAccount, binding.namespace))
	}
	var targets []string
	for index := 0; index < 12; index++ {
		name := fmt.Sprintf("gh60-target-%02d", index)
		targets = append(targets, name+"."+testNamespace+".svc:9100")
		documents = append(documents, fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata: {name: %s, namespace: %s}
spec:
  selector: {app.kubernetes.io/name: metrics-target}
  ports: [{name: metrics, port: 9100, targetPort: metrics}]
`, name, testNamespace))
	}
	documents = append(documents, fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: ScrapeConfig
metadata:
  name: gh60-shared
  namespace: %s
  labels: {monitoring.fjsxy.github.io/gh60-shared: "true"}
spec:
  staticConfigs:
    - targets: [%s]
`, testNamespace, strings.Join(targets, ", ")))
	return strings.Join(documents, "\n---\n")
}

func gh60AgentManifest(name, namespace, serviceAccount string, replicas, shards int, clusterID string, selectShared bool) string {
	selector := ""
	if selectShared {
		selector = "  scrapeConfigSelector: {matchLabels: {monitoring.fjsxy.github.io/gh60-shared: \"true\"}}\n"
	}
	storage := ""
	if shards == 2 {
		storage = "  storage:\n    volumeClaimTemplate:\n      spec:\n        accessModes: [ReadWriteOnce]\n        resources: {requests: {storage: 64Mi}}\n"
	}
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: %s, namespace: %s}
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus@sha256:2659f4c2ebb718e7695cb9b25ffa7d6be64db013daba13e05c875451cf51b0d3
  serviceAccountName: %s
  replicas: %d
  shards: %d
  replicaExternalLabelName: ""
  externalLabels: {mih_cluster_id: %s}
%s%s  remoteWrite:
    - name: gh60-vm
      url: http://vminsert.monitor-in-hand-test.svc:8480/insert/0/prometheus/api/v1/write
`, name, namespace, serviceAccount, replicas, shards, clusterID, selector, storage)
}

func waitForAgentConditionsInNamespace(ctx context.Context, namespace, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		output, err := kubectlResult(ctx, "--namespace", namespace, "get", "prometheusagent", name, "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), output)
		var object struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type               string `json:"type"`
					Status             string `json:"status"`
					ObservedGeneration int64  `json:"observedGeneration"`
				} `json:"conditions"`
			} `json:"status"`
		}
		g.Expect(json.Unmarshal([]byte(output), &object)).To(Succeed())
		for _, conditionType := range []string{"Reconciled", "Available"} {
			g.Expect(object.Status.Conditions).To(ContainElement(SatisfyAll(
				HaveField("Type", conditionType), HaveField("Status", "True"), HaveField("ObservedGeneration", object.Metadata.Generation),
			)))
		}
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
}

func readObject(ctx context.Context, namespace, resource, name string) objectIdentity {
	GinkgoHelper()
	args := []string{}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", resource, name, "--output", "json")
	output := kubectl(ctx, args...)
	var object objectIdentity
	Expect(json.Unmarshal([]byte(output), &object)).To(Succeed())
	return object
}

func waitForMonitorInHandOperator(ctx context.Context) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		output, err := kubectlResult(ctx, "--namespace", "monitor-in-hand-system", "get", "deployment", "monitor-in-hand-monitor-in-hand", "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), output)
		var deployment struct {
			Metadata struct {
				Generation int64 `json:"generation"`
			} `json:"metadata"`
			Status struct {
				ObservedGeneration int64 `json:"observedGeneration"`
				AvailableReplicas  int32 `json:"availableReplicas"`
			} `json:"status"`
		}
		g.Expect(json.Unmarshal([]byte(output), &deployment)).To(Succeed())
		g.Expect(deployment.Status.ObservedGeneration).To(Equal(deployment.Metadata.Generation))
		g.Expect(deployment.Status.AvailableReplicas).To(Equal(int32(1)))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

func gh60RetainedPVCNames() []string {
	return []string{
		"prom-agent-gh60-b-db-prom-agent-gh60-b-0",
		"prom-agent-gh60-b-db-prom-agent-gh60-b-shard-1-0",
	}
}

func captureGH60BoundarySnapshots(ctx context.Context) []resourceSnapshot {
	GinkgoHelper()
	resources := []resourceSnapshot{
		{Namespace: testNamespace, Resource: "prometheusagent", Name: "gh60-b"},
		{Namespace: testNamespace, Resource: "serviceaccount", Name: "gh60-b"},
		{Resource: "clusterrolebinding", Name: "gh60-b-primary"},
		{Resource: "clusterrole", Name: "monitor-in-hand-test-prometheus-agent"},
		{Namespace: testNamespace, Resource: "scrapeconfig", Name: "gh60-shared"},
		{Namespace: testNamespace, Resource: "statefulset", Name: "prom-agent-gh60-b"},
		{Namespace: testNamespace, Resource: "statefulset", Name: "prom-agent-gh60-b-shard-1"},
		{Namespace: testNamespace, Resource: "pod", Name: "prom-agent-gh60-b-0"},
		{Namespace: testNamespace, Resource: "pod", Name: "prom-agent-gh60-b-shard-1-0"},
		{Namespace: testNamespace, Resource: "secret", Name: "prom-agent-gh60-b"},
		{Namespace: testNamespace, Resource: "secret", Name: "prom-agent-gh60-b-tls-assets-0"},
		{Namespace: testNamespace, Resource: "secret", Name: "prom-agent-gh60-b-web-config"},
	}
	for _, name := range gh60RetainedPVCNames() {
		resources = append(resources, resourceSnapshot{Namespace: testNamespace, Resource: "persistentvolumeclaim", Name: name})
	}
	for index := range resources {
		fingerprint, err := resourceFingerprint(ctx, resources[index].Namespace, resources[index].Resource, resources[index].Name)
		Expect(err).NotTo(HaveOccurred())
		resources[index].Fingerprint = fingerprint
	}
	return resources
}

func resourceFingerprint(ctx context.Context, namespace, resource, name string) (string, error) {
	args := []string{}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	args = append(args, "get", resource, name, "--output", "json")
	output, err := kubectlResult(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("读取 %s/%s: %w: %s", resource, name, err, output)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(output), &object); err != nil {
		return "", fmt.Errorf("解析 %s/%s: %w", resource, name, err)
	}
	metadata, _ := object["metadata"].(map[string]any)
	fingerprint := map[string]any{
		"uid":             metadata["uid"],
		"labels":          metadata["labels"],
		"ownerReferences": metadata["ownerReferences"],
	}
	for _, field := range []string{"spec", "data", "roleRef", "subjects", "rules"} {
		if value, found := object[field]; found {
			fingerprint[field] = value
		}
	}
	encoded, err := json.Marshal(fingerprint)
	if err != nil {
		return "", fmt.Errorf("编码 %s/%s 快照: %w", resource, name, err)
	}
	return string(encoded), nil
}

func verifyAgentShard(ctx context.Context, namespace, agentName string, shard int) objectIdentity {
	GinkgoHelper()
	agent := readObject(ctx, namespace, "prometheusagent", agentName)
	statefulSetName := "prom-agent-" + agentName
	if shard > 0 {
		statefulSetName += fmt.Sprintf("-shard-%d", shard)
	}
	var statefulSet objectIdentity
	By(fmt.Sprintf("等待 %s/%s 的 StatefulSet 与 Pod 收敛并验证 managing owner 链", namespace, statefulSetName))
	Eventually(func(g Gomega) {
		statefulSet = readObject(ctx, namespace, "statefulset", statefulSetName)
		g.Expect(statefulSet.Metadata.OwnerReferences).To(ContainElement(SatisfyAll(
			HaveField("APIVersion", "monitoring.coreos.com/v1alpha1"), HaveField("Kind", "PrometheusAgent"),
			HaveField("Name", agentName), HaveField("UID", agent.Metadata.UID), HaveField("Controller", true),
		)))
		g.Expect(statefulSet.Spec.Replicas).To(Equal(int32(1)))
		pod := readObject(ctx, namespace, "pod", statefulSetName+"-0")
		g.Expect(pod.Metadata.OwnerReferences).To(ContainElement(SatisfyAll(
			HaveField("APIVersion", "apps/v1"), HaveField("Kind", "StatefulSet"),
			HaveField("Name", statefulSetName), HaveField("UID", statefulSet.Metadata.UID), HaveField("Controller", true),
		)))
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	By(fmt.Sprintf("验证 %s/%s 的实例级配置、TLS 与 web Secret 只由原生 Agent 管理", namespace, agentName))
	for _, secretName := range []string{"prom-agent-" + agentName, "prom-agent-" + agentName + "-tls-assets-0", "prom-agent-" + agentName + "-web-config"} {
		secret := readObject(ctx, namespace, "secret", secretName)
		Expect(secret.Metadata.OwnerReferences).To(ContainElement(SatisfyAll(
			HaveField("APIVersion", "monitoring.coreos.com/v1alpha1"), HaveField("Kind", "PrometheusAgent"),
			HaveField("Name", agentName), HaveField("UID", agent.Metadata.UID), HaveField("Controller", true),
		)))
		for _, owner := range secret.Metadata.OwnerReferences {
			Expect(owner.APIVersion).NotTo(HavePrefix("monitoring.fjsxy.github.io/"))
		}
	}
	return statefulSet
}

func waitForGoverningServiceOwners(ctx context.Context, namespace string, expectedOwners []string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		output, err := kubectlResult(ctx, "--namespace", namespace, "get", "service", "prometheus-agent-operated", "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), output)
		var service struct {
			Metadata struct {
				Labels          map[string]string `json:"labels"`
				OwnerReferences []struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
					Name       string `json:"name"`
					Controller bool   `json:"controller"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				ClusterIP string            `json:"clusterIP"`
				Selector  map[string]string `json:"selector"`
			} `json:"spec"`
		}
		g.Expect(json.Unmarshal([]byte(output), &service)).To(Succeed())
		g.Expect(service.Spec.ClusterIP).To(Equal("None"))
		g.Expect(service.Spec.Selector).To(Equal(map[string]string{"app.kubernetes.io/name": "prometheus-agent"}))
		g.Expect(service.Metadata.Labels).To(HaveKeyWithValue("operated-prometheus", "true"))
		owners := make([]string, 0, len(service.Metadata.OwnerReferences))
		for _, owner := range service.Metadata.OwnerReferences {
			g.Expect(owner.APIVersion).To(Equal("monitoring.coreos.com/v1alpha1"))
			g.Expect(owner.Kind).To(Equal("PrometheusAgent"))
			g.Expect(owner.Controller).To(BeFalse())
			owners = append(owners, owner.Name)
		}
		for _, expected := range expectedOwners {
			g.Expect(owners).To(ContainElement(expected))
		}
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

func verifyRuntimeIdentity(ctx context.Context, namespace, serviceAccount, bindingName string) {
	GinkgoHelper()
	serviceAccountObject := readObject(ctx, namespace, "serviceaccount", serviceAccount)
	Expect(serviceAccountObject.Metadata.OwnerReferences).To(BeEmpty())
	output := kubectl(ctx, "get", "clusterrolebinding", bindingName, "--output", "json")
	var binding struct {
		Metadata struct {
			OwnerReferences []json.RawMessage `json:"ownerReferences"`
		} `json:"metadata"`
		RoleRef struct {
			Name string `json:"name"`
		} `json:"roleRef"`
		Subjects []struct {
			Kind, Name, Namespace string
		} `json:"subjects"`
	}
	Expect(json.Unmarshal([]byte(output), &binding)).To(Succeed())
	Expect(binding.Metadata.OwnerReferences).To(BeEmpty())
	Expect(binding.RoleRef.Name).To(Equal("monitor-in-hand-test-prometheus-agent"))
	Expect(binding.Subjects).To(ConsistOf(SatisfyAll(HaveField("Kind", "ServiceAccount"), HaveField("Name", serviceAccount), HaveField("Namespace", namespace))))
	sharedRole := readObject(ctx, testNamespace, "clusterrole", "monitor-in-hand-test-prometheus-agent")
	Expect(sharedRole.Metadata.OwnerReferences).To(BeEmpty())
}

func waitForPodTargets(ctx context.Context, namespace, pod string) targetSnapshot {
	GinkgoHelper()
	port, stop := startPortForwardTarget(ctx, namespace, "pod/"+pod, 9090)
	DeferCleanup(stop)
	client := &http.Client{Timeout: 5 * time.Second}
	var urls []string
	Eventually(func(g Gomega) {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/v1/targets", port))
		g.Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		contents, err := io.ReadAll(response.Body)
		g.Expect(err).NotTo(HaveOccurred())
		var targets struct {
			Status string `json:"status"`
			Data   struct {
				ActiveTargets []struct {
					Health    string `json:"health"`
					ScrapeURL string `json:"scrapeUrl"`
				} `json:"activeTargets"`
			} `json:"data"`
		}
		g.Expect(json.Unmarshal(contents, &targets)).To(Succeed())
		g.Expect(targets.Status).To(Equal("success"))
		g.Expect(targets.Data.ActiveTargets).NotTo(BeEmpty())
		urls = urls[:0]
		for _, target := range targets.Data.ActiveTargets {
			g.Expect(target.Health).To(Equal("up"))
			urls = append(urls, target.ScrapeURL)
		}
		sort.Strings(urls)
	}, 5*time.Minute, 3*time.Second).Should(Succeed())
	return targetSnapshot{URLs: append([]string(nil), urls...)}
}

func waitForRemoteAgentSeries(ctx context.Context, clusterID string, expectedAgents []string) {
	GinkgoHelper()
	port, stop := startPortForwardTarget(ctx, testNamespace, "service/vmselect", 8481)
	DeferCleanup(stop)
	client := &http.Client{Timeout: 5 * time.Second}
	query := fmt.Sprintf(`count by (prometheus) (node_uname_info{mih_cluster_id=%q})`, clusterID)
	Eventually(func(g Gomega) {
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/select/0/prometheus/api/v1/query?query=%s", port, url.QueryEscape(query)))
		g.Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		contents, err := io.ReadAll(response.Body)
		g.Expect(err).NotTo(HaveOccurred())
		var result struct {
			Status string `json:"status"`
			Data   struct {
				Result []struct {
					Metric map[string]string `json:"metric"`
				} `json:"result"`
			} `json:"data"`
		}
		g.Expect(json.Unmarshal(contents, &result)).To(Succeed())
		g.Expect(result.Status).To(Equal("success"), string(contents))
		actual := make([]string, 0, len(result.Data.Result))
		for _, series := range result.Data.Result {
			actual = append(actual, series.Metric["prometheus"])
		}
		for _, expected := range expectedAgents {
			g.Expect(actual).To(ContainElement(expected))
		}
	}, 5*time.Minute, 3*time.Second).Should(Succeed())
}

func waitForAgentStatefulSetReplicas(ctx context.Context, namespace, name string, expected int32) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		object := readObject(ctx, namespace, "statefulset", name)
		g.Expect(object.Spec.Replicas).To(Equal(expected))
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

func waitForReadyAgentPods(ctx context.Context, namespace, agentName string, expectedNames []string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		output, err := kubectlResult(ctx, "--namespace", namespace, "get", "pods", "--selector", "operator.prometheus.io/name="+agentName, "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), output)
		var list struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Status struct {
					Conditions []struct {
						Type, Status string
					} `json:"conditions"`
				} `json:"status"`
			} `json:"items"`
		}
		g.Expect(json.Unmarshal([]byte(output), &list)).To(Succeed())
		names := make([]string, 0, len(list.Items))
		for _, pod := range list.Items {
			names = append(names, pod.Metadata.Name)
			g.Expect(pod.Status.Conditions).To(ContainElement(SatisfyAll(HaveField("Type", "Ready"), HaveField("Status", "True"))))
		}
		g.Expect(names).To(ConsistOf(expectedNames))
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
}

func intersection(left, right []string) []string {
	rightSet := map[string]struct{}{}
	for _, item := range right {
		rightSet[item] = struct{}{}
	}
	result := []string{}
	for _, item := range left {
		if _, found := rightSet[item]; found {
			result = append(result, item)
		}
	}
	return result
}

func union(left, right []string) []string {
	items := map[string]struct{}{}
	for _, item := range append(append([]string{}, left...), right...) {
		items[item] = struct{}{}
	}
	result := make([]string, 0, len(items))
	for item := range items {
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}
