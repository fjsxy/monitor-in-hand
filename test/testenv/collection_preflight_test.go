package testenv_test

import (
	"bytes"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("采集配置部署预检", func() {
	It("[UT-PREFLIGHT-GH60-001] 在访问集群前拒绝一期不支持的多副本 Agent", func() {
		By("准备 replicas=2 但 shards 合法的 Agent 计划")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == *"create --dry-run=client --filename "*" --output json"* ]]; then
  printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","replicas":2,"shards":2}}'
  exit 0
fi
printf 'unexpected cluster access: %s\n' "$*" >&2
exit 1
`)

		By("通过公开预检入口执行一期拓扑检查")
		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证多副本保持上游可用但不能通过一期预检")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"check":"agent-topology"`),
			ContainSubstring(`"reason":"UnsupportedAgentReplicas"`),
		))
	})

	It("[UT-PREFLIGHT-GH60-005] 在资源名规划前拒绝非正整数 shards", func() {
		By("准备 shards=0 的非法 Agent 计划")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == *"create --dry-run=client --filename "*" --output json"* ]]; then
  printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","replicas":1,"shards":0}}'
  exit 0
fi
printf 'unexpected cluster access: %s\n' "$*" >&2
exit 1
`)

		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		By("通过公开预检入口执行 shards 拓扑检查")
		output, err := command.CombinedOutput()

		By("验证预检在任何集群访问前报告稳定拓扑原因")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"check":"agent-topology"`),
			ContainSubstring(`"reason":"InvalidAgentShards"`),
		))
	})

	It("[UT-PREFLIGHT-GH60-002] 同名配置 Secret 被无关对象占用时拒绝接管", func() {
		By("准备两分片 Agent 与占用其配置 Secret 保留名的无关对象")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
case "$*" in
  *"create --dry-run=client --filename "*" --output json"*)
    printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"edge","namespace":"monitoring"},"spec":{"serviceAccountName":"edge","replicas":1,"shards":2}}'
    ;;
  "version --output json")
    printf '%s\n' '{"serverVersion":{"major":"1","minor":"28","gitVersion":"v1.28.15"}}'
    ;;
  "get namespace "*" --output name")
    printf '%s\n' "namespace/$3"
    ;;
  "--namespace monitoring get serviceaccount edge --output name")
    printf '%s\n' 'serviceaccount/edge'
    ;;
  "--namespace monitoring get prometheusagent edge --ignore-not-found --output json")
    ;;
  "--namespace monitoring get secret prom-agent-edge --ignore-not-found --output json")
    printf '%s\n' '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"prom-agent-edge","uid":"unrelated"}}'
    ;;
  "--namespace monitoring get "*" --ignore-not-found --output json")
    ;;
  *)
    printf 'unexpected kubectl call: %s\n' "$*" >&2
    exit 1
    ;;
esac
`)

		By("通过公开预检入口检查上游生成资源的保留身份")
		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "edge",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证预检报告精确资源身份且在调和探针前非零退出")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"check":"agent-resource-identity"`),
			ContainSubstring(`"reason":"ReservedResourceConflict"`),
			ContainSubstring(`"kind":"Secret"`),
			ContainSubstring(`"name":"prom-agent-edge"`),
		))
	})

	It("[UT-PREFLIGHT-GH60-006] 拒绝同名但指向旧 StatefulSet UID 的遗留 Pod", func() {
		By("准备当前 Agent 与合法 StatefulSet，以及 owner 同名但 UID 已过期的 Pod")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
case "$*" in
  *"create --dry-run=client --filename "*" --output json"*)
    printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"edge","namespace":"monitoring"},"spec":{"serviceAccountName":"edge","replicas":1,"shards":1}}'
    ;;
  "version --output json")
    printf '%s\n' '{"serverVersion":{"major":"1","minor":"28","gitVersion":"v1.28.15"}}'
    ;;
  "get namespace "*" --output name") printf 'namespace/%s\n' "$3" ;;
  "--namespace monitoring get serviceaccount edge --output name") printf '%s\n' 'serviceaccount/edge' ;;
  "--namespace monitoring get prometheusagent edge --ignore-not-found --output json")
    printf '%s\n' '{"metadata":{"name":"edge","uid":"agent-current"}}'
    ;;
  "--namespace monitoring get statefulset prom-agent-edge --ignore-not-found --output json")
    printf '%s\n' '{"metadata":{"name":"prom-agent-edge","uid":"statefulset-current","ownerReferences":[{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","name":"edge","uid":"agent-current","controller":true}]}}'
    ;;
  "--namespace monitoring get pod prom-agent-edge-0 --ignore-not-found --output json")
    printf '%s\n' '{"metadata":{"name":"prom-agent-edge-0","uid":"pod-stale","ownerReferences":[{"apiVersion":"apps/v1","kind":"StatefulSet","name":"prom-agent-edge","uid":"statefulset-old","controller":true}]}}'
    ;;
  "--namespace monitoring get "*" --ignore-not-found --output json") ;;
  *) printf 'unexpected kubectl call: %s\n' "$*" >&2; exit 1 ;;
esac
`)

		By("通过公开预检入口核对 Pod 的直接 owner UID")
		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "edge",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证同名旧 UID Pod 被报告为保留身份冲突")
		Expect(err).To(HaveOccurred(), string(output))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"ReservedResourceConflict"`),
			ContainSubstring(`"kind":"Pod"`),
			ContainSubstring(`"name":"prom-agent-edge-0"`),
		))
	})

	It("[UT-PREFLIGHT-GH60-003] 接受同 namespace 其他 Agent 共享的合法 governing Service", func() {
		By("准备一个由另一 Agent 普通拥有的上游 headless Service")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
case "$*" in
  *"create --dry-run=client --filename "*" --output json"*)
    case "${PREFLIGHT_SERVICE_MODE:-default}" in
      custom) extra=',"serviceName":"agent-governing"' ;;
      port) extra=',"portName":"metrics-http"' ;;
      *) extra= ;;
    esac
    printf '%s\n' "{\"apiVersion\":\"monitoring.coreos.com/v1alpha1\",\"kind\":\"PrometheusAgent\",\"metadata\":{\"name\":\"second\",\"namespace\":\"monitoring\"},\"spec\":{\"serviceAccountName\":\"second\",\"replicas\":1,\"shards\":1${extra}}}"
    ;;
  "version --output json")
    printf '%s\n' '{"serverVersion":{"major":"1","minor":"28","gitVersion":"v1.28.15"}}'
    ;;
  "get namespace "*" --output name")
    printf '%s\n' "namespace/$3"
    ;;
  "--namespace monitoring get serviceaccount second --output name")
    printf '%s\n' 'serviceaccount/second'
    ;;
  "--namespace monitoring get prometheusagent second --ignore-not-found --output json")
    ;;
  "--namespace monitoring get service prometheus-agent-operated --ignore-not-found --output json")
    port_name=web
    if [[ "${PREFLIGHT_SERVICE_MODE:-default}" == port ]]; then port_name=metrics-http; fi
    jq --null-input --arg portName "${port_name}" '{apiVersion:"v1",kind:"Service",metadata:{name:"prometheus-agent-operated",labels:{"managed-by":"prometheus-operator","operated-prometheus":"true"},ownerReferences:[{apiVersion:"monitoring.coreos.com/v1alpha1",kind:"PrometheusAgent",name:"first",uid:"first-uid"}]},spec:{clusterIP:"None",type:"ClusterIP",selector:{"app.kubernetes.io/name":"prometheus-agent"},ports:[{name:$portName,port:9090,protocol:"TCP",targetPort:$portName}]}}'
    ;;
  "--namespace monitoring get prometheusagents --output json")
    printf '%s\n' '{"items":[{"metadata":{"name":"first","uid":"first-uid"}}]}'
    ;;
  "--namespace monitoring get "*" --ignore-not-found --output json")
    ;;
  *)
    printf 'expected later baseline check: %s\n' "$*" >&2
    exit 1
    ;;
esac
`)

		By("执行预检并读取 governing Service 身份诊断")
		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "second",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证共享 Service 通过身份检查，后续基线检查失败不改变该结论")
		Expect(err).To(HaveOccurred(), string(output))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"check":"governing-service"`),
			ContainSubstring(`"reason":"SharedGoverningServiceCompatible"`),
		))
		Expect(string(output)).NotTo(ContainSubstring(`"reason":"GoverningServiceConflict"`))

		By("使用自定义 serviceName 执行预检，默认共享 Service 不再属于该计划")
		customCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "second",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		customCommand.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "PREFLIGHT_SERVICE_MODE=custom")
		customOutput, customErr := customCommand.CombinedOutput()

		By("验证自定义 governing Service 跳过默认名称检查")
		Expect(customErr).To(HaveOccurred(), string(customOutput))
		expectJSONDiagnostics(customOutput)
		Expect(string(customOutput)).To(ContainSubstring(`"reason":"CustomGoverningServiceConfigured"`))
		Expect(string(customOutput)).NotTo(ContainSubstring(`"reason":"GoverningServiceConflict"`))

		By("使用自定义 portName 执行默认共享 Service 结构检查")
		portCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "second",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		portCommand.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "PREFLIGHT_SERVICE_MODE=port")
		portOutput, portErr := portCommand.CombinedOutput()

		By("验证默认 Service 使用 Agent portName 后仍被识别为合法共享结构")
		Expect(portErr).To(HaveOccurred(), string(portOutput))
		expectJSONDiagnostics(portOutput)
		Expect(string(portOutput)).To(ContainSubstring(`"reason":"SharedGoverningServiceCompatible"`))
		Expect(string(portOutput)).NotTo(ContainSubstring(`"reason":"GoverningServiceConflict"`))
	})

	It("[UT-PREFLIGHT-GH60-004] 只接受身份与存储属性匹配的 retained PVC", func() {
		By("准备带项目身份 labels 且与 volumeClaimTemplate 匹配的保留 PVC")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
case "$*" in
  *"create --dry-run=client --filename "*" --output json"*)
    case "${PREFLIGHT_STORAGE_VARIANT:-}" in
      upstream)
        printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"wal","namespace":"monitoring"},"spec":{"serviceAccountName":"wal","replicas":0,"shards":1,"storage":{"volumeClaimTemplate":{"spec":{"accessModes":["ReadWriteMany"],"volumeMode":"Block"}}}}}'
        ;;
      quantity)
        printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"wal","namespace":"monitoring"},"spec":{"serviceAccountName":"wal","replicas":0,"shards":1,"storage":{"volumeClaimTemplate":{"spec":{"accessModes":["ReadWriteOnce"],"volumeMode":"Filesystem","storageClassName":"fast","resources":{"requests":{"storage":"1024Mi"}}}}}}}'
        ;;
      empty-class | missing-class)
        printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"wal","namespace":"monitoring"},"spec":{"serviceAccountName":"wal","replicas":0,"shards":1,"storage":{"volumeClaimTemplate":{"spec":{"accessModes":["ReadWriteOnce"],"volumeMode":"Filesystem","storageClassName":"","resources":{"requests":{"storage":"2Gi"}}}}}}}'
        ;;
      *)
        printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"wal","namespace":"monitoring"},"spec":{"serviceAccountName":"wal","replicas":0,"shards":1,"storage":{"volumeClaimTemplate":{"spec":{"accessModes":["ReadWriteOnce"],"volumeMode":"Filesystem","storageClassName":"fast","resources":{"requests":{"storage":"2Gi"}}}}}}}'
        ;;
    esac
    ;;
  "create quota mih-preflight-storage-quantity --hard=requests.storage="*" --dry-run=client --output json")
    quantity=${*#*--hard=requests.storage=}
    quantity=${quantity%% *}
    if [[ "${quantity}" == 1024Mi ]]; then quantity=1Gi; fi
    jq --null-input --arg quantity "${quantity}" '{spec:{hard:{"requests.storage":$quantity}}}'
    ;;
  "version --output json")
    printf '%s\n' '{"serverVersion":{"major":"1","minor":"28","gitVersion":"v1.28.15"}}'
    ;;
  "get namespace "*" --output name")
    printf '%s\n' "namespace/$3"
    ;;
  "--namespace monitoring get serviceaccount wal --output name")
    printf '%s\n' 'serviceaccount/wal'
    ;;
  "--namespace monitoring get prometheusagent wal --ignore-not-found --output json")
    ;;
  "--namespace monitoring get persistentvolumeclaim prom-agent-wal-db-prom-agent-wal-0 --ignore-not-found --output json")
    if [[ "${PREFLIGHT_STORAGE_VARIANT:-}" == upstream ]]; then exit 0; fi
    storage_class=fast
    storage=2Gi
    if [[ "${PREFLIGHT_STORAGE_VARIANT:-}" == quantity ]]; then storage=1Gi; fi
    if [[ "${PREFLIGHT_PVC_MISMATCH:-}" == 1 ]]; then storage_class=unrelated; fi
    jq --null-input --arg storageClass "${storage_class}" --arg storage "${storage}" --arg variant "${PREFLIGHT_STORAGE_VARIANT:-}" '{apiVersion:"v1",kind:"PersistentVolumeClaim",metadata:{name:"prom-agent-wal-db-prom-agent-wal-0",labels:{"app.kubernetes.io/name":"prometheus-agent","app.kubernetes.io/managed-by":"prometheus-operator","app.kubernetes.io/instance":"wal","operator.prometheus.io/name":"wal","operator.prometheus.io/shard":"0"}},spec:{accessModes:["ReadWriteOnce"],volumeMode:"Filesystem",storageClassName:$storageClass,resources:{requests:{storage:$storage}}}} | if $variant == "missing-class" then del(.spec.storageClassName) else . end'
    ;;
  "--namespace monitoring get "*" --ignore-not-found --output json")
    ;;
  *)
    printf 'expected later baseline check: %s\n' "$*" >&2
    exit 1
    ;;
esac
`)

		By("执行预检并观察 retained PVC 恢复边界")
		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "wal",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证合法 retained PVC 被识别为同 namespace/name Agent 的恢复身份")
		Expect(err).To(HaveOccurred(), string(output))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"check":"retained-pvc"`),
			ContainSubstring(`"reason":"RetainedPVCCompatible"`),
			ContainSubstring(`"name":"prom-agent-wal-db-prom-agent-wal-0"`),
		))
		Expect(string(output)).NotTo(ContainSubstring(`"reason":"RetainedPVCConflict"`))

		By("把同名 PVC 的 StorageClass 改为与计划不匹配的来源")
		conflictCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "wal",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		conflictCommand.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "PREFLIGHT_PVC_MISMATCH=1")
		conflictOutput, conflictErr := conflictCommand.CombinedOutput()

		By("验证预检非零拒绝静默复用存储属性不匹配的 PVC")
		Expect(conflictErr).To(HaveOccurred(), string(conflictOutput))
		Expect(conflictErr.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(conflictOutput)
		Expect(string(conflictOutput)).To(SatisfyAll(
			ContainSubstring(`"check":"retained-pvc"`),
			ContainSubstring(`"reason":"RetainedPVCConflict"`),
			ContainSubstring(`"name":"prom-agent-wal-db-prom-agent-wal-0"`),
		))

		By("准备上游允许但一期未承诺兼容性的 PVC template，且不存在 retained claim")
		upstreamCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "wal",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		upstreamCommand.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "PREFLIGHT_STORAGE_VARIANT=upstream")
		By("对一期未承诺的 storage 变体执行公开预检入口")
		upstreamOutput, upstreamErr := upstreamCommand.CombinedOutput()

		By("验证预检不因不存在的 retained PVC 而收窄上游 storage 能力")
		Expect(upstreamErr).To(HaveOccurred(), string(upstreamOutput))
		expectJSONDiagnostics(upstreamOutput)
		Expect(string(upstreamOutput)).NotTo(ContainSubstring(`"reason":"UnsupportedAgentStorage"`))
		Expect(string(upstreamOutput)).To(ContainSubstring(`"reason":"RetainedPVCIdentitiesChecked"`))

		By("准备使用等价 Kubernetes Quantity 表达的 Agent 计划与 retained PVC")
		quantityCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "wal",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		quantityCommand.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "PREFLIGHT_STORAGE_VARIANT=quantity")
		By("对两种等价 Quantity 表达执行公开预检入口")
		quantityOutput, quantityErr := quantityCommand.CombinedOutput()

		By("验证 1024Mi 与 API 规范化后的 1Gi 被识别为同一容量")
		Expect(quantityErr).To(HaveOccurred(), string(quantityOutput))
		expectJSONDiagnostics(quantityOutput)
		Expect(string(quantityOutput)).To(ContainSubstring(`"reason":"RetainedPVCCompatible"`))
		Expect(string(quantityOutput)).NotTo(ContainSubstring(`"reason":"RetainedPVCConflict"`))

		By("准备显式禁用 StorageClass、但同名 PVC 已使用 fast 类的冲突计划")
		emptyClassCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "wal",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		emptyClassCommand.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "PREFLIGHT_STORAGE_VARIANT=empty-class")
		By("对显式空 StorageClass 与已有非空类冲突执行公开预检入口")
		emptyClassOutput, emptyClassErr := emptyClassCommand.CombinedOutput()

		By("验证显式空 StorageClass 不会被缺省语义吞掉或复用任意类 PVC")
		Expect(emptyClassErr).To(HaveOccurred(), string(emptyClassOutput))
		expectJSONDiagnostics(emptyClassOutput)
		Expect(string(emptyClassOutput)).To(ContainSubstring(`"reason":"RetainedPVCConflict"`))

		By("准备显式禁用 StorageClass、但同名 PVC 省略该字段而会触发集群默认类的冲突计划")
		missingClassCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "wal",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		missingClassCommand.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"), "PREFLIGHT_STORAGE_VARIANT=missing-class")
		By("对显式空 StorageClass 与已有缺省字段冲突执行公开预检入口")
		missingClassOutput, missingClassErr := missingClassCommand.CombinedOutput()

		By("验证已有 PVC 缺省 StorageClass 与计划显式空串保持不同语义")
		Expect(missingClassErr).To(HaveOccurred(), string(missingClassOutput))
		expectJSONDiagnostics(missingClassOutput)
		Expect(string(missingClassOutput)).To(ContainSubstring(`"reason":"RetainedPVCConflict"`))
	})

	It("[UT-EXAMPLE-GH59-001] 示例固定四类非空 selector、六种 discovery 与无 nodes runtime RBAC", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		By("读取管理员实际应用的公开示例")
		agentObjects := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "agent.yaml"))
		Expect(agentObjects).To(HaveLen(1))
		agent := agentObjects[0]
		for _, field := range []string{"serviceMonitorSelector", "podMonitorSelector", "probeSelector", "scrapeConfigSelector"} {
			selector, found, err := unstructured.NestedMap(agent, "spec", field)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), field)
			Expect(selector).To(HaveKey("matchLabels"), field)
			Expect(selector).NotTo(BeEmpty(), field)
		}
		for _, field := range []string{"serviceMonitorNamespaceSelector", "podMonitorNamespaceSelector", "probeNamespaceSelector", "scrapeConfigNamespaceSelector"} {
			selector, found, err := unstructured.NestedMap(agent, "spec", field)
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue(), field)
			Expect(selector).To(HaveKey("matchLabels"), field)
		}

		By("确认 ScrapeConfig 示例覆盖 static 与五种一期 Kubernetes discovery")
		configObjects := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "collection-configs.yaml"))
		kinds := map[string]int{}
		discoveries := map[string]bool{}
		for _, object := range configObjects {
			kind, found, err := unstructured.NestedString(object, "kind")
			Expect(err).NotTo(HaveOccurred())
			Expect(found).To(BeTrue())
			kinds[kind]++
			if kind != "ScrapeConfig" {
				continue
			}
			staticConfigs, found, err := unstructured.NestedSlice(object, "spec", "staticConfigs")
			Expect(err).NotTo(HaveOccurred())
			if found && len(staticConfigs) > 0 {
				discoveries["Static"] = true
			}
			kubernetesConfigs, found, err := unstructured.NestedSlice(object, "spec", "kubernetesSDConfigs")
			Expect(err).NotTo(HaveOccurred())
			if !found {
				continue
			}
			for _, item := range kubernetesConfigs {
				config, ok := item.(map[string]any)
				Expect(ok).To(BeTrue())
				role, found, err := unstructured.NestedString(config, "role")
				Expect(err).NotTo(HaveOccurred())
				Expect(found).To(BeTrue())
				discoveries[role] = true
			}
		}
		Expect(kinds).To(HaveKeyWithValue("ServiceMonitor", 1))
		Expect(kinds).To(HaveKeyWithValue("PodMonitor", 1))
		Expect(kinds).To(HaveKeyWithValue("Probe", 1))
		Expect(discoveries).To(Equal(map[string]bool{
			"Static": true, "Pod": true, "Service": true, "Endpoints": true, "EndpointSlice": true, "Ingress": true,
		}))

		By("确认统一 runtime ClusterRole 覆盖一期资源且没有 nodes 权限")
		rbacObjects := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "runtime-rbac.yaml"))
		var role map[string]any
		for _, object := range rbacObjects {
			if object["kind"] == "ClusterRole" {
				role = object
			}
		}
		Expect(role).NotTo(BeNil())
		rules, found, err := unstructured.NestedSlice(role, "rules")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		resources := []string{}
		hasMetrics := false
		for _, item := range rules {
			rule := item.(map[string]any)
			if ruleResources, ok := rule["resources"].([]any); ok {
				for _, resource := range ruleResources {
					resources = append(resources, resource.(string))
				}
			}
			if urls, ok := rule["nonResourceURLs"].([]any); ok {
				for _, url := range urls {
					hasMetrics = hasMetrics || url == "/metrics"
				}
			}
		}
		sort.Strings(resources)
		Expect(resources).To(ConsistOf("services", "endpoints", "pods", "configmaps", "ingresses", "endpointslices"))
		Expect(resources).NotTo(ContainElement("nodes"))
		Expect(hasMetrics).To(BeTrue())
	})

	It("[UT-EXAMPLE-GH60-001] 每个 Agent 独立绑定共享 runtime ClusterRole 且管理员资源无 owner", func() {
		By("读取 Agent 与 runtime RBAC 公开示例")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		agent := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "agent.yaml"))[0]
		rbacObjects := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "runtime-rbac.yaml"))

		By("验证 ServiceAccount 与 ClusterRoleBinding 表达 namespace/name Agent 身份")
		objectsByKind := map[string]map[string]any{}
		for _, object := range rbacObjects {
			objectsByKind[object["kind"].(string)] = object
			metadata := object["metadata"].(map[string]any)
			Expect(metadata).NotTo(HaveKey("ownerReferences"), object["kind"])
		}
		serviceAccount := objectsByKind["ServiceAccount"]
		binding := objectsByKind["ClusterRoleBinding"]
		role := objectsByKind["ClusterRole"]
		Expect(serviceAccount["metadata"]).To(HaveKeyWithValue("name", "prometheus-agent-example"))
		Expect(binding["metadata"]).To(HaveKeyWithValue("name", "prometheus-agent-monitoring-example"))
		Expect(binding["subjects"]).To(ConsistOf(map[string]any{
			"kind": "ServiceAccount", "name": "prometheus-agent-example", "namespace": "monitoring",
		}))

		By("验证绑定引用共享的一期 runtime ClusterRole，而非创建 Agent 专属角色")
		Expect(role["metadata"]).To(HaveKeyWithValue("name", "prometheus-agent-runtime"))
		Expect(binding["roleRef"]).To(HaveKeyWithValue("name", "prometheus-agent-runtime"))
		serviceAccountName, found, err := unstructured.NestedString(agent, "spec", "serviceAccountName")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(serviceAccountName).To(Equal("prometheus-agent-example"))
	})

	It("[UT-EXAMPLE-GH61-001] 一期示例固定两分片身份与 V1 remote_write", func() {
		By("读取管理员实际应用的 Agent 公开示例")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		agent := readManifestObjects(filepath.Join(root, "examples", "prometheus-agent", "agent.yaml"))[0]

		By("验证 Prometheus 运行版本、两分片拓扑和稳定集群身份")
		Expect(agent).To(HaveKeyWithValue("spec", SatisfyAll(
			HaveKeyWithValue("version", "v2.55.1"),
			HaveKeyWithValue("image", "quay.io/prometheus/prometheus:v2.55.1"),
			HaveKeyWithValue("replicas", BeNumerically("==", 1)),
			HaveKeyWithValue("shards", BeNumerically("==", 2)),
			HaveKeyWithValue("walCompression", true),
			HaveKeyWithValue("replicaExternalLabelName", ""),
			HaveKeyWithValue("externalLabels", HaveKeyWithValue("mih_cluster_id", "replace-with-stable-cluster-id")),
			Not(HaveKey("additionalArgs")),
		)))

		By("验证唯一 remote_write 目标使用无认证、无 TLS 的集群内 V1 URL")
		remoteWrites, found, err := unstructured.NestedSlice(agent, "spec", "remoteWrite")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		Expect(remoteWrites).To(HaveLen(1))
		remoteWrite := remoteWrites[0].(map[string]any)
		Expect(remoteWrite).To(SatisfyAll(
			HaveKeyWithValue("name", "victoriametrics-primary"),
			HaveKeyWithValue("messageVersion", "V1.0"),
			Not(HaveKey("basicAuth")),
			Not(HaveKey("authorization")),
			Not(HaveKey("oauth2")),
			Not(HaveKey("sigv4")),
			Not(HaveKey("tlsConfig")),
		))
		endpoint, err := url.Parse(remoteWrite["url"].(string))
		Expect(err).NotTo(HaveOccurred())
		Expect(endpoint.Scheme).To(Equal("http"))
		Expect(endpoint.Host).To(Equal("vminsert.monitoring.svc:8480"))
		Expect(endpoint.User).To(BeNil())
		Expect(endpoint.RawQuery).To(BeEmpty())
		Expect(endpoint.Fragment).To(BeEmpty())
	})

	It("[UT-BOUNDARY-GH60-001] 本 Operator 只新增配套 ServiceMonitor 权限且不管理 Agent", func() {
		By("读取会被 Helm 机械同步的 manager ClusterRole 生成源")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		objects := readManifestObjects(filepath.Join(root, "config", "rbac", "role.yaml"))
		Expect(objects).To(HaveLen(1))

		By("验证 monitoring API 权限仅覆盖配套 servicemonitors")
		rules, found, err := unstructured.NestedSlice(objects[0], "rules")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeTrue())
		for _, item := range rules {
			rule := item.(map[string]any)
			apiGroups, _, err := unstructured.NestedStringSlice(rule, "apiGroups")
			Expect(err).NotTo(HaveOccurred())
			if len(apiGroups) == 1 && apiGroups[0] == "monitoring.coreos.com" {
				resources, _, err := unstructured.NestedStringSlice(rule, "resources")
				Expect(err).NotTo(HaveOccurred())
				Expect(resources).To(ConsistOf("servicemonitors"))
			}
		}
	})

	It("[UT-BOUNDARY-GH60-002] 双版本门禁在 Agent 反向边界前运行当前工作树 Operator", func() {
		By("通过公开 Make 入口读取独立 Agent lane 的实际编排")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		command := exec.Command("make", "--dry-run", "e2e-agent",
			"KUBERNETES_VERSION=1.28",
			"TEST_RUN_ID=agent-lane-contract",
			"HELM=helm",
			"GINKGO=ginkgo",
			"CONTAINER_TOOL=docker",
			"KIND=kind",
			"KUBECTL=kubectl",
		)
		command.Dir = root
		output, err := command.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), string(output))

		By("定位预检、本 Operator 安装与 Agent collection 三个有状态阶段")
		plan := string(output)
		preflightIndex := bytes.Index(output, []byte("./test/e2e/preflight"))
		operatorIndex := bytes.Index(output, []byte("helm upgrade --install"))
		collectionIndex := bytes.Index(output, []byte("./test/e2e/collection"))

		By("验证 lane 自给自足且不串行重跑 KubeStateMetrics suite")
		Expect(preflightIndex).To(BeNumerically(">=", 0))
		Expect(operatorIndex).To(BeNumerically(">", preflightIndex))
		Expect(collectionIndex).To(BeNumerically(">", operatorIndex))
		Expect(plan).NotTo(ContainSubstring("./test/e2e/kubestatemetrics"))
	})

	It("[UT-PREFLIGHT-GH59-001] 拒绝一期范围外的空 object selector", func() {
		By("准备带空 ServiceMonitor object selector 的 Agent manifest")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == *"create --dry-run=client --filename "*" --output json"* ]]; then
  printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","serviceMonitorSelector":{}}}'
  exit 0
fi
printf 'unexpected kubectl call: %s\n' "$*" >&2
exit 1
`)

		By("通过公开预检入口执行计划检查")
		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证预检以支持资格失败而不是输入错误拒绝空 selector")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"check":"collection-configuration"`),
			ContainSubstring(`"reason":"EmptyObjectSelectorUnsupported"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-002] 拒绝 Node、FileSD 与其他一期外 ScrapeConfig discovery", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		for _, unsupported := range []struct {
			name string
			spec string
		}{
			{name: "Node", spec: `{"kubernetesSDConfigs":[{"role":"Node"}]}`},
			{name: "Node metadata", spec: `{"kubernetesSDConfigs":[{"role":"Pod","attachMetadata":{"node":true}}]}`},
			{name: "FileSD", spec: `{"fileSDConfigs":[{"files":["/etc/prometheus/targets.json"]}]}`},
			{name: "DNS", spec: `{"dnsSDConfigs":[{"names":["metrics.example"]}]}`},
			{name: "LightSail", spec: `{"lightSailSDConfigs":[{}]}`},
		} {
			By("准备一期外 " + unsupported.name + " discovery 计划")
			fakeBin := GinkgoT().TempDir()
			agentManifest := filepath.Join(fakeBin, "agent.yaml")
			configManifest := filepath.Join(fakeBin, "scrape-config.yaml")
			Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
			Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())

			writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
case "$*" in
  *"agent.yaml"*)
    printf '%s\n' '{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"monitoring.fjsxy.github.io/agent":"example"}}}}'
    ;;
  *"scrape-config.yaml"*)
    printf '%s\n' "{\"apiVersion\":\"monitoring.coreos.com/v1alpha1\",\"kind\":\"ScrapeConfig\",\"metadata\":{\"name\":\"unsupported\",\"namespace\":\"monitoring\",\"labels\":{\"monitoring.fjsxy.github.io/agent\":\"example\"}},\"spec\":${PREFLIGHT_UNSUPPORTED_SPEC}}"
    ;;
  *)
    printf 'unexpected kubectl call: %s\n' "$*" >&2
    exit 1
    ;;
esac
`)

			By("通过公开预检入口执行 discovery 支持子集检查")
			command := exec.Command(
				filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring",
				"--agent-service-account", "agent",
				"--config-namespaces", "monitoring",
				"--discovery-namespaces", "monitoring",
				"--agent-manifest", agentManifest,
				"--collection-config-manifest", configManifest,
			)
			command.Env = append(os.Environ(),
				"PATH="+fakeBin+":"+os.Getenv("PATH"),
				"PREFLIGHT_UNSUPPORTED_SPEC="+unsupported.spec,
			)
			output, err := command.CombinedOutput()

			By("验证一期外 discovery 以可定位原因非零退出")
			Expect(err).To(HaveOccurred(), string(output))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(output)
			Expect(string(output)).To(ContainSubstring(`"reason":"UnsupportedScrapeConfigDiscovery"`))
		}
	})

	It("[UT-PREFLIGHT-GH59-003] 空 namespace selector 要求覆盖 Operator watch scope 内全部 namespace", func() {
		By("准备跨 namespace 的 PodMonitor 与空 namespace selector")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","podMonitorSelector":{"matchLabels":{"mih":"selected"}},"podMonitorNamespaceSelector":{}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"PodMonitor","metadata":{"name":"selected","namespace":"shared","labels":{"mih":"selected"}},"spec":{}}`,
			false,
		)

		By("以不足的 config namespace 范围执行预检")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "shared",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证空 selector 的全部 watch scope 语义导致覆盖不足失败")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionNamespaceScopeIncomplete"`),
			ContainSubstring(`"requiredNamespaces"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-004] 在采集配置自身 namespace 检查 Secret 引用", func() {
		By("准备引用缺失 Secret 的同 namespace ServiceMonitor")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())

		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","serviceMonitorSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"endpoints":[{"basicAuth":{"password":{"name":"missing-credentials","key":"password"}}}]}}`,
			true,
		)

		By("通过公开预检入口执行引用检查")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证诊断保留声明对象 namespace 与缺失 Secret 名称")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionConfigsSelected"`),
			ContainSubstring(`"reason":"ReferencedAssetUnavailable"`),
			ContainSubstring(`"namespace":"monitoring"`),
			ContainSubstring(`"name":"missing-credentials"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-007] 跨 namespace 同名 ConfigMap 不满足采集配置引用", func() {
		By("准备在 monitoring namespace 引用缺失 ConfigMap 的 ServiceMonitor")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","serviceMonitorSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"endpoints":[{"tlsConfig":{"ca":{"configMap":{"name":"missing-ca","key":"ca.crt"}}}}]}}`,
			false,
		)

		By("通过公开预检入口执行 ConfigMap 引用检查")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证只查询声明对象 namespace，并以 ConfigMap 缺失原因非零退出")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"ReferencedAssetUnavailable"`),
			ContainSubstring(`"namespace":"monitoring"`),
			ContainSubstring(`"kind":"configmap"`),
			ContainSubstring(`"name":"missing-ca"`),
		))
	})

	It("[UT-PREFLIGHT-GH59-008] Secret/ConfigMap 存在但缺少引用 key 时预检失败", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		for _, fixture := range []struct {
			name, reference, expectedName, expectedKey string
			configMapExists                            bool
		}{
			{
				name:         "Secret",
				reference:    `"basicAuth":{"password":{"name":"missing-credentials","key":"password"}}`,
				expectedName: "missing-credentials",
				expectedKey:  "password",
			},
			{
				name:            "ConfigMap",
				reference:       `"tlsConfig":{"ca":{"configMap":{"name":"missing-ca","key":"ca.crt"}}}`,
				expectedName:    "missing-ca",
				expectedKey:     "ca.crt",
				configMapExists: true,
			},
		} {
			By("准备引用同 namespace " + fixture.name + " 缺失 key 的 ServiceMonitor")
			fakeBin := GinkgoT().TempDir()
			agentManifest := filepath.Join(fakeBin, "agent.yaml")
			configManifest := filepath.Join(fakeBin, "config.yaml")
			Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
			Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
			writeCollectionValidationKubectl(fakeBin,
				`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","serviceMonitorSelector":{"matchLabels":{"mih":"selected"}}}}`,
				`{"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"endpoints":[{`+fixture.reference+`}]}}`,
				false,
				fixture.configMapExists,
			)

			By("通过公开预检入口执行 " + fixture.name + " 引用 key 覆盖检查")
			command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring", "--agent-service-account", "agent",
				"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
				"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
			)
			command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
			output, err := command.CombinedOutput()

			By("验证对象存在仍因缺少引用 key 非零退出")
			Expect(err).To(HaveOccurred(), string(output))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(output)
			Expect(string(output)).To(SatisfyAll(
				ContainSubstring(`"reason":"ReferencedAssetKeyUnavailable"`),
				ContainSubstring(`"namespace":"monitoring"`),
				ContainSubstring(`"name":"`+fixture.expectedName+`"`),
				ContainSubstring(`"key":"`+fixture.expectedKey+`"`),
			))
		}
	})

	It("[UT-PREFLIGHT-GH59-005] Namespace LabelSelector 的 NotIn 匹配缺少该 label 的 namespace", func() {
		By("准备位于无 excluded label namespace 的 PodMonitor")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","podMonitorSelector":{"matchLabels":{"mih":"selected"}},"podMonitorNamespaceSelector":{"matchExpressions":[{"key":"excluded","operator":"NotIn","values":["true"]}]}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"PodMonitor","metadata":{"name":"selected","namespace":"shared","labels":{"mih":"selected"}},"spec":{}}`,
			false,
		)

		By("执行包含 NotIn namespace selector 的计划预检")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "*", "--discovery-namespaces", "shared",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证 selector 阶段成功，并在 fixture 未实现的后续 API discovery 边界明确失败")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionConfigsSelected"`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"CollectionConfigCoverageIncomplete"`)),
		))
	})

	It("[UT-PREFLIGHT-GH59-006] 空 namespace selector 只要求覆盖 Operator 实际 watch scope", func() {
		By("准备受限 Operator watch scope 与 scope 外 namespace")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","podMonitorSelector":{"matchLabels":{"mih":"selected"}},"podMonitorNamespaceSelector":{}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1","kind":"PodMonitor","metadata":{"name":"selected","namespace":"shared","labels":{"mih":"selected"}},"spec":{}}`,
			false,
		)
		GinkgoT().Setenv("PREFLIGHT_OPERATOR_ARGS", `--namespaces=monitoring,shared`)

		By("执行 Operator common watch scope 受限的计划预检")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring,shared", "--discovery-namespaces", "shared",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证 selector 阶段成功，并在 fixture 未实现的后续 API discovery 边界明确失败")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"CollectionNamespaceScopeCovered"`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"CollectionNamespaceScopeIncomplete"`)),
		))
	})

	It("[UT-PREFLIGHT-GH59-009] 不把 static labels 中的 name/key 当作 Secret 引用", func() {
		By("准备 labels 恰好包含 name/key 的合法静态 ScrapeConfig")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"staticConfigs":[{"targets":["example:9090"],"labels":{"name":"workload","key":"value"}}]}}`,
			false,
		)

		By("执行引用识别阶段并保留 fixture 后续阶段的预期失败")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证静态 label 未触发伪引用，并在后续 API discovery 边界明确失败")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"DiscoveryNamespaceScopeCovered"`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"ReferencedAssetUnavailable"`)),
			Not(ContainSubstring(`"reason":"ReferencedAssetKeyUnavailable"`)),
		))
	})

	It("[UT-PREFLIGHT-GH59-010] ScrapeConfig 省略 namespaces 时要求全 namespace discovery 覆盖", func() {
		By("准备使用 Kubernetes discovery 原生全 namespace 默认值的 ScrapeConfig")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":{"kubernetesSDConfigs":[{"role":"Pod"}]}}`,
			false,
		)

		By("以单 namespace discovery 声明执行严格预检")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证全 namespace 原生语义因 runtime RBAC 声明覆盖不足而非零退出")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"DiscoveryNamespaceScopeIncomplete"`),
			ContainSubstring(`"requiredDiscoveryNamespaces":["*"]`),
		))
	})

	It("[UT-PREFLIGHT-GH59-011] ScrapeConfig ownNamespace 指向 Agent namespace", func() {
		By("准备位于其他配置 namespace、但 discovery 使用 Agent ownNamespace 的 ScrapeConfig")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"mih":"selected"}},"scrapeConfigNamespaceSelector":{}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"shared","labels":{"mih":"selected"}},"spec":{"kubernetesSDConfigs":[{"role":"Pod","namespaces":{"ownNamespace":true}}]}}`,
			false,
		)

		By("以 Agent namespace 的 runtime discovery 覆盖执行计划检查")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "*", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证 ownNamespace 不被误解为 ScrapeConfig 对象所在 namespace")
		Expect(err).To(HaveOccurred(), string(output))
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"DiscoveryNamespaceScopeCovered"`),
			ContainSubstring(`"requiredDiscoveryNamespaces":["monitoring"]`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"DiscoveryNamespaceScopeIncomplete"`)),
		))
	})

	It("[UT-PREFLIGHT-GH59-012] ServiceMonitor、PodMonitor 与 Probe target namespace 必须被 runtime discovery 范围覆盖", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		for _, fixture := range []struct {
			name, objectField, kind, spec string
		}{
			{name: "ServiceMonitor", objectField: "serviceMonitorSelector", kind: "ServiceMonitor", spec: `{"namespaceSelector":{"matchNames":["shared"]},"selector":{},"endpoints":[{"port":"metrics"}]}`},
			{name: "PodMonitor", objectField: "podMonitorSelector", kind: "PodMonitor", spec: `{"namespaceSelector":{"matchNames":["shared"]},"selector":{},"podMetricsEndpoints":[{"port":"metrics"}]}`},
			{name: "Probe ingress", objectField: "probeSelector", kind: "Probe", spec: `{"prober":{"url":"blackbox:9115"},"targets":{"ingress":{"namespaceSelector":{"matchNames":["shared"]},"selector":{}}}}`},
		} {
			By("准备发现 shared namespace target 的 " + fixture.name)
			fakeBin := GinkgoT().TempDir()
			agentManifest := filepath.Join(fakeBin, "agent.yaml")
			configManifest := filepath.Join(fakeBin, "config.yaml")
			Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
			Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
			writeCollectionValidationKubectl(fakeBin,
				`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","`+fixture.objectField+`":{"matchLabels":{"mih":"selected"}}}}`,
				`{"apiVersion":"monitoring.coreos.com/v1","kind":"`+fixture.kind+`","metadata":{"name":"selected","namespace":"monitoring","labels":{"mih":"selected"}},"spec":`+fixture.spec+`}`,
				false,
			)

			By("以未覆盖 shared 的 runtime discovery 范围执行严格预检")
			command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring", "--agent-service-account", "agent",
				"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
				"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
			)
			command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
			output, err := command.CombinedOutput()

			By("验证 " + fixture.name + " 的真实 target namespace 覆盖不足导致非零退出")
			Expect(err).To(HaveOccurred(), string(output))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(output)
			Expect(string(output)).To(SatisfyAll(
				ContainSubstring(`"reason":"DiscoveryNamespaceScopeIncomplete"`),
				ContainSubstring(`"requiredDiscoveryNamespaces":["shared"]`),
			))
		}
	})

	It("[UT-PREFLIGHT-GH61-001] 拒绝产生或修改 mih_cluster_id 的一期采集配置", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fixtures := []struct {
			name, objectField, config string
		}{
			{
				name:        "ServiceMonitor relabeling",
				objectField: "serviceMonitorSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"selector":{},"endpoints":[{"port":"metrics","relabelings":[{"targetLabel":"mih_cluster_id","replacement":"forged"}]}]}}`,
			},
			{
				name:        "ServiceMonitor targetLabels",
				objectField: "serviceMonitorSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1","kind":"ServiceMonitor","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"selector":{},"targetLabels":["mih_cluster_id"],"endpoints":[{"port":"metrics"}]}}`,
			},
			{
				name:        "PodMonitor podTargetLabels",
				objectField: "podMonitorSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1","kind":"PodMonitor","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"selector":{},"podTargetLabels":["mih_cluster_id"],"podMetricsEndpoints":[{"port":"metrics"}]}}`,
			},
			{
				name:        "Probe static labels",
				objectField: "probeSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1","kind":"Probe","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"prober":{"url":"blackbox:9115"},"targets":{"staticConfig":{"static":["https://example.invalid"],"labels":{"mih_cluster_id":"forged"}}}}}`,
			},
			{
				name:        "ScrapeConfig metric relabeling",
				objectField: "scrapeConfigSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"staticConfigs":[{"targets":["target:9090"]}],"metricRelabelings":[{"targetLabel":"mih_cluster_id","replacement":"forged"}]}}`,
			},
			{
				name:        "ScrapeConfig explicit labelmap replacement",
				objectField: "scrapeConfigSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"kubernetesSDConfigs":[{"role":"Pod","namespaces":{"names":["monitoring"]}}],"relabelings":[{"action":"labelmap","regex":"__meta_kubernetes_pod_label_(.+)","replacement":"mih_cluster_id"}]}}`,
			},
			{
				name:        "ScrapeConfig explicit labelmap capture",
				objectField: "scrapeConfigSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"kubernetesSDConfigs":[{"role":"Pod","namespaces":{"names":["monitoring"]}}],"relabelings":[{"action":"labelmap","regex":"__meta_kubernetes_pod_label_(mih_cluster_id)","replacement":"$1"}]}}`,
			},
			{
				name:        "ScrapeConfig selected labelmap capture",
				objectField: "scrapeConfigSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"kubernetesSDConfigs":[{"role":"Pod","namespaces":{"names":["monitoring"]}}],"relabelings":[{"action":"labelmap","regex":"(team)_(mih_cluster_id)","replacement":"$2"}]}}`,
			},
			{
				name:        "ScrapeConfig nested labelmap capture",
				objectField: "scrapeConfigSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"kubernetesSDConfigs":[{"role":"Pod","namespaces":{"names":["monitoring"]}}],"relabelings":[{"action":"labelmap","regex":"(prefix_(mih_cluster_id))","replacement":"$2"}]}}`,
			},
			{
				name:        "ScrapeConfig case-insensitive labelmap capture",
				objectField: "scrapeConfigSelector",
				config:      `{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"kubernetesSDConfigs":[{"role":"Pod","namespaces":{"names":["monitoring"]}}],"relabelings":[{"action":"labelmap","regex":"(?i)(MIH_CLUSTER_ID)","replacement":"$1"}]}}`,
			},
		}

		for _, fixture := range fixtures {
			By("准备会产生或修改保护标签的 " + fixture.name)
			fakeBin := GinkgoT().TempDir()
			agentManifest := filepath.Join(fakeBin, "agent.yaml")
			configManifest := filepath.Join(fakeBin, "config.yaml")
			Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
			Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
			writeCollectionValidationKubectl(fakeBin,
				`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","`+fixture.objectField+`":{"matchLabels":{"mih":"selected"}}}}`,
				fixture.config,
				false,
			)

			By("通过公开预检入口扫描 " + fixture.name)
			command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring", "--agent-service-account", "agent",
				"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
				"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
			)
			command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
			output, err := command.CombinedOutput()

			By("验证保护标签冲突在任何集群访问前稳定失败")
			Expect(err).To(HaveOccurred(), fixture.name+"\n"+string(output))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(output)
			Expect(string(output)).To(SatisfyAll(
				ContainSubstring(`"check":"collection-configuration"`),
				ContainSubstring(`"reason":"ProtectedExternalLabelMutation"`),
				ContainSubstring(`"label":"mih_cluster_id"`),
				Not(ContainSubstring(`"check":"kubernetes-version"`)),
			))
		}
	})

	It("[UT-PREFLIGHT-GH61-002] 保留只能产生非保护标签的 labelmap 配置", func() {
		By("准备只能生成 team 或 team_ 前缀标签的 ScrapeConfig")
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		agentManifest := filepath.Join(fakeBin, "agent.yaml")
		configManifest := filepath.Join(fakeBin, "config.yaml")
		Expect(os.WriteFile(agentManifest, []byte("placeholder"), 0o600)).To(Succeed())
		Expect(os.WriteFile(configManifest, []byte("placeholder"), 0o600)).To(Succeed())
		writeCollectionValidationKubectl(fakeBin,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"PrometheusAgent","metadata":{"name":"agent","namespace":"monitoring"},"spec":{"serviceAccountName":"agent","scrapeConfigSelector":{"matchLabels":{"mih":"selected"}}}}`,
			`{"apiVersion":"monitoring.coreos.com/v1alpha1","kind":"ScrapeConfig","metadata":{"name":"selected","namespace":"monitoring"},"spec":{"kubernetesSDConfigs":[{"role":"Pod","namespaces":{"names":["monitoring"]}}],"relabelings":[{"action":"labelmap","regex":"__meta_kubernetes_pod_label_team","replacement":"team"},{"action":"labelmap","regex":"__meta_kubernetes_pod_label_(.+)","replacement":"team_$1"},{"action":"labelmap","regex":"__meta_kubernetes_pod_label_(team)","replacement":"$1"},{"action":"labelmap","regex":"__meta_kubernetes_pod_label_(mih_cluster_id)","replacement":"team_$1"},{"action":"labelmap","regex":"mih_cluster_id_(team)","replacement":"$1"},{"action":"labelmap","regex":"(team)_(mih_cluster_id)","replacement":"$1"},{"action":"labelmap","regex":"(team)_(mih_cluster_id)","replacement":"${1}_${2}"},{"action":"labelmap","regex":"(prefix_(mih_cluster_id))","replacement":"$1"},{"action":"labelmap","regex":"(prefix_(mih_cluster_id))","replacement":"team_$2"},{"action":"labelmap","regex":"(?i)(TEAM)","replacement":"$1"}]}}`,
			false,
		)

		By("通过公开预检入口扫描与保护标签无关的 labelmap")
		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring", "--agent-service-account", "agent",
			"--config-namespaces", "monitoring", "--discovery-namespaces", "monitoring",
			"--agent-manifest", agentManifest, "--collection-config-manifest", configManifest,
		)
		command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
		output, err := command.CombinedOutput()

		By("验证 labelmap 通过保护标签检查，预检只因 fixture 未提供集群 API 而失败")
		Expect(err).To(HaveOccurred(), string(output))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"ProtectedExternalLabelPreserved"`),
			ContainSubstring(`"reason":"APIGroupVersionNotServed"`),
			Not(ContainSubstring(`"reason":"ProtectedExternalLabelMutation"`)),
		))
	})
})

func writeCollectionValidationKubectl(fakeBin, agentJSON, configJSON string, rejectSecret bool, configMapExists ...bool) {
	GinkgoHelper()
	rejectSecretValue := "0"
	if rejectSecret {
		rejectSecretValue = "1"
	}
	configMapExistsValue := "0"
	if len(configMapExists) > 0 && configMapExists[0] {
		configMapExistsValue = "1"
	}
	script := `#!/usr/bin/env bash
set -eu
case "$*" in
  *"agent.yaml"*) printf '%s\n' "${PREFLIGHT_AGENT_JSON}" ;;
  *"config.yaml"*) printf '%s\n' "${PREFLIGHT_CONFIG_JSON}" ;;
  "version --output json") printf '%s\n' '{"serverVersion":{"major":"1","minor":"28","gitVersion":"v1.28.15"}}' ;;
  "get namespace "*" --output name") printf 'namespace/%s\n' "$3" ;;
  "--namespace monitoring get serviceaccount agent --output name") printf '%s\n' 'serviceaccount/agent' ;;
  "--namespace monitoring get prometheusagent agent --ignore-not-found --output json") ;;
  "--namespace monitoring get statefulset prom-agent-agent"*" --ignore-not-found --output json") ;;
  "--namespace monitoring get pod prom-agent-agent"*" --ignore-not-found --output json") ;;
  "--namespace monitoring get secret prom-agent-agent"*" --ignore-not-found --output json") ;;
  "--namespace monitoring get service prometheus-agent-operated --ignore-not-found --output json") ;;
  "get namespaces --output json")
    printf '%s\n' '{"items":[{"metadata":{"name":"monitoring","labels":{"scope":"agent"}}},{"metadata":{"name":"shared","labels":{"scope":"shared"}}},{"metadata":{"name":"prometheus-operator","labels":{}}}]}'
    ;;
  "--namespace prometheus-operator get deployment prometheus-operator --output json")
    jq --null-input --arg args "${PREFLIGHT_OPERATOR_ARGS:-}" '{spec:{template:{spec:{serviceAccountName:"prometheus-operator",containers:[{name:"prometheus-operator",args:(if $args == "" then [] else [$args] end)}]}}}}'
    ;;
  "--namespace monitoring get secret missing-credentials --output json")
    if [[ "${PREFLIGHT_REJECT_SECRET}" == 1 ]]; then
      printf '%s\n' 'secret not found' >&2
      exit 1
    fi
    printf '%s\n' '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"missing-credentials","namespace":"monitoring"},"data":{"username":"dXNlcg=="}}'
    ;;
  "--namespace monitoring get configmap missing-ca --output json")
    if [[ "${PREFLIGHT_CONFIGMAP_EXISTS}" == 1 ]]; then
      printf '%s\n' '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"missing-ca","namespace":"monitoring"},"data":{"other-key":"value"}}'
      exit 0
    fi
    printf '%s\n' 'configmap exists only in another namespace' >&2
    exit 1
    ;;
  *) printf 'unexpected kubectl call: %s\n' "$*" >&2; exit 1 ;;
esac
`
	writeExecutable(filepath.Join(fakeBin, "kubectl"), script)
	GinkgoT().Setenv("PREFLIGHT_AGENT_JSON", agentJSON)
	GinkgoT().Setenv("PREFLIGHT_CONFIG_JSON", configJSON)
	GinkgoT().Setenv("PREFLIGHT_REJECT_SECRET", rejectSecretValue)
	GinkgoT().Setenv("PREFLIGHT_CONFIGMAP_EXISTS", configMapExistsValue)
}

func readManifestObjects(path string) []map[string]any {
	GinkgoHelper()
	contents, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096)
	objects := []map[string]any{}
	for {
		object := map[string]any{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		Expect(err).NotTo(HaveOccurred())
		if len(object) > 0 {
			objects = append(objects, object)
		}
	}
	return objects
}
