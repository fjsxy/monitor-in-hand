#!/usr/bin/env bash

set -euo pipefail

action=${1:-}
kubernetes_version=${KUBERNETES_VERSION:-}
test_run_id=${TEST_RUN_ID:-}
repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
fixture_root="${repo_root}/test/fixtures/testenv"
# shellcheck disable=SC1091
source "${fixture_root}/versions.env"
kind_bin=${KIND_BIN:-kind}
kubectl_bin=${KUBECTL_BIN:-kubectl}
cluster_name="mih-e2e-${kubernetes_version//./}-${test_run_id}"
context="kind-${cluster_name}"
identity_config_map=monitor-in-hand-test-run
operator_namespace=prometheus-operator
test_namespace=monitor-in-hand-test
wait_timeout=${TEST_ENV_WAIT_TIMEOUT:-600s}

case "${kubernetes_version}" in
  1.19 | 1.28) ;;
  *)
    echo "KUBERNETES_VERSION 必须是 1.19 或 1.28" >&2
    exit 2
    ;;
esac

if [[ ! "${test_run_id}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || (( ${#test_run_id} > 32 )); then
  echo "TEST_RUN_ID 必须是非空的 DNS label，且长度不超过 32" >&2
  exit 2
fi

delete_environment() {
  if ! "${kind_bin}" get clusters | grep -Fxq "${cluster_name}"; then
    echo "测试集群不存在，无需清理：${cluster_name}"
    return
  fi

  local actual_identity
  if ! actual_identity=$("${kubectl_bin}" --context "${context}" --namespace kube-system \
    get configmap "${identity_config_map}" \
    --output 'jsonpath={.data.testRunID}{"|"}{.data.kubernetesVersion}'); then
    echo "无法读取 test-run 身份，拒绝清理：${cluster_name}" >&2
    exit 1
  fi
  if [[ "${actual_identity}" != "${test_run_id}|${kubernetes_version}" ]]; then
    echo "test-run 身份不匹配，拒绝清理：期望 ${test_run_id}|${kubernetes_version}，实际 ${actual_identity}" >&2
    exit 1
  fi

  "${kind_bin}" delete cluster --name "${cluster_name}"
}

read_environment_identity() {
  "${kubectl_bin}" --context "${context}" --namespace kube-system \
    get configmap "${identity_config_map}" \
    --output 'jsonpath={.data.testRunID}{"|"}{.data.kubernetesVersion}'
}

require_tool() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "缺少必需命令：$1" >&2
    exit 1
  fi
}

node_image_for_version() {
  case "${kubernetes_version}" in
    1.19) printf '%s\n' "${KIND_NODE_IMAGE_119}" ;;
    1.28) printf '%s\n' "${KIND_NODE_IMAGE_128}" ;;
  esac
}

install_prometheus_operator() {
  local bundle_dir bundle_path rendered_path actual_checksum
  bundle_dir=$(mktemp -d "${TMPDIR:-/tmp}/mih-testenv-bundle.XXXXXX")
  bundle_path="${bundle_dir}/bundle.yaml"
  rendered_path="${bundle_dir}/rendered.yaml"

  cp "${fixture_root}/prometheus-operator/kustomization.yaml" "${bundle_dir}/kustomization.yaml"
  curl --fail --location --silent --show-error --retry 3 \
    "${PROMETHEUS_OPERATOR_BUNDLE_URL}" --output "${bundle_path}.download"
  actual_checksum=$(shasum -a 256 "${bundle_path}.download" | awk '{print $1}')
  if [[ "${actual_checksum}" != "${PROMETHEUS_OPERATOR_BUNDLE_SHA256}" ]]; then
    echo "Prometheus Operator bundle checksum 不匹配：${actual_checksum}" >&2
    rm -r "${bundle_dir}"
    exit 1
  fi

  sed \
    -e "s|quay.io/prometheus-operator/prometheus-operator:v0.83.0|${PROMETHEUS_OPERATOR_IMAGE}|g" \
    -e "s|quay.io/prometheus-operator/prometheus-config-reloader:v0.83.0|${CONFIG_RELOADER_IMAGE}|g" \
    "${bundle_path}.download" >"${bundle_path}"
  "${kubectl_bin}" kustomize "${bundle_dir}" >"${rendered_path}"

  if grep -Fq 'quay.io/prometheus-operator/prometheus-operator:v0.83.0' "${rendered_path}" \
    || grep -Fq 'quay.io/prometheus-operator/prometheus-config-reloader:v0.83.0' "${rendered_path}"; then
    echo "Prometheus Operator render 仍包含未固定 digest 的镜像" >&2
    rm -r "${bundle_dir}"
    exit 1
  fi

  "${kubectl_bin}" --context "${context}" create namespace "${operator_namespace}"
  "${kubectl_bin}" --context "${context}" create --filename "${rendered_path}"
  wait_for_rollout "${operator_namespace}" deployment/prometheus-operator
  rm -r "${bundle_dir}"
}

wait_for_rollout() {
  local namespace=$1
  local resource=$2
  local rollout_pid image_failure

  "${kubectl_bin}" --context "${context}" --namespace "${namespace}" \
    rollout status "${resource}" --timeout "${wait_timeout}" &
  rollout_pid=$!
  while kill -0 "${rollout_pid}" >/dev/null 2>&1; do
    image_failure=$("${kubectl_bin}" --context "${context}" --namespace "${namespace}" \
      get pods --output json 2>/dev/null \
      | jq --raw-output '
          [
            .items[] as $pod |
            (($pod.status.initContainerStatuses // []) + ($pod.status.containerStatuses // []))[] |
            select(.state.waiting.reason == "ErrImagePull" or
                   .state.waiting.reason == "ImagePullBackOff" or
                   .state.waiting.reason == "InvalidImageName") |
            ($pod.metadata.name + "/" + .name + ":" + .state.waiting.reason)
          ] | unique | join(", ")
        ' || true)
    if [[ -n "${image_failure}" ]]; then
      kill "${rollout_pid}" >/dev/null 2>&1 || true
      wait "${rollout_pid}" 2>/dev/null || true
      echo "镜像准备失败，停止等待 ${resource}：${image_failure}" >&2
      return 1
    fi
    sleep 2
  done
  wait "${rollout_pid}"
}

wait_for_test_workloads() {
  local deployment
  for deployment in metrics-target blackbox-exporter vminsert vmselect; do
    wait_for_rollout "${test_namespace}" "deployment/${deployment}"
  done
  wait_for_rollout "${test_namespace}" statefulset/vmstorage
  wait_for_rollout "${test_namespace}" statefulset/prom-agent-fixture
}

install_test_workloads() {
  local rendered service_discovery_role
  case "${kubernetes_version}" in
    1.19) service_discovery_role=Endpoints ;;
    1.28) service_discovery_role=EndpointSlice ;;
  esac
  rendered=$(sed \
    -e "s|__NODE_EXPORTER_IMAGE__|${NODE_EXPORTER_IMAGE}|g" \
    -e "s|__BLACKBOX_EXPORTER_IMAGE__|${BLACKBOX_EXPORTER_IMAGE}|g" \
    -e "s|__VMSTORAGE_IMAGE__|${VMSTORAGE_IMAGE}|g" \
    -e "s|__VMINSERT_IMAGE__|${VMINSERT_IMAGE}|g" \
    -e "s|__VMSELECT_IMAGE__|${VMSELECT_IMAGE}|g" \
    -e "s|__PROMETHEUS_IMAGE__|${PROMETHEUS_IMAGE}|g" \
    -e "s|__SERVICE_DISCOVERY_ROLE__|${service_discovery_role}|g" \
    "${fixture_root}/environment.yaml")
  if grep -Eq '__[A-Z0-9_]+__' <<<"${rendered}"; then
    echo "测试依赖基线 manifest 存在未替换的模板 token" >&2
    exit 1
  fi
  printf '%s\n' "${rendered}" \
    | "${kubectl_bin}" --context "${context}" create --filename -
  "${kubectl_bin}" --context "${context}" label namespace "${test_namespace}" \
    "monitoring.fjsxy.github.io/test-run=${test_run_id}" --overwrite
  wait_for_test_workloads
}

create_environment() {
  if "${kind_bin}" get clusters | grep -Fxq "${cluster_name}"; then
    echo "拒绝复用已有 kind 集群：${cluster_name}" >&2
    exit 1
  fi

  require_tool "${kind_bin}"
  require_tool "${kubectl_bin}"
  require_tool docker
  require_tool curl
  require_tool shasum
  require_tool jq
  docker info >/dev/null

  "${kind_bin}" create cluster \
    --name "${cluster_name}" \
    --image "$(node_image_for_version)" \
    --wait "${wait_timeout}"

  "${kubectl_bin}" --context "${context}" --namespace kube-system create configmap "${identity_config_map}" \
    --from-literal "testRunID=${test_run_id}" \
    --from-literal "kubernetesVersion=${kubernetes_version}"
  "${kubectl_bin}" --context "${context}" --namespace kube-system label configmap "${identity_config_map}" \
    app.kubernetes.io/managed-by=monitor-in-hand-test-fixture

  install_prometheus_operator
  install_test_workloads
  echo "测试环境已创建：context=${context}，Kubernetes=${kubernetes_version}，test-run=${test_run_id}"
}

verify_crd_served() {
  local crd=$1
  local version=$2
  "${kubectl_bin}" --context "${context}" get crd "${crd}" --output json \
    | jq --exit-status --arg version "${version}" \
      '.spec.versions | any(.name == $version and .served == true)' >/dev/null
}

verify_crd_schema_fields() {
  local crd=$1
  local version=$2
  local fields=$3
  "${kubectl_bin}" --context "${context}" get crd "${crd}" --output json \
    | jq --exit-status --arg version "${version}" --argjson fields "${fields}" '
        (.spec.versions[] | select(.name == $version and .served == true) |
          .schema.openAPIV3Schema.properties.spec.properties) as $spec |
        $fields | all(.[]; . as $field | $spec | has($field))
      ' >/dev/null
}

verify_endpoint_addresses() {
  local service=$1
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    get endpoints "${service}" --output json \
    | jq --exit-status '[.subsets[]?.addresses[]?] | length > 0' >/dev/null
}

verify_workload_image() {
  local kind=$1
  local name=$2
  local expected_image=$3
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    get "${kind}/${name}" --output json \
    | jq --exit-status --arg expected "${expected_image}" \
      '.spec.template.spec.containers | any(.image == $expected)' >/dev/null
}

verify_environment() {
  local expected_patch actual_version actual_identity docker_arch
  case "${kubernetes_version}" in
    1.19) expected_patch=v1.19.16 ;;
    1.28) expected_patch=v1.28.15 ;;
  esac

  actual_identity=$(read_environment_identity)
  if [[ "${actual_identity}" != "${test_run_id}|${kubernetes_version}" ]]; then
    echo "test-run 身份不匹配：期望 ${test_run_id}|${kubernetes_version}，实际 ${actual_identity}" >&2
    exit 1
  fi

  actual_version=$("${kubectl_bin}" --context "${context}" version --output json 2>/dev/null \
    | jq --raw-output '.serverVersion.gitVersion')
  if [[ "${actual_version}" != "${expected_patch}" ]]; then
    echo "Kubernetes patch 不匹配：期望 ${expected_patch}，实际 ${actual_version}" >&2
    exit 1
  fi
  "${kubectl_bin}" --context "${context}" get --raw /readyz | grep -Fxq ok

  docker_arch=$(docker info --format '{{.Architecture}}')
  case "${docker_arch}" in
    aarch64) docker_arch=arm64 ;;
    x86_64) docker_arch=amd64 ;;
  esac
  "${kubectl_bin}" --context "${context}" get nodes --output json \
    | jq --exit-status --arg architecture "${docker_arch}" '
        (.items | length > 0) and
        all(.items[];
          .status.nodeInfo.operatingSystem == "linux" and
          .status.nodeInfo.architecture == $architecture and
          any(.status.conditions[]; .type == "Ready" and .status == "True")
        )
      ' >/dev/null

  "${kubectl_bin}" --context "${context}" --namespace "${operator_namespace}" \
    get deployment prometheus-operator --output json \
    | jq --exit-status \
      --arg operator_image "${PROMETHEUS_OPERATOR_IMAGE}" \
      --arg reloader_arg "--prometheus-config-reloader=${CONFIG_RELOADER_IMAGE}" '
        .spec.template.spec.containers[0] as $container |
        $container.image == $operator_image and
        ($container.args | index($reloader_arg) != null) and
        ([$container.args[] | select(startswith("--namespaces=") or startswith("--deny-namespaces="))] | length == 0)
      ' >/dev/null

  verify_crd_served prometheusagents.monitoring.coreos.com v1alpha1
  verify_crd_served scrapeconfigs.monitoring.coreos.com v1alpha1
  verify_crd_served servicemonitors.monitoring.coreos.com v1
  verify_crd_served podmonitors.monitoring.coreos.com v1
  verify_crd_served probes.monitoring.coreos.com v1

  verify_crd_schema_fields prometheusagents.monitoring.coreos.com v1alpha1 \
    '["image","version","serviceAccountName","replicas","shards","storage","walCompression","externalLabels","replicaExternalLabelName","serviceDiscoveryRole","serviceMonitorSelector","serviceMonitorNamespaceSelector","podMonitorSelector","podMonitorNamespaceSelector","probeSelector","probeNamespaceSelector","scrapeConfigSelector","scrapeConfigNamespaceSelector","remoteWrite"]'
  verify_crd_schema_fields scrapeconfigs.monitoring.coreos.com v1alpha1 \
    '["staticConfigs","kubernetesSDConfigs"]'
  verify_crd_schema_fields servicemonitors.monitoring.coreos.com v1 \
    '["selector","namespaceSelector","endpoints"]'
  verify_crd_schema_fields podmonitors.monitoring.coreos.com v1 \
    '["selector","namespaceSelector","podMetricsEndpoints"]'
  verify_crd_schema_fields probes.monitoring.coreos.com v1 \
    '["module","prober","targets"]'

  "${kubectl_bin}" --context "${context}" get crd prometheusagents.monitoring.coreos.com --output json \
    | jq --exit-status '
        (.spec.versions[] | select(.name == "v1alpha1") |
          .schema.openAPIV3Schema.properties.spec.properties.remoteWrite.items.properties) as $remote |
        ($remote | has("name") and has("url") and
          has("remoteTimeout") and has("messageVersion") and has("queueConfig")) and
        ($remote.queueConfig.properties as $queue |
          ["capacity", "minShards", "maxShards", "maxSamplesPerSend",
           "batchSendDeadline", "minBackoff", "maxBackoff",
           "retryOnRateLimit", "sampleAgeLimit"] |
          all(.[]; . as $field | $queue | has($field)))
      ' >/dev/null

  local operator_sa agent_sa resource service verb
  operator_sa=system:serviceaccount:${operator_namespace}:prometheus-operator
  agent_sa=system:serviceaccount:${test_namespace}:prometheus-agent
  for verb in get list watch; do
    "${kubectl_bin}" --context "${context}" auth can-i "${verb}" prometheusagents.monitoring.coreos.com \
      --all-namespaces --as "${operator_sa}" | grep -Fxq yes
  done
  "${kubectl_bin}" --context "${context}" auth can-i create statefulsets.apps \
    --all-namespaces --as "${operator_sa}" | grep -Fxq yes
  for resource in services endpoints pods ingresses.networking.k8s.io endpointslices.discovery.k8s.io; do
    for verb in get list watch; do
      "${kubectl_bin}" --context "${context}" auth can-i "${verb}" "${resource}" \
        --all-namespaces --as "${agent_sa}" | grep -Fxq yes
    done
  done
  "${kubectl_bin}" --context "${context}" auth can-i get /metrics \
    --as "${agent_sa}" | grep -Fxq yes

  wait_for_test_workloads >/dev/null
  verify_workload_image deployment metrics-target "${NODE_EXPORTER_IMAGE}"
  verify_workload_image deployment blackbox-exporter "${BLACKBOX_EXPORTER_IMAGE}"
  verify_workload_image deployment vminsert "${VMINSERT_IMAGE}"
  verify_workload_image deployment vmselect "${VMSELECT_IMAGE}"
  verify_workload_image statefulset vmstorage "${VMSTORAGE_IMAGE}"

  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    get statefulset prom-agent-fixture --output json \
    | jq --exit-status --arg prometheus "${PROMETHEUS_IMAGE}" --arg reloader "${CONFIG_RELOADER_IMAGE}" '
        .status.readyReplicas == 1 and
        ([.spec.template.spec.containers[], .spec.template.spec.initContainers[]] |
          any(.name == "prometheus" and .image == $prometheus) and
          all(.[]; if (.name | contains("config-reloader")) then .image == $reloader else true end))
      ' >/dev/null
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    get prometheusagent fixture --output json \
    | jq --exit-status '
        .metadata.generation as $generation |
        (.status.conditions | any(.type == "Reconciled" and .status == "True" and .observedGeneration == $generation)) and
        (.status.conditions | any(.type == "Available" and .status == "True" and .observedGeneration == $generation))
      ' >/dev/null

  for service in metrics-target blackbox-exporter vmstorage vminsert vmselect; do
    verify_endpoint_addresses "${service}"
  done
  for resource in servicemonitor podmonitor probe scrapeconfig; do
    "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" get "${resource}/fixture" >/dev/null
  done

  echo "测试环境验收通过：context=${context}，Kubernetes=${actual_version}，architecture=${docker_arch}，test-run=${test_run_id}"
}

capture_evidence() {
  local evidence_dir deployment pod
  evidence_dir=${TEST_ENV_EVIDENCE_DIR:-"${repo_root}/artifacts/test-env/${test_run_id}-${kubernetes_version}"}
  mkdir -p "${evidence_dir}"
  printf 'context=%s\nkubernetesVersion=%s\ntestRunID=%s\n' \
    "${context}" "${kubernetes_version}" "${test_run_id}" >"${evidence_dir}/fixture.txt"

  "${kubectl_bin}" --context "${context}" version --output yaml >"${evidence_dir}/version.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" get nodes --output yaml >"${evidence_dir}/nodes.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" get pods --all-namespaces --output wide >"${evidence_dir}/pods.txt" 2>&1 || true
  "${kubectl_bin}" --context "${context}" get events --all-namespaces --sort-by=.lastTimestamp >"${evidence_dir}/events.txt" 2>&1 || true
  "${kubectl_bin}" --context "${context}" get crd --output yaml >"${evidence_dir}/crds.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" get all --output yaml >"${evidence_dir}/test-resources.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    get prometheusagent,servicemonitor,podmonitor,probe,scrapeconfig --output yaml \
    >"${evidence_dir}/monitoring-resources.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    get secret prom-agent-fixture --output yaml \
    >"${evidence_dir}/prom-agent-fixture-secret.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    get secret prom-agent-fixture --output 'jsonpath={.data.prometheus\.yaml\.gz}' 2>/dev/null \
    | base64 --decode | gzip --decompress --stdout \
    >"${evidence_dir}/prometheus-generated.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" get kubestatemetrics --output yaml \
    >"${evidence_dir}/kubestatemetrics.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" --namespace monitor-in-hand-system get all,lease --output yaml \
    >"${evidence_dir}/monitor-in-hand-system.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" get clusterrole,clusterrolebinding \
    --selector app.kubernetes.io/managed-by=monitor-in-hand --output yaml \
    >"${evidence_dir}/managed-cluster-rbac.yaml" 2>&1 || true
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" get \
    deployment,service,serviceaccount --selector app.kubernetes.io/managed-by=monitor-in-hand --output yaml \
    >"${evidence_dir}/managed-kubestatemetrics-resources.yaml" 2>&1 || true

  for deployment in prometheus-operator; do
    "${kubectl_bin}" --context "${context}" --namespace "${operator_namespace}" \
      logs "deployment/${deployment}" --all-containers >"${evidence_dir}/${deployment}.log" 2>&1 || true
  done
  "${kubectl_bin}" --context "${context}" --namespace monitor-in-hand-system \
    logs deployment/monitor-in-hand-monitor-in-hand --all-containers \
    >"${evidence_dir}/monitor-in-hand.log" 2>&1 || true
  for deployment in metrics-target blackbox-exporter vminsert vmselect; do
    "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
      logs "deployment/${deployment}" --all-containers >"${evidence_dir}/${deployment}.log" 2>&1 || true
  done
  for pod in $("${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    get pods --output 'jsonpath={.items[*].metadata.name}' 2>/dev/null); do
    case "${pod}" in
      vmstorage-* | prom-agent-* | ksm-*)
        "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
          logs "pod/${pod}" --all-containers >"${evidence_dir}/${pod}.log" 2>&1 || true
        ;;
    esac
  done
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    exec pod/prom-agent-fixture-0 --container prometheus -- \
    wget -qO- http://127.0.0.1:9090/api/v1/status/config \
    >"${evidence_dir}/agent-status-config.json" 2>&1 || true
  "${kubectl_bin}" --context "${context}" --namespace "${test_namespace}" \
    exec pod/prom-agent-fixture-0 --container prometheus -- \
    wget -qO- http://127.0.0.1:9090/api/v1/targets \
    >"${evidence_dir}/agent-targets.json" 2>&1 || true
  echo "诊断证据已保存：${evidence_dir}"
}

case "${action}" in
  up)
    create_environment
    ;;
  down)
    delete_environment
    ;;
  verify)
    verify_environment
    ;;
  evidence)
    capture_evidence
    ;;
  *)
    echo "用法：$0 up|verify|evidence|down" >&2
    exit 2
    ;;
esac
