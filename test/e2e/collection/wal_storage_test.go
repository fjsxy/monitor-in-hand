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
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	gh62AccountID       = "62"
	gh62RemoteWriteName = "gh62-victoriametrics-primary"
)

type gh62StorageMode string

const (
	gh62EmptyDirStorage gh62StorageMode = "emptyDir"
	gh62PVCStorage      gh62StorageMode = "pvc"
)

type gh62PVC struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		AccessModes      []string `json:"accessModes"`
		VolumeMode       string   `json:"volumeMode"`
		StorageClassName string   `json:"storageClassName"`
		Resources        struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type gh62Volume struct {
	Name     string           `json:"name"`
	EmptyDir *json.RawMessage `json:"emptyDir"`
	PVC      *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim"`
}

type gh62Pod struct {
	Metadata struct {
		UID string `json:"uid"`
	} `json:"metadata"`
	Spec struct {
		Containers []struct {
			Name         string `json:"name"`
			VolumeMounts []struct {
				Name      string `json:"name"`
				MountPath string `json:"mountPath"`
			} `json:"volumeMounts"`
		} `json:"containers"`
		Volumes []gh62Volume `json:"volumes"`
	} `json:"spec"`
}

type gh62RangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

var _ = Describe("PrometheusAgent WAL 存储与恢复", Serial, func() {
	It(fmt.Sprintf("[%s] 省略 storage 时 Pod 替换使用新 EmptyDir", gh62CollectionScenarioID("001")), func(ctx SpecContext) {
		scenarioID := gh62CollectionScenarioID("001")
		agentName := "gh62-emptydir"

		By(scenarioID + " 创建省略 storage 的 Agent 并等待 Pod 可用")
		applyManifest(ctx, gh62AgentManifest(agentName, 1, 1, gh62EmptyDirStorage, "vminsert", ""))
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", agentName, testNamespace)
		})
		waitForAgentConditionsInNamespace(ctx, testNamespace, agentName)
		podName := "prom-agent-" + agentName + "-0"
		waitForReadyAgentPods(ctx, testNamespace, agentName, []string{podName})

		By(scenarioID + " 验证 WAL 挂载源是 EmptyDir 且没有为 Agent 生成 PVC")
		originalPod := readGH62Pod(ctx, podName)
		assertGH62EmptyDirDataVolume(originalPod)
		claims := kubectl(ctx, "--namespace", testNamespace, "get", "persistentvolumeclaims", "--selector", "operator.prometheus.io/name="+agentName, "--output", "json")
		var claimList struct {
			Items []json.RawMessage `json:"items"`
		}
		Expect(json.Unmarshal([]byte(claims), &claimList)).To(Succeed())
		Expect(claimList.Items).To(BeEmpty())

		By(scenarioID + " 删除 Agent Pod，验证同名新 Pod 拥有新 UID 与新 EmptyDir")
		deleteResource(ctx, "pod", podName, testNamespace)
		replacement := waitForGH62ReplacementPod(ctx, agentName, podName, originalPod.Metadata.UID)
		assertGH62EmptyDirDataVolume(replacement)

		By(scenarioID + " 只验证可观察的卷替换边界，不对待发送 WAL 做时序敏感的必丢断言")
	}, SpecTimeout(8*time.Minute))

	It(fmt.Sprintf("[%s] PVC 在缩副本、挂起、缩分片、删除与同名重建中保留", gh62CollectionScenarioID("002")), func(ctx SpecContext) {
		scenarioID := gh62CollectionScenarioID("002")
		agentName := "gh62-pvc"
		manifest := gh62AgentManifest(agentName, 2, 2, gh62PVCStorage, "vminsert", "")
		recoveryManifest := gh62AgentManifest(agentName, 1, 2, gh62PVCStorage, "vminsert", "")
		pvcNames := gh62PVCNames(agentName, 2, 2)
		allPodNames := []string{
			"prom-agent-" + agentName + "-0",
			"prom-agent-" + agentName + "-1",
			"prom-agent-" + agentName + "-shard-1-0",
			"prom-agent-" + agentName + "-shard-1-1",
		}
		oneReplicaPodNames := []string{
			"prom-agent-" + agentName + "-0",
			"prom-agent-" + agentName + "-shard-1-0",
		}

		By(scenarioID + " 创建两副本、两分片持久 Agent，验证每个 shard/ordinal 独立 PVC 的存储属性")
		applyManifest(ctx, manifest)
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", agentName, testNamespace)
			for _, pvcName := range pvcNames {
				deleteResource(cleanupCtx, "persistentvolumeclaim", pvcName, testNamespace)
			}
			deleteResource(cleanupCtx, "persistentvolumeclaim", gh62ConflictPVCName, testNamespace)
		})
		waitForAgentConditionsInNamespace(ctx, testNamespace, agentName)
		waitForReadyAgentPods(ctx, testNamespace, agentName, allPodNames)
		originalPVCs := waitForGH62PVCs(ctx, agentName, pvcNames)
		uniquePVCUIDs := map[string]struct{}{}
		for _, claim := range originalPVCs {
			uniquePVCUIDs[claim.Metadata.UID] = struct{}{}
		}
		Expect(uniquePVCUIDs).To(HaveLen(4))
		assertGH62PVCProperties(originalPVCs[0], agentName, "0", "64Mi")
		assertGH62PVCProperties(originalPVCs[1], agentName, "0", "64Mi")
		assertGH62PVCProperties(originalPVCs[2], agentName, "1", "64Mi")
		assertGH62PVCProperties(originalPVCs[3], agentName, "1", "64Mi")
		assertGH62StatefulSetStorage(ctx, agentName)

		By(scenarioID + " 将 replicas 从 2 缩为 1，验证被缩减 ordinal 的 PVC UID 仍保留")
		kubectl(ctx, "--namespace", testNamespace, "patch", "prometheusagent", agentName, "--type", "merge", "--patch", `{"spec":{"replicas":1}}`)
		waitForAgentStatefulSetReplicas(ctx, testNamespace, "prom-agent-"+agentName, 1)
		waitForAgentStatefulSetReplicas(ctx, testNamespace, "prom-agent-"+agentName+"-shard-1", 1)
		waitForReadyAgentPods(ctx, testNamespace, agentName, oneReplicaPodNames)
		assertGH62PVCUIDs(ctx, agentName, originalPVCs)

		By(scenarioID + " 再将 replicas 缩为 0 挂起全部 shard，验证 PVC UID 全部保留")
		kubectl(ctx, "--namespace", testNamespace, "patch", "prometheusagent", agentName, "--type", "merge", "--patch", `{"spec":{"replicas":0}}`)
		waitForAgentStatefulSetReplicas(ctx, testNamespace, "prom-agent-"+agentName, 0)
		waitForAgentStatefulSetReplicas(ctx, testNamespace, "prom-agent-"+agentName+"-shard-1", 0)
		waitForReadyAgentPods(ctx, testNamespace, agentName, nil)
		assertGH62PVCUIDs(ctx, agentName, originalPVCs)

		By(scenarioID + " 恢复后缩减 shards，验证被删除 StatefulSet 的 PVC 仍保留")
		kubectl(ctx, "--namespace", testNamespace, "patch", "prometheusagent", agentName, "--type", "merge", "--patch", `{"spec":{"replicas":1}}`)
		waitForReadyAgentPods(ctx, testNamespace, agentName, oneReplicaPodNames)
		kubectl(ctx, "--namespace", testNamespace, "patch", "prometheusagent", agentName, "--type", "merge", "--patch", `{"spec":{"shards":1}}`)
		waitForGH62ResourceAbsent(ctx, "statefulset", "prom-agent-"+agentName+"-shard-1")
		assertGH62PVCUIDs(ctx, agentName, originalPVCs)

		By(scenarioID + " 重新扩为两分片，验证 shard 1 重新挂载原 PVC")
		kubectl(ctx, "--namespace", testNamespace, "patch", "prometheusagent", agentName, "--type", "merge", "--patch", `{"spec":{"shards":2}}`)
		waitForReadyAgentPods(ctx, testNamespace, agentName, oneReplicaPodNames)
		assertGH62PVCUIDs(ctx, agentName, originalPVCs)
		assertGH62PVCDataVolume(readGH62Pod(ctx, "prom-agent-"+agentName+"-shard-1-0"), pvcNames[2])

		By(scenarioID + " 直接删除 shard 1 StatefulSet，验证调谐重建不更换 PVC")
		statefulSetUID := readObject(ctx, testNamespace, "statefulset", "prom-agent-"+agentName+"-shard-1").Metadata.UID
		deleteResource(ctx, "statefulset", "prom-agent-"+agentName+"-shard-1", testNamespace)
		waitForGH62StatefulSetReplacement(ctx, "prom-agent-"+agentName+"-shard-1", statefulSetUID)
		waitForReadyAgentPods(ctx, testNamespace, agentName, oneReplicaPodNames)
		assertGH62PVCUIDs(ctx, agentName, originalPVCs)

		By(scenarioID + " 删除 Agent，验证 workload 消失但 PVC 不依赖 retention policy 而保留")
		deleteResource(ctx, "prometheusagent", agentName, testNamespace)
		waitForGH62ResourceAbsent(ctx, "statefulset", "prom-agent-"+agentName)
		waitForGH62ResourceAbsent(ctx, "statefulset", "prom-agent-"+agentName+"-shard-1")
		for _, podName := range allPodNames {
			waitForGH62ResourceAbsent(ctx, "pod", podName)
		}
		assertGH62PVCUIDs(ctx, agentName, originalPVCs)

		By(scenarioID + " 对同 namespace/name 恢复计划执行严格预检，再重建 Agent")
		preflightOutput, preflightErr := runGH62Preflight(ctx, recoveryManifest)
		AddReportEntry("same-name-recovery-preflight", preflightOutput, ReportEntryVisibilityFailureOrVerbose)
		Expect(preflightErr).NotTo(HaveOccurred(), preflightOutput)
		Expect(preflightOutput).To(SatisfyAll(ContainSubstring(`"reason":"RetainedPVCCompatible"`), ContainSubstring(`"reason":"PreflightPassed"`)))
		applyManifest(ctx, recoveryManifest)
		waitForAgentConditionsInNamespace(ctx, testNamespace, agentName)
		waitForReadyAgentPods(ctx, testNamespace, agentName, oneReplicaPodNames)
		assertGH62PVCUIDs(ctx, agentName, originalPVCs)
		assertGH62PVCDataVolume(readGH62Pod(ctx, "prom-agent-"+agentName+"-0"), pvcNames[0])
		assertGH62PVCDataVolume(readGH62Pod(ctx, "prom-agent-"+agentName+"-shard-1-0"), pvcNames[2])

		By(scenarioID + " 将同名恢复实例扩回两副本，验证每个 shard/ordinal 都重新挂载原 PVC")
		kubectl(ctx, "--namespace", testNamespace, "patch", "prometheusagent", agentName, "--type", "merge", "--patch", `{"spec":{"replicas":2}}`)
		waitForReadyAgentPods(ctx, testNamespace, agentName, allPodNames)
		assertGH62PVCUIDs(ctx, agentName, originalPVCs)
		for index, podName := range allPodNames {
			assertGH62PVCDataVolume(readGH62Pod(ctx, podName), pvcNames[index])
		}

		By(scenarioID + " 创建容量与计划不匹配的同名 PVC，验证来源/属性冲突非零退出")
		applyManifest(ctx, gh62ConflictPVCManifest)
		conflictOutput, conflictErr := runGH62Preflight(ctx, gh62AgentManifest("gh62-conflict", 1, 1, gh62PVCStorage, "vminsert", ""))
		AddReportEntry("pvc-conflict-preflight", conflictOutput, ReportEntryVisibilityFailureOrVerbose)
		Expect(conflictErr).To(HaveOccurred(), conflictOutput)
		Expect(conflictErr.(*exec.ExitError).ExitCode()).To(Equal(1))
		Expect(conflictOutput).To(SatisfyAll(ContainSubstring(`"reason":"RetainedPVCConflict"`), ContainSubstring(gh62ConflictPVCName)))
	}, SpecTimeout(15*time.Minute))

	It(fmt.Sprintf("[%s] Pod 故障后从原 PVC WAL 恢复故障窗口样本", gh62CollectionScenarioID("003")), func(ctx SpecContext) {
		scenarioID := gh62CollectionScenarioID("003")
		agentName := "gh62-wal"
		podName := "prom-agent-" + agentName + "-0"
		pvcName := gh62PVCNames(agentName, 1, 1)[0]
		clusterID := "gh62-" + config.testRunID
		sampleID := "gh62-wal-" + config.testRunID

		By(scenarioID + " 创建无 endpoints 的 remote_write Service、唯一样本配置与持久 Agent")
		applyManifest(ctx, gh62WALFixtures(sampleID))
		applyManifest(ctx, gh62WALAgentManifest(clusterID))
		DeferCleanup(func(cleanupCtx SpecContext) {
			deleteResource(cleanupCtx, "prometheusagent", agentName, testNamespace)
			deleteResource(cleanupCtx, "scrapeconfig", "gh62-wal", testNamespace)
			deleteResource(cleanupCtx, "service", "gh62-remote-write", testNamespace)
			deleteResource(cleanupCtx, "persistentvolumeclaim", pvcName, testNamespace)
		})
		waitForAgentConditionsInNamespace(ctx, testNamespace, agentName)
		waitForReadyAgentPods(ctx, testNamespace, agentName, []string{podName})
		originalPVC := waitForGH62PVCs(ctx, agentName, []string{pvcName})[0]
		originalPod := readGH62Pod(ctx, podName)
		assertGH62PVCDataVolume(originalPod, pvcName)
		outageStart := time.Now().Add(-5 * time.Second)

		By(scenarioID + " 在目标不可达时产生唯一标签样本，等待 pending 与 WAL lag 同时增长")
		backlogMetrics := waitForGH62BacklogGrowth(ctx, podName)
		AddReportEntry("remote-write-backlog", backlogMetrics, ReportEntryVisibilityFailureOrVerbose)
		walWindowEnd := time.Now()

		By(scenarioID + " 删除 Agent Pod，验证新 Pod 挂载原 PVC UID")
		deleteResource(ctx, "pod", podName, testNamespace)
		replacement := waitForGH62ReplacementPod(ctx, agentName, podName, originalPod.Metadata.UID)
		assertGH62PVCDataVolume(replacement, pvcName)
		currentPVC := waitForGH62PVCs(ctx, agentName, []string{pvcName})[0]
		Expect(currentPVC.Metadata.UID).To(Equal(originalPVC.Metadata.UID))

		By(scenarioID + " 恢复 remote_write Service endpoints，仅查询 Pod 故障前已进入 WAL 的样本")
		kubectl(ctx, "--namespace", testNamespace, "patch", "service", "gh62-remote-write", "--type", "json", "--patch", `[{"op":"replace","path":"/spec/selector","value":{"app.kubernetes.io/name":"vminsert"}}]`)
		queryBody := waitForGH62OutageSamples(ctx, sampleID, clusterID, outageStart, walWindowEnd)
		AddReportEntry("vmselect-outage-window-query", queryBody, ReportEntryVisibilityFailureOrVerbose)
	}, SpecTimeout(12*time.Minute))
})

func gh62CollectionScenarioID(sequence string) string {
	ids := map[string]map[string]string{
		"1.19": {
			"001": "E2E-119-COLLECTION-GH62-001",
			"002": "E2E-119-COLLECTION-GH62-002",
			"003": "E2E-119-COLLECTION-GH62-003",
		},
		"1.28": {
			"001": "E2E-128-COLLECTION-GH62-001",
			"002": "E2E-128-COLLECTION-GH62-002",
			"003": "E2E-128-COLLECTION-GH62-003",
		},
	}
	if versionIDs, supported := ids[config.kubernetesMinor]; supported {
		if id, found := versionIDs[sequence]; found {
			return id
		}
	}
	return "E2E-UNSUPPORTED-COLLECTION-GH62-" + sequence
}

func gh62AgentManifest(name string, replicas, shards int, storageMode gh62StorageMode, remoteService, scrapeSelector string) string {
	storage := ""
	if storageMode == gh62PVCStorage {
		storage = `  storage:
    volumeClaimTemplate:
      spec:
        storageClassName: standard
        accessModes: [ReadWriteOnce]
        volumeMode: Filesystem
        resources: {requests: {storage: 64Mi}}
`
	}
	selector := ""
	if scrapeSelector != "" {
		selector = fmt.Sprintf("  scrapeConfigSelector: {matchLabels: {monitoring.fjsxy.github.io/gh62: %q}}\n", scrapeSelector)
	}
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: %s, namespace: %s}
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus:v2.55.1
  serviceAccountName: prometheus-agent
  replicas: %d
  shards: %d
  walCompression: true
  replicaExternalLabelName: ""
  externalLabels: {mih_cluster_id: gh62-%s}
%s%s  remoteWrite:
    - name: %s
      url: http://%s.%s.svc:8480/insert/%s/prometheus/api/v1/write
      messageVersion: V1.0
`, name, testNamespace, replicas, shards, config.testRunID, storage, selector, gh62RemoteWriteName, remoteService, testNamespace, gh62AccountID)
}

func gh62WALAgentManifest(clusterID string) string {
	return fmt.Sprintf(`
apiVersion: monitoring.coreos.com/v1alpha1
kind: PrometheusAgent
metadata: {name: gh62-wal, namespace: %s}
spec:
  version: v2.55.1
  image: quay.io/prometheus/prometheus:v2.55.1
  serviceAccountName: prometheus-agent
  replicas: 1
  shards: 1
  walCompression: true
  replicaExternalLabelName: ""
  externalLabels: {mih_cluster_id: %s}
  storage:
    volumeClaimTemplate:
      spec:
        storageClassName: standard
        accessModes: [ReadWriteOnce]
        volumeMode: Filesystem
        resources: {requests: {storage: 64Mi}}
  scrapeConfigSelector: {matchLabels: {monitoring.fjsxy.github.io/gh62: wal}}
  remoteWrite:
    - name: %s
      url: http://gh62-remote-write.%s.svc:8480/insert/%s/prometheus/api/v1/write
      messageVersion: V1.0
`, testNamespace, clusterID, gh62RemoteWriteName, testNamespace, gh62AccountID)
}

func gh62WALFixtures(sampleID string) string {
	return fmt.Sprintf(`
apiVersion: v1
kind: Service
metadata: {name: gh62-remote-write, namespace: %s}
spec:
  selector: {monitoring.fjsxy.github.io/gh62-unreachable: "true"}
  ports: [{name: http, port: 8480, targetPort: http}]
---
apiVersion: monitoring.coreos.com/v1alpha1
kind: ScrapeConfig
metadata:
  name: gh62-wal
  namespace: %s
  labels: {monitoring.fjsxy.github.io/gh62: wal}
spec:
  scrapeInterval: 2s
  staticConfigs:
    - targets: [metrics-target.%s.svc:9100]
      labels: {mih_gh62_sample: %s}
`, testNamespace, testNamespace, testNamespace, sampleID)
}

const (
	gh62ConflictPVCName     = "prom-agent-gh62-conflict-db-prom-agent-gh62-conflict-0"
	gh62ConflictPVCManifest = `
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: prom-agent-gh62-conflict-db-prom-agent-gh62-conflict-0
  namespace: monitor-in-hand-test
  labels:
    app.kubernetes.io/name: prometheus-agent
    app.kubernetes.io/managed-by: prometheus-operator
    app.kubernetes.io/instance: gh62-conflict
    operator.prometheus.io/name: gh62-conflict
    operator.prometheus.io/shard: "0"
spec:
  storageClassName: standard
  accessModes: [ReadWriteOnce]
  volumeMode: Filesystem
  resources: {requests: {storage: 32Mi}}
`
)

func gh62PVCNames(agentName string, replicas, shards int) []string {
	names := make([]string, 0, replicas*shards)
	for shard := 0; shard < shards; shard++ {
		statefulSet := "prom-agent-" + agentName
		if shard > 0 {
			statefulSet += fmt.Sprintf("-shard-%d", shard)
		}
		for ordinal := 0; ordinal < replicas; ordinal++ {
			names = append(names, fmt.Sprintf("prom-agent-%s-db-%s-%d", agentName, statefulSet, ordinal))
		}
	}
	return names
}

func readGH62Pod(ctx context.Context, name string) gh62Pod {
	GinkgoHelper()
	body := kubectl(ctx, "--namespace", testNamespace, "get", "pod", name, "--output", "json")
	var pod gh62Pod
	Expect(json.Unmarshal([]byte(body), &pod)).To(Succeed())
	return pod
}

func gh62DataVolume(pod gh62Pod) gh62Volume {
	GinkgoHelper()
	// 先从 Prometheus 容器的 WAL 挂载点解析实际 volume 名称。
	volumeName := ""
	for _, container := range pod.Spec.Containers {
		if container.Name != "prometheus" {
			continue
		}
		for _, mount := range container.VolumeMounts {
			if mount.MountPath == "/prometheus" {
				volumeName = mount.Name
			}
		}
	}
	Expect(volumeName).NotTo(BeEmpty())

	// 再返回 Pod 中同名的卷源，供 EmptyDir 与 PVC 场景分别断言。
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == volumeName {
			return volume
		}
	}
	Fail("Prometheus WAL 挂载对应的 volume 不存在")
	return gh62Volume{}
}

func assertGH62EmptyDirDataVolume(pod gh62Pod) {
	GinkgoHelper()
	volume := gh62DataVolume(pod)
	Expect(volume.EmptyDir).NotTo(BeNil())
	Expect(volume.PVC).To(BeNil())
}

func assertGH62PVCDataVolume(pod gh62Pod, expectedClaim string) {
	GinkgoHelper()
	volume := gh62DataVolume(pod)
	Expect(volume.EmptyDir).To(BeNil())
	Expect(volume.PVC).NotTo(BeNil())
	Expect(volume.PVC.ClaimName).To(Equal(expectedClaim))
}

func waitForGH62ReplacementPod(ctx context.Context, agentName, podName, oldUID string) gh62Pod {
	GinkgoHelper()
	var replacement gh62Pod
	Eventually(func(g Gomega) {
		body, err := kubectlResult(ctx, "--namespace", testNamespace, "get", "pod", podName, "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), body)
		g.Expect(json.Unmarshal([]byte(body), &replacement)).To(Succeed())
		g.Expect(replacement.Metadata.UID).NotTo(BeEmpty())
		g.Expect(replacement.Metadata.UID).NotTo(Equal(oldUID))
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	waitForReadyAgentPods(ctx, testNamespace, agentName, []string{podName})
	return readGH62Pod(ctx, podName)
}

func waitForGH62PVCs(ctx context.Context, agentName string, names []string) []gh62PVC {
	GinkgoHelper()
	claims := make([]gh62PVC, len(names))
	Eventually(func(g Gomega) {
		for index, name := range names {
			body, err := kubectlResult(ctx, "--namespace", testNamespace, "get", "persistentvolumeclaim", name, "--output", "json")
			g.Expect(err).NotTo(HaveOccurred(), body)
			g.Expect(json.Unmarshal([]byte(body), &claims[index])).To(Succeed())
			g.Expect(claims[index].Metadata.UID).NotTo(BeEmpty())
			g.Expect(claims[index].Metadata.DeletionTimestamp).To(BeNil())
			g.Expect(claims[index].Status.Phase).To(Equal("Bound"))
			g.Expect(claims[index].Metadata.Labels).To(HaveKeyWithValue("operator.prometheus.io/name", agentName))
		}
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
	return claims
}

func assertGH62PVCProperties(claim gh62PVC, agentName, shard, storage string) {
	GinkgoHelper()
	Expect(claim.Metadata.DeletionTimestamp).To(BeNil())
	Expect(claim.Metadata.Labels).To(SatisfyAll(
		HaveKeyWithValue("app.kubernetes.io/name", "prometheus-agent"),
		HaveKeyWithValue("app.kubernetes.io/managed-by", "prometheus-operator"),
		HaveKeyWithValue("app.kubernetes.io/instance", agentName),
		HaveKeyWithValue("operator.prometheus.io/name", agentName),
		HaveKeyWithValue("operator.prometheus.io/shard", shard),
	))
	Expect(claim.Spec.StorageClassName).To(Equal("standard"))
	Expect(claim.Spec.AccessModes).To(ConsistOf("ReadWriteOnce"))
	Expect(claim.Spec.VolumeMode).To(Equal("Filesystem"))
	Expect(claim.Spec.Resources.Requests).To(HaveKeyWithValue("storage", storage))
}

func assertGH62PVCUIDs(ctx context.Context, agentName string, expected []gh62PVC) {
	GinkgoHelper()
	names := make([]string, 0, len(expected))
	for _, claim := range expected {
		names = append(names, claim.Metadata.Name)
	}
	current := waitForGH62PVCs(ctx, agentName, names)
	for index := range expected {
		Expect(current[index].Metadata.UID).To(Equal(expected[index].Metadata.UID), expected[index].Metadata.Name)
	}
}

func assertGH62StatefulSetStorage(ctx context.Context, agentName string) {
	GinkgoHelper()
	// 读取 Operator 在当前 Kubernetes 版本中实际生成的 StatefulSet PVC 模板。
	body := kubectl(ctx, "--namespace", testNamespace, "get", "statefulset", "prom-agent-"+agentName, "--output", "json")
	var statefulSet struct {
		Spec struct {
			VolumeClaimTemplates []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					AccessModes      []string `json:"accessModes"`
					VolumeMode       string   `json:"volumeMode"`
					StorageClassName string   `json:"storageClassName"`
					Resources        struct {
						Requests map[string]string `json:"requests"`
					} `json:"resources"`
				} `json:"spec"`
			} `json:"volumeClaimTemplates"`
		} `json:"spec"`
	}
	Expect(json.Unmarshal([]byte(body), &statefulSet)).To(Succeed())

	// 只断言两个版本共同的 volumeClaimTemplate 契约，不依赖 1.28 可能物化的 retention policy 缺省值。
	Expect(statefulSet.Spec.VolumeClaimTemplates).To(HaveLen(1))
	template := statefulSet.Spec.VolumeClaimTemplates[0]
	Expect(template.Metadata.Name).To(Equal("prom-agent-" + agentName + "-db"))
	Expect(template.Spec.StorageClassName).To(Equal("standard"))
	Expect(template.Spec.AccessModes).To(ConsistOf("ReadWriteOnce"))
	Expect(template.Spec.VolumeMode).To(Equal("Filesystem"))
	Expect(template.Spec.Resources.Requests).To(HaveKeyWithValue("storage", "64Mi"))
}

func waitForGH62ResourceAbsent(ctx context.Context, resource, name string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		body, err := kubectlResult(ctx, "--namespace", testNamespace, "get", resource, name, "--ignore-not-found", "--output", "name")
		g.Expect(err).NotTo(HaveOccurred(), body)
		g.Expect(strings.TrimSpace(body)).To(BeEmpty())
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
}

func waitForGH62StatefulSetReplacement(ctx context.Context, name, oldUID string) {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		body, err := kubectlResult(ctx, "--namespace", testNamespace, "get", "statefulset", name, "--output", "json")
		g.Expect(err).NotTo(HaveOccurred(), body)
		var object struct {
			Metadata struct {
				UID string `json:"uid"`
			} `json:"metadata"`
		}
		g.Expect(json.Unmarshal([]byte(body), &object)).To(Succeed())
		g.Expect(object.Metadata.UID).NotTo(Equal(oldUID))
	}, 5*time.Minute, 2*time.Second).Should(Succeed())
}

func runGH62Preflight(ctx context.Context, manifest string) (string, error) {
	GinkgoHelper()
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	Expect(err).NotTo(HaveOccurred())
	manifestPath := filepath.Join(GinkgoT().TempDir(), "agent.yaml")
	Expect(os.WriteFile(manifestPath, []byte(manifest), 0o600)).To(Succeed())
	command := exec.CommandContext(ctx, filepath.Join(repoRoot, "hack", "preflight.sh"),
		"--context", config.kubeContext,
		"--agent-namespace", testNamespace,
		"--agent-service-account", "prometheus-agent",
		"--config-namespaces", testNamespace,
		"--discovery-namespaces", testNamespace,
		"--agent-manifest", manifestPath,
	)
	output, commandErr := command.CombinedOutput()
	return string(output), commandErr
}

func waitForGH62BacklogGrowth(ctx context.Context, podName string) string {
	GinkgoHelper()
	port, stop := startPortForwardTarget(ctx, testNamespace, "pod/"+podName, 9090)
	DeferCleanup(stop)
	client := &http.Client{Timeout: 5 * time.Second}
	var body string
	var baselinePending, baselineLag float64
	baselineCaptured := false

	// 先捕获一次非零 backlog，再在同一观察面等待 pending 和未发送时间差严格增长。
	Eventually(func(g Gomega) {
		response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/metrics")
		g.Expect(err).NotTo(HaveOccurred())
		defer response.Body.Close()
		contents, err := io.ReadAll(response.Body)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(response.StatusCode).To(Equal(http.StatusOK), string(contents))
		body = string(contents)
		pending, lag, found := gh62BacklogMetrics(body)
		g.Expect(found).To(BeTrue(), body)
		if !baselineCaptured && pending > 0 && lag > 0 {
			baselinePending, baselineLag = pending, lag
			baselineCaptured = true
		}
		g.Expect(baselineCaptured).To(BeTrue())
		g.Expect(pending).To(BeNumerically(">", baselinePending))
		g.Expect(lag).To(BeNumerically(">", baselineLag))
	}, 4*time.Minute, 2*time.Second).Should(Succeed())
	return body
}

func gh62BacklogMetrics(contents string) (float64, float64, bool) {
	requiredLabel := `remote_name="` + gh62RemoteWriteName + `"`
	// Prometheus 2.55 以全局入队最高时间减去当前 remote queue 的已发送最高时间表示 WAL lag。
	pending, found := metricSample(contents, "prometheus_remote_storage_samples_pending", requiredLabel)
	if !found {
		return 0, 0, false
	}
	highest, found := metricSample(contents, "prometheus_remote_storage_highest_timestamp_in_seconds", "")
	if !found {
		return 0, 0, false
	}
	sent, found := metricSample(contents, "prometheus_remote_storage_queue_highest_sent_timestamp_seconds", requiredLabel)
	if !found {
		return 0, 0, false
	}
	return pending, highest - sent, true
}

func waitForGH62OutageSamples(ctx context.Context, sampleID, clusterID string, start, end time.Time) string {
	GinkgoHelper()
	path := gh62OutageQueryPath(sampleID, clusterID, start, end)
	return waitForGH61HTTPBody(ctx, "service/vmselect", 8481, path, 5*time.Minute, func(g Gomega, contents []byte) {
		// 先验证 vmselect 返回的 series 唯一且标签身份完整。
		var result gh62RangeResponse
		g.Expect(json.Unmarshal(contents, &result)).To(Succeed())
		g.Expect(result.Status).To(Equal("success"), string(contents))
		g.Expect(result.Data.Result).To(HaveLen(1), string(contents))
		series := result.Data.Result[0]
		g.Expect(series.Metric).To(SatisfyAll(
			HaveKeyWithValue("mih_gh62_sample", sampleID),
			HaveKeyWithValue("mih_cluster_id", clusterID),
			HaveKeyWithValue("prometheus", testNamespace+"/gh62-wal"),
		))

		// 再验证每个返回样本都确实生成于远端不可达且 Pod 尚未替换的时间窗。
		g.Expect(series.Values).NotTo(BeEmpty(), string(contents))
		for _, sample := range series.Values {
			g.Expect(sample).To(HaveLen(2))
			var timestamp float64
			g.Expect(json.Unmarshal(sample[0], &timestamp)).To(Succeed())
			g.Expect(timestamp).To(BeNumerically(">=", start.Unix()))
			g.Expect(timestamp).To(BeNumerically("<=", end.Unix()))
		}
	})
}

func gh62OutageQueryPath(sampleID, clusterID string, start, end time.Time) string {
	// 用测试运行身份与 Agent 身份锁定唯一 series，并把查询窗口截断在 Pod 故障之前。
	query := fmt.Sprintf(`node_uname_info{mih_gh62_sample=%q,mih_cluster_id=%q,prometheus=%q}`, sampleID, clusterID, testNamespace+"/gh62-wal")
	// WAL replay 会回填历史样本；禁用 vmselect cache，避免首次空结果遮蔽随后写入的数据。
	return fmt.Sprintf("/select/%s/prometheus/api/v1/query_range?query=%s&start=%d&end=%d&step=1&nocache=1", gh62AccountID, url.QueryEscape(query), start.Unix(), end.Unix())
}
