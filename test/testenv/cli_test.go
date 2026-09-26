package testenv_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("测试依赖基线公共入口", func() {
	It("部署预检缺少必填输入时以退出码 2 返回 JSON 诊断", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		command := exec.Command(filepath.Join(root, "hack", "preflight.sh"))
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(2))
		expectJSONDiagnostics(output)
		diagnostic := map[string]any{}
		firstLine, _, _ := strings.Cut(string(output), "\n")
		Expect(json.Unmarshal([]byte(firstLine), &diagnostic)).To(Succeed(), string(output))
		Expect(diagnostic).To(SatisfyAll(
			HaveKeyWithValue("check", "input"),
			HaveKeyWithValue("status", "fail"),
			HaveKeyWithValue("reason", "InvalidInput"),
		))
	})

	It("部署预检以退出码 2 拒绝 default runtime ServiceAccount", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "default",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(2))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(SatisfyAll(
			ContainSubstring(`"reason":"InvalidInput"`),
			ContainSubstring("不得使用 default"),
		))
	})

	It("部署预检拒绝范围外的 server minor 且不创建探针", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		created := filepath.Join(fakeBin, "created")

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == *"version --output json"* ]]; then
  printf '%s\n' '{"serverVersion":{"major":"1","minor":"27","gitVersion":"v1.27.16"}}'
  exit 0
fi
if [[ "$*" == *" create "* ]]; then
  touch "${PREFLIGHT_CREATE_SENTINEL}"
fi
exit 1
`)

		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
		)
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"PREFLIGHT_CREATE_SENTINEL="+created,
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(ContainSubstring(`"check":"kubernetes-version"`))
		Expect(string(output)).To(ContainSubstring(`"reason":"UnsupportedKubernetesVersion"`))
		Expect(created).NotTo(BeAnExistingFile())
	})

	It("部署预检拒绝不存在的 Agent runtime ServiceAccount 且不创建探针", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		created := filepath.Join(fakeBin, "created")

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
case "$*" in
  *"version --output json"*)
    printf '%s\n' '{"serverVersion":{"major":"1","minor":"28+","gitVersion":"v1.28.15"}}'
    ;;
  *"get namespace monitoring"*)
    printf '%s\n' 'namespace/monitoring'
    ;;
  *"get serviceaccount agent"*)
    printf '%s\n' 'serviceaccount "agent" not found' >&2
    exit 1
    ;;
  *" create "*)
    touch "${PREFLIGHT_CREATE_SENTINEL}"
    exit 1
    ;;
  *) exit 1 ;;
esac
`)

		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
		)
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"PREFLIGHT_CREATE_SENTINEL="+created,
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(ContainSubstring(`"check":"runtime-service-account"`))
		Expect(string(output)).To(ContainSubstring(`"reason":"RuntimeServiceAccountNotFound"`))
		Expect(created).NotTo(BeAnExistingFile())
	})

	It("部署预检成功后只清理自己创建的 reconcile 探针", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		manifest := filepath.Join(fakeBin, "manifest.json")
		calls := filepath.Join(fakeBin, "calls")
		deleted := filepath.Join(fakeBin, "deleted")

		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >>"${PREFLIGHT_CALLS}"
case "$*" in
  "version --output json")
    printf '%s\n' '{"serverVersion":{"major":"1","minor":"19","gitVersion":"v1.19.16"}}'
    ;;
  "get namespace "*" --output name")
    printf '%s\n' "namespace/${3}"
    ;;
  "--namespace monitoring get serviceaccount agent --output name")
    printf '%s\n' 'serviceaccount/agent'
    ;;
  "get --raw /apis/monitoring.coreos.com/v1alpha1")
    printf '%s\n' '{"resources":[{"name":"prometheusagents"},{"name":"scrapeconfigs"}]}'
    ;;
  "get --raw /apis/monitoring.coreos.com/v1")
    printf '%s\n' '{"resources":[{"name":"servicemonitors"},{"name":"podmonitors"},{"name":"probes"}]}'
    ;;
  "get customresourcedefinition "*" --output json")
    version=v1
    case "$3" in prometheusagents.*|scrapeconfigs.*) version=v1alpha1 ;; esac
    schema=$(jq --null-input --arg version "${version}" '{spec:{versions:[{name:$version,served:true,schema:{openAPIV3Schema:{properties:{spec:{properties:{
      image:{},version:{},serviceAccountName:{},replicas:{},shards:{},
      storage:{properties:{emptyDir:{},volumeClaimTemplate:{properties:{spec:{properties:{accessModes:{},resources:{},storageClassName:{}}}}}}},
      walCompression:{},externalLabels:{},replicaExternalLabelName:{},
      serviceMonitorSelector:{properties:{matchLabels:{},matchExpressions:{}}},serviceMonitorNamespaceSelector:{properties:{matchLabels:{},matchExpressions:{}}},
      podMonitorSelector:{properties:{matchLabels:{},matchExpressions:{}}},podMonitorNamespaceSelector:{properties:{matchLabels:{},matchExpressions:{}}},
      probeSelector:{properties:{matchLabels:{},matchExpressions:{}}},probeNamespaceSelector:{properties:{matchLabels:{},matchExpressions:{}}},
      scrapeConfigSelector:{properties:{matchLabels:{},matchExpressions:{}}},scrapeConfigNamespaceSelector:{properties:{matchLabels:{},matchExpressions:{}}},
      staticConfigs:{items:{properties:{targets:{},labels:{}}}},
      kubernetesSDConfigs:{items:{properties:{role:{},namespaces:{properties:{names:{},ownNamespace:{}}},selectors:{items:{properties:{role:{},label:{},field:{}}}}}}},
      selector:{properties:{matchLabels:{},matchExpressions:{}}},namespaceSelector:{properties:{any:{},matchNames:{}}},
      endpoints:{items:{properties:{port:{},interval:{}}}},podMetricsEndpoints:{items:{properties:{port:{},interval:{}}}},
      module:{},prober:{properties:{url:{},path:{},scheme:{}}},
      targets:{properties:{staticConfig:{properties:{static:{},labels:{}}},ingress:{properties:{selector:{},namespaceSelector:{}}}}},
      remoteWrite:{items:{properties:{name:{},url:{},remoteTimeout:{},messageVersion:{},queueConfig:{properties:{capacity:{},minShards:{},
      maxShards:{},maxSamplesPerSend:{},batchSendDeadline:{},minBackoff:{},maxBackoff:{},retryOnRateLimit:{},sampleAgeLimit:{}}}}}}}}}}}}]}}')
    if [[ "${PREFLIGHT_SCHEMA_MISSING:-}" == 1 ]]; then
      jq 'del(.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.prober.properties.url)' <<<"${schema}"
    else
      printf '%s\n' "${schema}"
    fi
    ;;
  "--namespace prometheus-operator get deployment prometheus-operator --output json")
    if [[ "${PREFLIGHT_WATCH_RESTRICTED:-}" == 1 ]]; then
      printf '%s\n' '{"spec":{"template":{"spec":{"serviceAccountName":"prometheus-operator","containers":[{"name":"prometheus-operator","args":["--namespaces=other"]}]}}}}'
    else
      printf '%s\n' '{"spec":{"template":{"spec":{"serviceAccountName":"prometheus-operator","containers":[{"name":"prometheus-operator","args":[]}]}}}}'
    fi
    ;;
  "auth can-i "*)
    if [[ "${PREFLIGHT_RBAC_DENIED:-}" == 1 && "$*" == *"watch endpointslices.discovery.k8s.io"* ]]; then
      printf '%s\n' no
      exit 1
    fi
    printf '%s\n' yes
    ;;
  "create --filename - --output json")
    tee "${PREFLIGHT_MANIFEST}" >/dev/null
    name=$(jq --raw-output '.metadata.name' "${PREFLIGHT_MANIFEST}")
    if [[ "${PREFLIGHT_CREATE_RESPONSE_LOST:-}" == 1 ]]; then
      printf '%s\n' 'connection closed after server accepted request' >&2
      exit 1
    elif [[ "${PREFLIGHT_CREATE_MALFORMED:-}" == 1 ]]; then
      jq --null-input --arg name "${name}" '{metadata:{name:$name}}'
    else
      jq --null-input --arg name "${name}" '{metadata:{name:$name,uid:"probe-uid"}}'
    fi
    ;;
  "--namespace monitoring get prometheusagent "*" --ignore-not-found --output json")
    if [[ -e "${PREFLIGHT_DELETED}" ]]; then
      exit 0
    fi
    if [[ "${PREFLIGHT_CREATE_RESPONSE_LOST:-}" == 1 ]]; then
      recovery_reads="${PREFLIGHT_CALLS}.recovery-reads"
      reads=0
      if [[ -e "${recovery_reads}" ]]; then reads=$(cat "${recovery_reads}"); fi
      reads=$((reads + 1))
      printf '%s\n' "${reads}" >"${recovery_reads}"
      if [[ "${PREFLIGHT_CREATE_RECOVERY_READ_FAIL:-}" == 1 ]]; then
        printf '%s\n' 'temporary API read failure' >&2
        exit 1
      fi
      if [[ "${PREFLIGHT_CREATE_NEVER_VISIBLE:-}" == 1 || "${reads}" -le "${PREFLIGHT_CREATE_VISIBILITY_DELAY:-0}" ]]; then
        exit 0
      fi
    fi
    name=$(jq --raw-output '.metadata.name' "${PREFLIGHT_MANIFEST}")
    token=$(jq --raw-output '.metadata.annotations["monitoring.fjsxy.github.io/preflight-token"]' "${PREFLIGHT_MANIFEST}")
    jq --null-input --arg name "${name}" --arg token "${token}" '{metadata:{name:$name,uid:"probe-uid",generation:1,annotations:{"monitoring.fjsxy.github.io/preflight-token":$token}},status:{conditions:[]}}'
    ;;
  "--namespace monitoring get prometheusagent "*" --output json")
    name=$(jq --raw-output '.metadata.name' "${PREFLIGHT_MANIFEST}")
    token=$(jq --raw-output '.metadata.annotations["monitoring.fjsxy.github.io/preflight-token"]' "${PREFLIGHT_MANIFEST}")
    if [[ "${PREFLIGHT_RECONCILE_FAIL:-}" == 1 ]]; then
      jq --null-input --arg name "${name}" --arg token "${token}" '{metadata:{name:$name,uid:"probe-uid",generation:1,annotations:{"monitoring.fjsxy.github.io/preflight-token":$token}},status:{conditions:[]}}'
    else
      jq --null-input --arg name "${name}" --arg token "${token}" '{metadata:{name:$name,uid:"probe-uid",generation:1,annotations:{"monitoring.fjsxy.github.io/preflight-token":$token}},status:{conditions:[{type:"Reconciled",status:"True",observedGeneration:1}]}}'
    fi
    ;;
  "--namespace monitoring get statefulset prom-agent-mih-preflight-"*" --output json")
    name=$(jq --raw-output '.metadata.name' "${PREFLIGHT_MANIFEST}")
    jq --null-input --arg name "prom-agent-${name}" '{metadata:{name:$name,ownerReferences:[{apiVersion:"monitoring.coreos.com/v1alpha1",kind:"PrometheusAgent",uid:"probe-uid"}]}}'
    ;;
  "delete --raw /apis/monitoring.coreos.com/v1alpha1/namespaces/monitoring/prometheusagents/mih-preflight-"*" --filename -")
    jq --exit-status '.preconditions.uid == "probe-uid" and .propagationPolicy == "Foreground"' >/dev/null
    touch "${PREFLIGHT_DELETED}"
    printf '%s\n' '{"kind":"PrometheusAgent","metadata":{"deletionTimestamp":"2026-09-26T00:00:00Z"}}'
    ;;
  *)
    printf 'unexpected kubectl call: %s\n' "$*" >&2
    exit 1
    ;;
esac
`)

		command := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--timeout-seconds", "5",
		)
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"PREFLIGHT_MANIFEST="+manifest,
			"PREFLIGHT_CALLS="+calls,
			"PREFLIGHT_DELETED="+deleted,
		)
		output, err := command.CombinedOutput()

		Expect(err).NotTo(HaveOccurred(), string(output))
		expectJSONDiagnostics(output)
		Expect(string(output)).To(ContainSubstring(`"reason":"PreflightPassed"`))
		manifestContents, err := os.ReadFile(manifest)
		Expect(err).NotTo(HaveOccurred())
		probe := map[string]any{}
		Expect(json.Unmarshal(manifestContents, &probe)).To(Succeed())
		Expect(probe).To(HaveKeyWithValue("spec", SatisfyAll(
			HaveKeyWithValue("serviceAccountName", "agent"),
			HaveKeyWithValue("replicas", float64(0)),
		)))
		probeMetadata := probe["metadata"].(map[string]any)
		probeName := probeMetadata["name"].(string)
		Expect(probeName).To(HavePrefix("mih-preflight-"))
		callContents, err := os.ReadFile(calls)
		Expect(err).NotTo(HaveOccurred())
		deleteCalls := []string{}
		for _, call := range strings.Split(string(callContents), "\n") {
			if strings.HasPrefix(call, "delete --raw ") {
				deleteCalls = append(deleteCalls, call)
			}
		}
		Expect(deleteCalls).To(Equal([]string{
			"delete --raw /apis/monitoring.coreos.com/v1alpha1/namespaces/monitoring/prometheusagents/" + probeName + " --filename -",
		}))

		failedCalls := filepath.Join(fakeBin, "failed-calls")
		failedDeleted := filepath.Join(fakeBin, "failed-deleted")
		failedCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--timeout-seconds", "1",
		)
		failedCommand.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"PREFLIGHT_MANIFEST="+manifest,
			"PREFLIGHT_CALLS="+failedCalls,
			"PREFLIGHT_DELETED="+failedDeleted,
			"PREFLIGHT_RECONCILE_FAIL=1",
		)
		failedOutput, err := failedCommand.CombinedOutput()
		Expect(err).To(HaveOccurred())
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(failedOutput)
		Expect(string(failedOutput)).To(SatisfyAll(
			ContainSubstring(`"reason":"MinimumReconcileTimedOut"`),
			ContainSubstring(`"reason":"ProbeCleaned"`),
		))
		failedCallContents, err := os.ReadFile(failedCalls)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(failedCallContents)).To(ContainSubstring("delete --raw /apis/monitoring.coreos.com/v1alpha1/namespaces/monitoring/prometheusagents/mih-preflight-"))

		malformedCalls := filepath.Join(fakeBin, "malformed-calls")
		malformedCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--timeout-seconds", "1",
		)
		malformedCommand.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"PREFLIGHT_MANIFEST="+manifest,
			"PREFLIGHT_CALLS="+malformedCalls,
			"PREFLIGHT_DELETED="+filepath.Join(fakeBin, "malformed-deleted"),
			"PREFLIGHT_CREATE_MALFORMED=1",
		)
		malformedOutput, err := malformedCommand.CombinedOutput()
		Expect(err).To(HaveOccurred())
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(malformedOutput)
		Expect(string(malformedOutput)).To(SatisfyAll(
			ContainSubstring(`"reason":"InvalidProbeCreateResponse"`),
			ContainSubstring(`"reason":"ProbeCleaned"`),
		))
		malformedCallContents, err := os.ReadFile(malformedCalls)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(malformedCallContents)).To(ContainSubstring("delete --raw /apis/monitoring.coreos.com/v1alpha1/namespaces/monitoring/prometheusagents/mih-preflight-"))

		lostResponseCalls := filepath.Join(fakeBin, "lost-response-calls")
		lostResponseCommand := exec.Command(
			filepath.Join(root, "hack", "preflight.sh"),
			"--agent-namespace", "monitoring",
			"--agent-service-account", "agent",
			"--config-namespaces", "monitoring",
			"--discovery-namespaces", "monitoring",
			"--timeout-seconds", "1",
		)
		lostResponseCommand.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"PREFLIGHT_MANIFEST="+manifest,
			"PREFLIGHT_CALLS="+lostResponseCalls,
			"PREFLIGHT_DELETED="+filepath.Join(fakeBin, "lost-response-deleted"),
			"PREFLIGHT_CREATE_RESPONSE_LOST=1",
			"PREFLIGHT_CREATE_VISIBILITY_DELAY=1",
		)
		lostResponseOutput, err := lostResponseCommand.CombinedOutput()
		Expect(err).To(HaveOccurred())
		Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
		expectJSONDiagnostics(lostResponseOutput)
		Expect(string(lostResponseOutput)).To(SatisfyAll(
			ContainSubstring(`"reason":"ProbeCreateFailed"`),
			ContainSubstring(`"reason":"ProbeCleaned"`),
		))
		lostResponseCallContents, err := os.ReadFile(lostResponseCalls)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(lostResponseCallContents)).To(ContainSubstring("delete --raw /apis/monitoring.coreos.com/v1alpha1/namespaces/monitoring/prometheusagents/mih-preflight-"))
		lostResponseRecoveryReads, err := os.ReadFile(lostResponseCalls + ".recovery-reads")
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(lostResponseRecoveryReads))).To(Equal("3"))

		for _, tc := range []struct {
			name   string
			env    string
			reason string
		}{
			{name: "accepted create remains absent", env: "PREFLIGHT_CREATE_NEVER_VISIBLE", reason: "ProbeCreateOutcomeUnknown"},
			{name: "recovery reads fail", env: "PREFLIGHT_CREATE_RECOVERY_READ_FAIL", reason: "ProbeCreateOutcomeUnknown"},
		} {
			By(tc.name)
			recoveryCalls := filepath.Join(fakeBin, strings.ReplaceAll(tc.name, " ", "-")+"-calls")
			recoveryCommand := exec.Command(
				filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring",
				"--agent-service-account", "agent",
				"--config-namespaces", "monitoring",
				"--discovery-namespaces", "monitoring",
				"--timeout-seconds", "1",
			)
			recoveryCommand.Env = append(os.Environ(),
				"PATH="+fakeBin+":"+os.Getenv("PATH"),
				"PREFLIGHT_MANIFEST="+manifest,
				"PREFLIGHT_CALLS="+recoveryCalls,
				"PREFLIGHT_DELETED="+filepath.Join(fakeBin, strings.ReplaceAll(tc.name, " ", "-")+"-deleted"),
				"PREFLIGHT_CREATE_RESPONSE_LOST=1",
				tc.env+"=1",
			)
			recoveryOutput, err := recoveryCommand.CombinedOutput()
			Expect(err).To(HaveOccurred())
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(recoveryOutput)
			Expect(string(recoveryOutput)).To(SatisfyAll(
				ContainSubstring(`"reason":"ProbeCreateFailed"`),
				ContainSubstring(`"reason":"`+tc.reason+`"`),
			))
			recoveryCallContents, err := os.ReadFile(recoveryCalls)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(recoveryCallContents)).NotTo(ContainSubstring("delete --raw "))
		}

		negativeCases := []struct {
			name   string
			env    string
			reason string
		}{
			{name: "missing nested schema", env: "PREFLIGHT_SCHEMA_MISSING", reason: "RequiredSchemaFieldMissing"},
			{name: "restricted watch scope", env: "PREFLIGHT_WATCH_RESTRICTED", reason: "WatchScopeRestricted"},
			{name: "denied runtime RBAC", env: "PREFLIGHT_RBAC_DENIED", reason: "RBACDenied"},
		}
		for _, tc := range negativeCases {
			By(tc.name)
			negativeCalls := filepath.Join(fakeBin, strings.ReplaceAll(tc.name, " ", "-")+"-calls")
			negativeCommand := exec.Command(
				filepath.Join(root, "hack", "preflight.sh"),
				"--agent-namespace", "monitoring",
				"--agent-service-account", "agent",
				"--config-namespaces", "monitoring",
				"--discovery-namespaces", "monitoring",
				"--timeout-seconds", "1",
			)
			negativeCommand.Env = append(os.Environ(),
				"PATH="+fakeBin+":"+os.Getenv("PATH"),
				"PREFLIGHT_MANIFEST="+manifest,
				"PREFLIGHT_CALLS="+negativeCalls,
				"PREFLIGHT_DELETED="+filepath.Join(fakeBin, strings.ReplaceAll(tc.name, " ", "-")+"-deleted"),
				tc.env+"=1",
			)
			negativeOutput, err := negativeCommand.CombinedOutput()
			Expect(err).To(HaveOccurred(), string(negativeOutput))
			Expect(err.(*exec.ExitError).ExitCode()).To(Equal(1))
			expectJSONDiagnostics(negativeOutput)
			Expect(string(negativeOutput)).To(ContainSubstring(`"reason":"` + tc.reason + `"`))
			negativeCallContents, err := os.ReadFile(negativeCalls)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(negativeCallContents)).NotTo(ContainSubstring("create --filename"))
		}
	})

	It("拒绝一期范围外的 Kubernetes minor", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		command := exec.Command(
			"make", "-s", "-C", root, "test-env-up",
			"KUBERNETES_VERSION=1.27",
			"TEST_RUN_ID=cli-contract",
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("KUBERNETES_VERSION 必须是 1.19 或 1.28"))
	})

	It("拒绝缺失的 test-run 身份", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())

		command := exec.Command(
			"make", "-s", "-C", root, "test-env-up",
			"KUBERNETES_VERSION=1.28",
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("TEST_RUN_ID 必须是非空的 DNS label"))
	})

	It("身份标记不匹配时拒绝删除集群", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		deleted := filepath.Join(fakeBin, "deleted")

		writeExecutable(filepath.Join(fakeBin, "kind"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == "get clusters" ]]; then
  echo "mih-e2e-128-cli-contract"
  exit 0
fi
if [[ "${1:-}" == "delete" ]]; then
  touch "${TESTENV_DELETE_SENTINEL}"
  exit 0
fi
exit 1
`)
		writeExecutable(filepath.Join(fakeBin, "kubectl"), `#!/usr/bin/env bash
set -eu
printf 'another-run'
`)

		command := exec.Command(
			"make", "-s", "-C", root, "test-env-down",
			"KUBERNETES_VERSION=1.28",
			"TEST_RUN_ID=cli-contract",
		)
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"TESTENV_DELETE_SENTINEL="+deleted,
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("test-run 身份不匹配，拒绝清理"))
		Expect(deleted).NotTo(BeAnExistingFile())
	})

	It("拒绝复用同名 kind 集群", func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		Expect(err).NotTo(HaveOccurred())
		fakeBin := GinkgoT().TempDir()
		created := filepath.Join(fakeBin, "created")

		writeExecutable(filepath.Join(fakeBin, "kind"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == "get clusters" ]]; then
  echo "mih-e2e-119-cli-contract"
  exit 0
fi
if [[ "${1:-}" == "create" ]]; then
  touch "${TESTENV_CREATE_SENTINEL}"
  exit 0
fi
exit 1
`)

		command := exec.Command(
			"make", "-s", "-C", root, "test-env-up",
			"KUBERNETES_VERSION=1.19",
			"TEST_RUN_ID=cli-contract",
		)
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+":"+os.Getenv("PATH"),
			"TESTENV_CREATE_SENTINEL="+created,
		)
		output, err := command.CombinedOutput()

		Expect(err).To(HaveOccurred())
		Expect(string(output)).To(ContainSubstring("拒绝复用已有 kind 集群"))
		Expect(created).NotTo(BeAnExistingFile())
	})
})

func writeExecutable(path, contents string) {
	Expect(os.WriteFile(path, []byte(contents), 0o755)).To(Succeed())
}

func expectJSONDiagnostics(output []byte) []map[string]any {
	GinkgoHelper()
	diagnostics := []map[string]any{}
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		diagnostic := map[string]any{}
		Expect(json.Unmarshal([]byte(line), &diagnostic)).To(Succeed(), line)
		Expect(diagnostic).To(SatisfyAll(
			HaveKey("level"),
			HaveKey("check"),
			HaveKey("status"),
			HaveKey("reason"),
			HaveKey("message"),
			HaveKey("details"),
		))
		diagnostics = append(diagnostics, diagnostic)
	}
	Expect(diagnostics).NotTo(BeEmpty(), string(output))
	return diagnostics
}
