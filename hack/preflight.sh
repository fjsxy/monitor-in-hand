#!/usr/bin/env bash

set -uo pipefail

agent_namespace=
agent_service_account=
config_namespaces=
discovery_namespaces=
agent_manifest=
collection_config_manifests=()
operator_namespace=prometheus-operator
operator_deployment=prometheus-operator
kube_context=
timeout_seconds=120
kubectl_bin=${KUBECTL_BIN:-kubectl}
failures=0
probe_created=false
probe_name=
probe_uid=
probe_statefulset=
agent_json=
collection_configs_json='[]'
RENDERED_MANIFESTS=

emit_diagnostic() {
  local level=$1 check=$2 status=$3 reason=$4 message=$5 details='{}'
  if (($# >= 6)); then
    details=$6
  fi
  jq --compact-output --null-input \
    --arg level "${level}" --arg check "${check}" --arg status "${status}" \
    --arg reason "${reason}" --arg message "${message}" --argjson details "${details}" \
    '{level: $level, check: $check, status: $status, reason: $reason, message: $message, details: $details}'
}

record_failure() {
  emit_diagnostic error "$@"
  failures=$((failures + 1))
}

record_pass() {
  emit_diagnostic info "$@"
}

usage() {
  cat >&2 <<'EOF'
Usage: hack/preflight.sh \
  --agent-namespace NAMESPACE \
  --agent-service-account NAME \
  --config-namespaces NAMESPACE[,NAMESPACE...]|* \
  --discovery-namespaces NAMESPACE[,NAMESPACE...]|* \
  [--agent-manifest FILE] \
  [--collection-config-manifest FILE]... \
  [--operator-namespace NAMESPACE] \
  [--operator-deployment NAME] \
  [--context KUBE_CONTEXT] \
  [--timeout-seconds SECONDS]
EOF
}

fail_input() {
  emit_diagnostic error input fail InvalidInput "$1"
  exit 2
}

require_option_value() {
  if (($# < 2)) || [[ -z "${2:-}" ]]; then
    fail_input "参数 $1 需要非空值"
  fi
}

while (($# > 0)); do
  case "$1" in
    --agent-namespace) require_option_value "$@"; agent_namespace=$2; shift 2 ;;
    --agent-service-account) require_option_value "$@"; agent_service_account=$2; shift 2 ;;
    --config-namespaces) require_option_value "$@"; config_namespaces=$2; shift 2 ;;
    --discovery-namespaces) require_option_value "$@"; discovery_namespaces=$2; shift 2 ;;
    --agent-manifest) require_option_value "$@"; agent_manifest=$2; shift 2 ;;
    --collection-config-manifest) require_option_value "$@"; collection_config_manifests+=("$2"); shift 2 ;;
    --operator-namespace) require_option_value "$@"; operator_namespace=$2; shift 2 ;;
    --operator-deployment) require_option_value "$@"; operator_deployment=$2; shift 2 ;;
    --context) require_option_value "$@"; kube_context=$2; shift 2 ;;
    --timeout-seconds) require_option_value "$@"; timeout_seconds=$2; shift 2 ;;
    --help|-h) usage; exit 0 ;;
    *) fail_input "未知参数：$1" ;;
  esac
done

if ! command -v jq >/dev/null 2>&1; then
  printf '%s\n' '{"level":"error","check":"tooling","status":"fail","reason":"ToolUnavailable","message":"缺少必需命令 jq","details":{}}'
  exit 1
fi

if [[ -z "${agent_namespace}" || -z "${agent_service_account}" || -z "${config_namespaces}" || -z "${discovery_namespaces}" ]]; then
  fail_input "--agent-namespace、--agent-service-account、--config-namespaces 和 --discovery-namespaces 均为必填参数"
fi
if ((${#collection_config_manifests[@]} > 0)) && [[ -z "${agent_manifest}" ]]; then
  fail_input "--collection-config-manifest 必须与 --agent-manifest 一起使用"
fi
if [[ -n "${agent_manifest}" && ! -f "${agent_manifest}" ]]; then
  fail_input "Agent manifest 不存在或不是文件：${agent_manifest}"
fi
for manifest in "${collection_config_manifests[@]}"; do
  if [[ ! -f "${manifest}" ]]; then
    fail_input "采集配置 manifest 不存在或不是文件：${manifest}"
  fi
done

dns_label_regex='^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'
dns_subdomain_regex='^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$'
validate_dns_label() {
  local value=$1 description=$2
  if ((${#value} > 63)) || [[ ! "${value}" =~ ${dns_label_regex} ]]; then
    fail_input "${description} 必须是合法的 Kubernetes DNS label：${value}"
  fi
}

validate_dns_subdomain() {
  local value=$1 description=$2
  if ((${#value} > 253)) || [[ ! "${value}" =~ ${dns_subdomain_regex} ]]; then
    fail_input "${description} 必须是合法的 Kubernetes DNS subdomain：${value}"
  fi
}

validate_namespace_scope() {
  local value=$1 description=$2 namespace
  local -a namespaces=()
  if [[ "${value}" == "*" ]]; then return; fi
  IFS=',' read -r -a namespaces <<<"${value}"
  if ((${#namespaces[@]} == 0)); then fail_input "${description} 不能为空"; fi
  for namespace in "${namespaces[@]}"; do
    if [[ -z "${namespace}" || "${namespace}" == "*" ]]; then
      fail_input "${description} 必须是逗号分隔的 namespace，或单独使用 *"
    fi
    validate_dns_label "${namespace}" "${description}"
  done
}

validate_dns_label "${agent_namespace}" "Agent namespace"
validate_dns_subdomain "${agent_service_account}" "Agent runtime ServiceAccount"
validate_dns_label "${operator_namespace}" "Prometheus Operator namespace"
validate_dns_subdomain "${operator_deployment}" "Prometheus Operator Deployment"
validate_namespace_scope "${config_namespaces}" "采集配置 namespace 范围"
validate_namespace_scope "${discovery_namespaces}" "discovery namespace 范围"
if [[ "${agent_service_account}" == default ]]; then
  fail_input "Agent runtime ServiceAccount 必须显式指定且不得使用 default"
fi
if [[ ! "${timeout_seconds}" =~ ^[1-9][0-9]*$ ]] || ((timeout_seconds > 900)); then
  fail_input "--timeout-seconds 必须是 1 到 900 的整数"
fi
if ! command -v "${kubectl_bin}" >/dev/null 2>&1; then
  record_failure tooling fail ToolUnavailable "缺少必需命令：${kubectl_bin}"
  emit_diagnostic error summary fail PreflightFailed "部署预检失败" \
    "$(jq --null-input --argjson failures "${failures}" '{failures: $failures}')"
  exit 1
fi

kubectl_args=()
if [[ -n "${kube_context}" ]]; then kubectl_args+=(--context "${kube_context}"); fi
kubectl_call() { "${kubectl_bin}" "${kubectl_args[@]}" "$@"; }

finish_failed() {
  emit_diagnostic error summary fail PreflightFailed "部署预检失败，未创建采集实例" \
    "$(jq --null-input --argjson failures "${failures}" '{failures: $failures}')"
  exit 1
}

render_manifest_documents() {
  local manifest=$1 rendered
  if ! rendered=$(kubectl_call create --dry-run=client --filename "${manifest}" --output json 2>&1); then
    record_failure collection-configuration fail ManifestUnreadable "无法读取或解析 manifest ${manifest}" \
      "$(jq --null-input --arg manifest "${manifest}" --arg error "${rendered}" '{manifest: $manifest, error: $error}')"
    return 1
  fi
  if ! RENDERED_MANIFESTS=$(jq --compact-output --slurp '
      [ .[] | if .kind == "List" then .items[] else . end ]
    ' <<<"${rendered}" 2>/dev/null); then
    record_failure collection-configuration fail ManifestUnreadable "manifest ${manifest} 未渲染为有效 Kubernetes JSON 对象" \
      "$(jq --null-input --arg manifest "${manifest}" '{manifest: $manifest}')"
    return 1
  fi
}

validate_collection_plan() {
  local rendered agent_count invalid_identity empty_selectors unsupported manifest
  [[ -n "${agent_manifest}" ]] || return 0

  # 先把单个 Agent manifest 规范化为 JSON，并确认它与命令行声明的运行身份一致。
  if ! render_manifest_documents "${agent_manifest}"; then return; fi
  rendered=${RENDERED_MANIFESTS}
  agent_count=$(jq '[.[] | select(.apiVersion == "monitoring.coreos.com/v1alpha1" and .kind == "PrometheusAgent")] | length' <<<"${rendered}")
  if [[ "${agent_count}" != 1 || "$(jq 'length' <<<"${rendered}")" != 1 ]]; then
    record_failure collection-configuration fail InvalidAgentManifest "Agent manifest 必须只包含一个 monitoring.coreos.com/v1alpha1 PrometheusAgent" \
      "$(jq --null-input --arg manifest "${agent_manifest}" --argjson agentCount "${agent_count}" '{manifest: $manifest, agentCount: $agentCount}')"
    return
  fi
  agent_json=$(jq '.[0]' <<<"${rendered}")
  if ! invalid_identity=$(jq --exit-status --compact-output \
      --arg namespace "${agent_namespace}" --arg serviceAccount "${agent_service_account}" '
        select((.metadata.namespace // "default") != $namespace or (.spec.serviceAccountName // "") != $serviceAccount) |
        {manifestNamespace: (.metadata.namespace // "default"), manifestServiceAccount: (.spec.serviceAccountName // "")}
      ' <<<"${agent_json}"); then
    invalid_identity=
  fi
  if [[ -n "${invalid_identity}" ]]; then
    record_failure collection-configuration fail AgentIdentityMismatch "Agent manifest 的 namespace 或 runtime ServiceAccount 与预检输入不一致" \
      "$(jq --null-input --arg namespace "${agent_namespace}" --arg serviceAccount "${agent_service_account}" --argjson actual "${invalid_identity}" '{expected: {namespace: $namespace, serviceAccount: $serviceAccount}, actual: $actual}')"
  fi

  # 分别检查四类 object selector；nil 是合法关闭状态，显式空对象不属于一期支持子集。
  empty_selectors=$(jq --compact-output '
      ["serviceMonitorSelector", "podMonitorSelector", "probeSelector", "scrapeConfigSelector"] as $fields |
      [$fields[] as $field |
        select(.spec | has($field)) |
        select((.spec[$field] | type) == "object") |
        select(((.spec[$field].matchLabels // {}) | length) == 0 and ((.spec[$field].matchExpressions // []) | length) == 0) |
        $field]
    ' <<<"${agent_json}")
  if [[ "${empty_selectors}" != '[]' ]]; then
    record_failure collection-configuration fail EmptyObjectSelectorUnsupported "空 object selector 是上游能力，但不属于一期支持子集" \
      "$(jq --null-input --argjson fields "${empty_selectors}" '{fields: $fields}')"
  elif jq --exit-status '
      [.spec.serviceMonitorSelector, .spec.podMonitorSelector, .spec.probeSelector, .spec.scrapeConfigSelector] |
      all(. == null)
    ' <<<"${agent_json}" >/dev/null; then
    record_pass collection-configuration pass ZeroCollectionConfigurationSupported "四类 object selector 均为 nil，属于合法零配置状态"
  else
    record_pass collection-configuration pass ExplicitObjectSelectors "计划中的非 nil object selector 均为明确的非空 LabelSelector"
  fi

  # 汇总管理员提供的采集配置，再拒绝需要一期基线外权限或依赖的 discovery。
  for manifest in "${collection_config_manifests[@]}"; do
    if render_manifest_documents "${manifest}"; then
      rendered=${RENDERED_MANIFESTS}
      collection_configs_json=$(jq --compact-output --argjson current "${collection_configs_json}" '$current + .' <<<"${rendered}")
    fi
  done
  if ((failures > 0)); then return; fi

  unsupported=$(jq --compact-output '
      def unsupported_sources:
        ["fileSDConfigs", "httpSDConfigs", "consulSDConfigs", "dnsSDConfigs", "ec2SDConfigs",
         "azureSDConfigs", "gceSDConfigs", "openstackSDConfigs", "digitalOceanSDConfigs",
         "kumaSDConfigs", "eurekaSDConfigs", "dockerSDConfigs", "linodeSDConfigs",
         "hetznerSDConfigs", "nomadSDConfigs", "dockerSwarmSDConfigs", "puppetDBSDConfigs",
         "lightSailSDConfigs", "ovhcloudSDConfigs", "scalewaySDConfigs", "ionosSDConfigs"];
      [ .[] |
        select(.apiVersion == "monitoring.coreos.com/v1alpha1" and .kind == "ScrapeConfig") |
        . as $config |
        ([unsupported_sources[] as $field | select($config.spec | has($field)) | $field]) as $fields |
        ([($config.spec.kubernetesSDConfigs // [])[]?.role |
          select(. != "Pod" and . != "Service" and . != "Endpoints" and . != "EndpointSlice" and . != "Ingress")]) as $roles |
        ([($config.spec.kubernetesSDConfigs // [])[]? |
          select(.attachMetadata.node == true) | .role]) as $nodeMetadataRoles |
        select(($fields | length) > 0 or ($roles | length) > 0 or ($nodeMetadataRoles | length) > 0) |
        {namespace: (.metadata.namespace // "default"), name: .metadata.name,
         unsupportedFields: $fields, unsupportedKubernetesRoles: $roles,
         nodeMetadataRoles: $nodeMetadataRoles}]
    ' <<<"${collection_configs_json}")
  if [[ "${unsupported}" != '[]' ]]; then
    record_failure collection-configuration fail UnsupportedScrapeConfigDiscovery "ScrapeConfig 使用了 Node、FileSD 或其他一期外 discovery" \
      "$(jq --null-input --argjson configs "${unsupported}" '{configs: $configs, supported: ["static", "Pod", "Service", "Endpoints", "EndpointSlice", "Ingress"]}')"
  else
    record_pass collection-configuration pass SupportedScrapeConfigDiscovery "提供的 ScrapeConfig 仅使用一期支持的 discovery"
  fi
}

# 先检查计划中无需访问集群即可判定的支持边界，避免无效配置触发临时 reconcile 探针。
validate_collection_plan
if ((failures > 0)); then finish_failed; fi

# kubectl 可能把 client/server 版本偏差警告写到 stderr；只允许 stdout 进入 JSON 解析。
version_stderr_file=$(mktemp "${TMPDIR:-/tmp}/monitor-in-hand-preflight-version.XXXXXX")
if ! version_json=$(kubectl_call version --output json 2>"${version_stderr_file}"); then
  version_stderr=$(cat "${version_stderr_file}")
  rm -f "${version_stderr_file}"
  record_failure kubernetes-version fail KubernetesAPIUnavailable "无法读取 Kubernetes server version" \
    "$(jq --null-input --arg stdout "${version_json}" --arg stderr "${version_stderr}" '{stdout: $stdout, stderr: $stderr}')"
  finish_failed
fi
rm -f "${version_stderr_file}"
if ! version_fields=$(jq --raw-output --exit-status '
    [.serverVersion.major, (.serverVersion.minor | sub("[^0-9].*$"; "")), .serverVersion.gitVersion] |
    select(length == 3 and all(.[]; type == "string" and length > 0)) |
    @tsv
  ' <<<"${version_json}" 2>&1); then
  record_failure kubernetes-version fail InvalidKubernetesVersionResponse "Kubernetes server version 响应无法解析" \
    "$(jq --null-input --arg error "${version_fields}" '{error: $error}')"
  finish_failed
fi
IFS=$'\t' read -r server_major server_minor server_git_version <<<"${version_fields}"
minor="${server_major}.${server_minor}"
case "${minor}" in
  1.19)
    record_pass kubernetes-version pass SupportedKubernetes119 "Kubernetes ${server_git_version} 属于一期支持的 1.19.x 事实范围" \
      "$(jq --null-input --arg gitVersion "${server_git_version}" --arg minor "${minor}" '{gitVersion: $gitVersion, minor: $minor}')"
    ;;
  1.28)
    record_pass kubernetes-version pass SupportedKubernetes128 "Kubernetes ${server_git_version} 属于一期支持的 1.28.x 事实范围" \
      "$(jq --null-input --arg gitVersion "${server_git_version}" --arg minor "${minor}" '{gitVersion: $gitVersion, minor: $minor}')"
    ;;
  *)
    record_failure kubernetes-version fail UnsupportedKubernetesVersion "Kubernetes ${server_git_version} 不在一期支持的 1.19.x 或 1.28.x 范围" \
      "$(jq --null-input --arg gitVersion "${server_git_version}" --arg minor "${minor}" '{gitVersion: $gitVersion, minor: $minor, supportedMinors: ["1.19", "1.28"]}')"
    finish_failed
    ;;
esac

scope_to_array() {
  local value=$1
  SCOPE_VALUES=()
  if [[ "${value}" != "*" ]]; then IFS=',' read -r -a SCOPE_VALUES <<<"${value}"; fi
}

namespaces_to_check=("${agent_namespace}" "${operator_namespace}")
scope_to_array "${config_namespaces}"; namespaces_to_check+=("${SCOPE_VALUES[@]}")
scope_to_array "${discovery_namespaces}"; namespaces_to_check+=("${SCOPE_VALUES[@]}")
checked_namespaces=,
for namespace in "${namespaces_to_check[@]}"; do
  if [[ "${checked_namespaces}" == *",${namespace},"* ]]; then continue; fi
  checked_namespaces+="${namespace},"
  if namespace_error=$(kubectl_call get namespace "${namespace}" --output name 2>&1); then
    record_pass namespace pass NamespaceExists "namespace ${namespace} 存在" \
      "$(jq --null-input --arg namespace "${namespace}" '{namespace: $namespace}')"
  else
    record_failure namespace fail NamespaceNotFound "namespace ${namespace} 不存在或不可读" \
      "$(jq --null-input --arg namespace "${namespace}" --arg error "${namespace_error}" '{namespace: $namespace, error: $error}')"
  fi
done

if service_account_error=$(kubectl_call --namespace "${agent_namespace}" get serviceaccount "${agent_service_account}" --output name 2>&1); then
  record_pass runtime-service-account pass RuntimeServiceAccountExists "Agent runtime ServiceAccount ${agent_namespace}/${agent_service_account} 存在且不是 default" \
    "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${agent_service_account}" '{namespace: $namespace, name: $name}')"
else
  record_failure runtime-service-account fail RuntimeServiceAccountNotFound "Agent runtime ServiceAccount ${agent_namespace}/${agent_service_account} 不存在或不可读" \
    "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${agent_service_account}" --arg error "${service_account_error}" '{namespace: $namespace, name: $name, error: $error}')"
fi
if ((failures > 0)); then finish_failed; fi

validate_collection_selection() {
  local namespaces_json report declared_scope refs_json reference namespace kind name key path output
  [[ -n "${agent_manifest}" ]] || return

  # 读取实时 Namespace labels，作为解析 nil、空对象与非空 namespace selector 的事实输入。
  if ! namespaces_json=$(kubectl_call get namespaces --output json 2>&1); then
    record_failure collection-configuration fail NamespaceLabelsUnavailable "无法读取 Namespace labels 以解析 namespace selector" \
      "$(jq --null-input --arg error "${namespaces_json}" '{error: $error}')"
    return
  fi
  if ! jq --exit-status '(.items | type) == "array"' <<<"${namespaces_json}" >/dev/null 2>&1; then
    record_failure collection-configuration fail NamespaceLabelsUnavailable "Namespace 列表响应无法解析"
    return
  fi

  if [[ "${config_namespaces}" == "*" ]]; then
    declared_scope='[]'
  else
    declared_scope=$(jq --compact-output --null-input --arg scope "${config_namespaces}" '$scope | split(",")')
  fi
  # 同时应用 object selector、namespace selector 与 Operator watch scope，生成必须覆盖的配置集合。
  report=$(jq --compact-output --null-input \
    --argjson agent "${agent_json}" \
    --argjson configs "${collection_configs_json}" \
    --argjson namespaces "${namespaces_json}" \
    --arg configScope "${config_namespaces}" \
    --arg operatorAllow "${general_allow:-}" \
    --arg operatorDeny "${general_deny:-}" \
    --argjson declaredScope "${declared_scope}" '
      def selector_empty($selector):
        (($selector.matchLabels // {}) | length) == 0 and (($selector.matchExpressions // []) | length) == 0;
      def selector_matches($selector; $labels):
        (($selector.matchLabels // {}) | to_entries |
          all(. as $entry | ($labels[$entry.key] // null) == $entry.value)) and
        (($selector.matchExpressions // []) | all(
          . as $expression |
          if $expression.operator == "In" then
            (($labels[$expression.key] // null) as $value | ($expression.values // []) | index($value) != null)
          elif $expression.operator == "NotIn" then
            (($labels | has($expression.key) | not) or (($expression.values // []) | index($labels[$expression.key]) == null))
          elif $expression.operator == "Exists" then
            ($labels | has($expression.key))
          elif $expression.operator == "DoesNotExist" then
            ($labels | has($expression.key) | not)
          else false end));
      def fields_for_kind($kind):
        if $kind == "ServiceMonitor" then ["serviceMonitorSelector", "serviceMonitorNamespaceSelector"]
        elif $kind == "PodMonitor" then ["podMonitorSelector", "podMonitorNamespaceSelector"]
        elif $kind == "Probe" then ["probeSelector", "probeNamespaceSelector"]
        elif $kind == "ScrapeConfig" then ["scrapeConfigSelector", "scrapeConfigNamespaceSelector"]
        else [null, null] end;
      def namespace_labels($name):
        ([ $namespaces.items[] | select(.metadata.name == $name) | (.metadata.labels // {}) ][0] // {});
      def csv_contains($csv; $name):
        ($csv | split(",") | index($name)) != null;
      def operator_watches($name):
        if $operatorAllow != "" then csv_contains($operatorAllow; $name)
        elif $operatorDeny != "" then (csv_contains($operatorDeny; $name) | not)
        else true end;
      def namespace_selected($selector; $namespace):
        operator_watches($namespace) and
        (if $selector == null then $namespace == ($agent.metadata.namespace // "default")
         elif selector_empty($selector) then true
         else selector_matches($selector; namespace_labels($namespace)) end);
      def selected_namespaces($selector):
        if $selector == null then [($agent.metadata.namespace // "default") | select(operator_watches(.))]
        elif selector_empty($selector) then [$namespaces.items[] | select(operator_watches(.metadata.name)) | .metadata.name]
        else [$namespaces.items[] |
          select(operator_watches(.metadata.name) and selector_matches($selector; (.metadata.labels // {}))) |
          .metadata.name] end;

      (["ServiceMonitor", "PodMonitor", "Probe", "ScrapeConfig"] |
        map(. as $kind | fields_for_kind($kind) as $fields |
          select($agent.spec[$fields[0]] != null) |
          {kind: $kind, objectField: $fields[0], namespaceField: $fields[1],
           namespaces: selected_namespaces($agent.spec[$fields[1]])})) as $enabled |
      ([ $configs[] as $config |
        fields_for_kind($config.kind) as $fields |
        ($agent.spec[$fields[0]] // null) as $objectSelector |
        ($agent.spec[$fields[1]] // null) as $namespaceSelector |
        select($fields[0] == null or $objectSelector == null or
          (selector_matches($objectSelector; ($config.metadata.labels // {})) | not) or
          (namespace_selected($namespaceSelector; ($config.metadata.namespace // "default")) | not)) |
        {apiVersion: $config.apiVersion, kind: $config.kind,
         namespace: ($config.metadata.namespace // "default"), name: $config.metadata.name}]) as $unselected |
      ([ $enabled[] | select(.kind as $kind | [$configs[].kind] | index($kind) == null) | .kind]) as $missingKinds |
      ([$enabled[].namespaces[]] | unique) as $requiredNamespaces |
      ($requiredNamespaces |
        if $configScope == "*" then [] else map(select(. as $namespace | $declaredScope | index($namespace) == null)) end) as $uncoveredNamespaces |
      {unselected: $unselected, missingKinds: $missingKinds,
       requiredNamespaces: $requiredNamespaces, uncoveredNamespaces: $uncoveredNamespaces}
    ')

  # 将未选中对象、缺失类型和 namespace 覆盖差距分别转成稳定诊断。
  if [[ "$(jq '.unselected | length' <<<"${report}")" != 0 || "$(jq '.missingKinds | length' <<<"${report}")" != 0 ]]; then
    record_failure collection-configuration fail CollectionConfigCoverageIncomplete "提供的采集配置没有完整对应四类独立 selector，或包含未被选中的对象" "${report}"
  else
    record_pass collection-configuration pass CollectionConfigsSelected "提供的四类采集配置分别被对应 object/namespace selector 选中" "${report}"
  fi
  if [[ "$(jq '.uncoveredNamespaces | length' <<<"${report}")" != 0 ]]; then
    record_failure collection-configuration fail CollectionNamespaceScopeIncomplete "配置 namespace 范围未覆盖 namespace selector 的原生选择结果" "${report}"
  else
    record_pass collection-configuration pass CollectionNamespaceScopeCovered "配置 namespace 范围覆盖 nil、空对象或非空 namespace selector 的原生选择结果" \
      "$(jq '{requiredNamespaces}' <<<"${report}")"
  fi

  # 所有 key selector 都按声明对象的 namespace 解析，并验证对象可读且引用 key 存在。
  refs_json=$(jq --compact-output --null-input --argjson agent "${agent_json}" --argjson configs "${collection_configs_json}" '
      def key_selector_refs($document; $namespace):
        [$document.spec | paths(objects | select((.name? | type) == "string" and (.key? | type) == "string")) as $path |
          getpath($path) as $selector |
          ($path | map(tostring) | join(".")) as $fieldPath |
          {namespace: $namespace,
           kind: (if ($fieldPath | ascii_downcase | contains("configmap")) then "configmap" else "secret" end),
           name: $selector.name, key: $selector.key, path: $fieldPath}];
      (key_selector_refs($agent; ($agent.metadata.namespace // "default")) +
       [($agent.spec.secrets // [])[] | {namespace: ($agent.metadata.namespace // "default"), kind: "secret", name: ., key: null, path: "spec.secrets"}] +
       [($agent.spec.configMaps // [])[] | {namespace: ($agent.metadata.namespace // "default"), kind: "configmap", name: ., key: null, path: "spec.configMaps"}] +
       [$configs[] as $config | key_selector_refs($config; ($config.metadata.namespace // "default"))[]]) |
      unique_by([.namespace, .kind, .name, .key])
    ')
  while IFS= read -r reference; do
    [[ -n "${reference}" ]] || continue
    namespace=$(jq --raw-output '.namespace' <<<"${reference}")
    kind=$(jq --raw-output '.kind' <<<"${reference}")
    name=$(jq --raw-output '.name' <<<"${reference}")
    key=$(jq --raw-output '.key // empty' <<<"${reference}")
    path=$(jq --raw-output '.path' <<<"${reference}")
    if output=$(kubectl_call --namespace "${namespace}" get "${kind}" "${name}" --output json 2>&1); then
      if [[ -n "${key}" ]] && ! jq --exit-status --arg key "${key}" '(.data // {}) | has($key)' <<<"${output}" >/dev/null 2>&1; then
        record_failure collection-reference fail ReferencedAssetKeyUnavailable "${kind} ${namespace}/${name} 缺少引用的 key ${key}" "${reference}"
      else
        record_pass collection-reference pass ReferencedAssetReadable "${kind} ${namespace}/${name} 的同 namespace 引用可用" "${reference}"
      fi
    else
      record_failure collection-reference fail ReferencedAssetUnavailable "${kind} ${namespace}/${name} 必须存在于声明对象的同一 namespace 且可读" \
        "$(jq --null-input --argjson reference "${reference}" --arg error "${output}" '{reference: $reference, error: $error}')"
    fi
  done < <(jq --compact-output '.[]' <<<"${refs_json}")
}

check_api_resources() {
  local version=$1 expected_json=$2 response missing
  if ! response=$(kubectl_call get --raw "/apis/monitoring.coreos.com/${version}" 2>&1); then
    record_failure api-discovery fail APIGroupVersionNotServed "monitoring.coreos.com/${version} API 不可用" \
      "$(jq --null-input --arg version "${version}" --arg error "${response}" '{version: $version, error: $error}')"
    return
  fi
  if ! missing=$(jq --compact-output --argjson expected "${expected_json}" '[.resources[]?.name] as $served | [$expected[] | select(. as $name | $served | index($name) == null)]' <<<"${response}" 2>/dev/null); then
    record_failure api-discovery fail InvalidAPIDiscoveryResponse "monitoring.coreos.com/${version} API discovery 响应无法解析" \
      "$(jq --null-input --arg version "${version}" '{version: $version}')"
    return
  fi
  if [[ "${missing}" != '[]' ]]; then
    record_failure api-discovery fail APIResourceNotServed "monitoring.coreos.com/${version} 未 served 所有必需 API" \
      "$(jq --null-input --arg version "${version}" --argjson missing "${missing}" '{version: $version, missingResources: $missing}')"
    return
  fi
  record_pass api-discovery pass APIResourcesServed "monitoring.coreos.com/${version} 的必需 API 均已 served" \
    "$(jq --null-input --arg version "${version}" --argjson resources "${expected_json}" '{version: $version, resources: $resources}')"
}

check_crd_schema() {
  local crd=$1 version=$2 paths_json=$3 response version_facts served missing
  if ! response=$(kubectl_call get customresourcedefinition "${crd}" --output json 2>&1); then
    record_failure crd-schema fail CRDNotFound "CRD ${crd} 不存在或不可读" \
      "$(jq --null-input --arg crd "${crd}" --arg error "${response}" '{crd: $crd, error: $error}')"
    return
  fi
  if ! version_facts=$(jq --compact-output --exit-status --arg version "${version}" '
      if (.spec.versions | type) != "array" then error("spec.versions must be an array") else
        [.spec.versions[] | select(.name == $version)] as $matches |
        {exists: ($matches | length > 0), served: ($matches | any(.served == true))}
      end
    ' <<<"${response}" 2>&1); then
    record_failure crd-schema fail InvalidCRDSchema "CRD ${crd} 响应无法解析" \
      "$(jq --null-input --arg crd "${crd}" --arg version "${version}" --arg error "${version_facts}" '{crd: $crd, version: $version, error: $error}')"
    return
  fi
  served=$(jq --raw-output '.served' <<<"${version_facts}")
  if [[ "${served}" != true ]]; then
    record_failure crd-schema fail CRDVersionNotServed "CRD ${crd} 未 served ${version}" \
      "$(jq --null-input --arg crd "${crd}" --arg version "${version}" '{crd: $crd, version: $version}')"
    return
  fi
  if ! missing=$(jq --compact-output --arg version "${version}" --argjson paths "${paths_json}" '
      (.spec.versions[] | select(.name == $version and .served == true) | .schema.openAPIV3Schema) as $schema |
      [$paths[] | select(. as $path | ($schema | getpath($path | split("."))) == null)]
    ' <<<"${response}" 2>&1); then
    record_failure crd-schema fail InvalidCRDSchema "CRD ${crd} 的 ${version} schema 无法解析" \
      "$(jq --null-input --arg crd "${crd}" --arg version "${version}" --arg error "${missing}" '{crd: $crd, version: $version, error: $error}')"
    return
  fi
  if [[ "${missing}" != '[]' ]]; then
    record_failure crd-schema fail RequiredSchemaFieldMissing "CRD ${crd} 缺少一期必需 schema 字段" \
      "$(jq --null-input --arg crd "${crd}" --arg version "${version}" --argjson missing "${missing}" '{crd: $crd, version: $version, missingFields: $missing}')"
    return
  fi
  record_pass crd-schema pass RequiredSchemaFieldsPresent "CRD ${crd} ${version} served 且包含一期必需字段" \
    "$(jq --null-input --arg crd "${crd}" --arg version "${version}" --argjson paths "${paths_json}" '{crd: $crd, version: $version, fieldCount: ($paths | length)}')"
}

check_api_resources v1alpha1 '["prometheusagents","scrapeconfigs"]'
check_api_resources v1 '["servicemonitors","podmonitors","probes"]'
check_crd_schema prometheusagents.monitoring.coreos.com v1alpha1 '[
  "properties.spec.properties.image", "properties.spec.properties.version",
  "properties.spec.properties.serviceAccountName", "properties.spec.properties.replicas",
  "properties.spec.properties.shards", "properties.spec.properties.storage",
  "properties.spec.properties.storage.properties.emptyDir",
  "properties.spec.properties.storage.properties.volumeClaimTemplate",
  "properties.spec.properties.storage.properties.volumeClaimTemplate.properties.spec.properties.accessModes",
  "properties.spec.properties.storage.properties.volumeClaimTemplate.properties.spec.properties.resources",
  "properties.spec.properties.storage.properties.volumeClaimTemplate.properties.spec.properties.storageClassName",
  "properties.spec.properties.walCompression", "properties.spec.properties.externalLabels",
  "properties.spec.properties.replicaExternalLabelName", "properties.spec.properties.serviceMonitorSelector",
  "properties.spec.properties.serviceMonitorSelector.properties.matchLabels",
  "properties.spec.properties.serviceMonitorSelector.properties.matchExpressions",
  "properties.spec.properties.serviceMonitorNamespaceSelector",
  "properties.spec.properties.serviceMonitorNamespaceSelector.properties.matchLabels",
  "properties.spec.properties.serviceMonitorNamespaceSelector.properties.matchExpressions",
  "properties.spec.properties.podMonitorSelector",
  "properties.spec.properties.podMonitorSelector.properties.matchLabels",
  "properties.spec.properties.podMonitorSelector.properties.matchExpressions",
  "properties.spec.properties.podMonitorNamespaceSelector",
  "properties.spec.properties.podMonitorNamespaceSelector.properties.matchLabels",
  "properties.spec.properties.podMonitorNamespaceSelector.properties.matchExpressions",
  "properties.spec.properties.probeSelector",
  "properties.spec.properties.probeSelector.properties.matchLabels",
  "properties.spec.properties.probeSelector.properties.matchExpressions",
  "properties.spec.properties.probeNamespaceSelector",
  "properties.spec.properties.probeNamespaceSelector.properties.matchLabels",
  "properties.spec.properties.probeNamespaceSelector.properties.matchExpressions",
  "properties.spec.properties.scrapeConfigSelector",
  "properties.spec.properties.scrapeConfigSelector.properties.matchLabels",
  "properties.spec.properties.scrapeConfigSelector.properties.matchExpressions",
  "properties.spec.properties.scrapeConfigNamespaceSelector",
  "properties.spec.properties.scrapeConfigNamespaceSelector.properties.matchLabels",
  "properties.spec.properties.scrapeConfigNamespaceSelector.properties.matchExpressions",
  "properties.spec.properties.remoteWrite",
  "properties.spec.properties.remoteWrite.items.properties.name", "properties.spec.properties.remoteWrite.items.properties.url",
  "properties.spec.properties.remoteWrite.items.properties.remoteTimeout", "properties.spec.properties.remoteWrite.items.properties.messageVersion",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.capacity",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.minShards",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.maxShards",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.maxSamplesPerSend",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.batchSendDeadline",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.minBackoff",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.maxBackoff",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.retryOnRateLimit",
  "properties.spec.properties.remoteWrite.items.properties.queueConfig.properties.sampleAgeLimit"
]'
check_crd_schema scrapeconfigs.monitoring.coreos.com v1alpha1 '[
  "properties.spec.properties.staticConfigs",
  "properties.spec.properties.staticConfigs.items.properties.targets",
  "properties.spec.properties.staticConfigs.items.properties.labels",
  "properties.spec.properties.kubernetesSDConfigs",
  "properties.spec.properties.kubernetesSDConfigs.items.properties.role",
  "properties.spec.properties.kubernetesSDConfigs.items.properties.namespaces.properties.names",
  "properties.spec.properties.kubernetesSDConfigs.items.properties.namespaces.properties.ownNamespace",
  "properties.spec.properties.kubernetesSDConfigs.items.properties.selectors.items.properties.role",
  "properties.spec.properties.kubernetesSDConfigs.items.properties.selectors.items.properties.label",
  "properties.spec.properties.kubernetesSDConfigs.items.properties.selectors.items.properties.field"
]'
check_crd_schema servicemonitors.monitoring.coreos.com v1 '[
  "properties.spec.properties.selector.properties.matchLabels",
  "properties.spec.properties.selector.properties.matchExpressions",
  "properties.spec.properties.namespaceSelector.properties.any",
  "properties.spec.properties.namespaceSelector.properties.matchNames",
  "properties.spec.properties.endpoints.items.properties.port",
  "properties.spec.properties.endpoints.items.properties.interval"
]'
check_crd_schema podmonitors.monitoring.coreos.com v1 '[
  "properties.spec.properties.selector.properties.matchLabels",
  "properties.spec.properties.selector.properties.matchExpressions",
  "properties.spec.properties.namespaceSelector.properties.any",
  "properties.spec.properties.namespaceSelector.properties.matchNames",
  "properties.spec.properties.podMetricsEndpoints.items.properties.port",
  "properties.spec.properties.podMetricsEndpoints.items.properties.interval"
]'
check_crd_schema probes.monitoring.coreos.com v1 '[
  "properties.spec.properties.module",
  "properties.spec.properties.prober.properties.url",
  "properties.spec.properties.prober.properties.path",
  "properties.spec.properties.prober.properties.scheme",
  "properties.spec.properties.targets.properties.staticConfig.properties.static",
  "properties.spec.properties.targets.properties.staticConfig.properties.labels",
  "properties.spec.properties.targets.properties.ingress.properties.selector",
  "properties.spec.properties.targets.properties.ingress.properties.namespaceSelector"
]'

operator_available=true
if ! operator_json=$(kubectl_call --namespace "${operator_namespace}" get deployment "${operator_deployment}" --output json 2>&1); then
  record_failure operator-watch-scope fail PrometheusOperatorDeploymentNotFound "Prometheus Operator Deployment ${operator_namespace}/${operator_deployment} 不存在或不可读" \
    "$(jq --null-input --arg namespace "${operator_namespace}" --arg deployment "${operator_deployment}" --arg error "${operator_json}" '{namespace: $namespace, deployment: $deployment, error: $error}')"
  operator_available=false
else
  if ! operator_facts=$(jq --compact-output --exit-status '
      .spec.template.spec as $pod |
      if ($pod | type) != "object" or ($pod.containers | type) != "array" or ($pod.containers | length) == 0 then
        error("spec.template.spec.containers must be a non-empty array")
      else
        ([ $pod.containers[] | select(.name == "prometheus-operator") ][0] // $pod.containers[0]) as $container |
        ($container.args // []) as $args |
        if ($args | type) != "array" or any($args[]; type != "string") then
          error("Prometheus Operator container args must be an array of strings")
        elif (($pod.serviceAccountName // "default") | type) != "string" then
          error("serviceAccountName must be a string")
        else
          {serviceAccountName: ($pod.serviceAccountName // "default"), args: $args}
        end
      end
    ' <<<"${operator_json}" 2>&1); then
    record_failure operator-watch-scope fail InvalidPrometheusOperatorDeployment "Prometheus Operator Deployment 响应无法解析" \
      "$(jq --null-input --arg namespace "${operator_namespace}" --arg deployment "${operator_deployment}" --arg error "${operator_facts}" '{namespace: $namespace, deployment: $deployment, error: $error}')"
    operator_available=false
  else
    operator_service_account=$(jq --raw-output '.serviceAccountName' <<<"${operator_facts}")
    operator_args=$(jq --compact-output '.args' <<<"${operator_facts}")
  fi
fi

flag_values() {
  local args_json=$1 flag=$2
  jq --raw-output --arg flag "${flag}" '. as $args | [range(0; length) as $i | if $args[$i] == $flag then ($args[$i + 1] // "") elif ($args[$i] | startswith($flag + "=")) then ($args[$i] | ltrimstr($flag + "=")) else empty end] | map(select(length > 0)) | join(",")' <<<"${args_json}"
}

list_contains() {
  local csv=$1 expected=$2 item
  local -a values=()
  IFS=',' read -r -a values <<<"${csv}"
  for item in "${values[@]}"; do if [[ "${item}" == "${expected}" ]]; then return 0; fi; done
  return 1
}

scope_allows_namespace() {
  local allow=$1 deny=$2 namespace=$3
  if [[ -n "${allow}" ]]; then list_contains "${allow}" "${namespace}"; return; fi
  if [[ -n "${deny}" ]] && list_contains "${deny}" "${namespace}"; then return 1; fi
  return 0
}

check_watch_scope() {
  local kind=$1 requested=$2 allow=$3 deny=$4 namespace
  local -a namespaces=()
  if [[ "${requested}" == "*" ]]; then
    if [[ -n "${allow}" || -n "${deny}" ]]; then
      record_failure operator-watch-scope fail WatchScopeRestricted "Prometheus Operator 的 ${kind} watch scope 不覆盖所有 namespace" \
        "$(jq --null-input --arg kind "${kind}" --arg allow "${allow}" --arg deny "${deny}" '{kind: $kind, requested: "*", allow: $allow, deny: $deny}')"
    else
      record_pass operator-watch-scope pass WatchScopeCovered "Prometheus Operator 的 ${kind} watch scope 覆盖所有 namespace" \
        "$(jq --null-input --arg kind "${kind}" '{kind: $kind, requested: "*"}')"
    fi
    return
  fi
  IFS=',' read -r -a namespaces <<<"${requested}"
  for namespace in "${namespaces[@]}"; do
    if scope_allows_namespace "${allow}" "${deny}" "${namespace}"; then
      record_pass operator-watch-scope pass WatchScopeCovered "Prometheus Operator 的 ${kind} watch scope 覆盖 namespace ${namespace}" \
        "$(jq --null-input --arg kind "${kind}" --arg namespace "${namespace}" '{kind: $kind, namespace: $namespace}')"
    else
      record_failure operator-watch-scope fail WatchScopeRestricted "Prometheus Operator 的 ${kind} watch scope 不覆盖 namespace ${namespace}" \
        "$(jq --null-input --arg kind "${kind}" --arg namespace "${namespace}" --arg allow "${allow}" --arg deny "${deny}" '{kind: $kind, namespace: $namespace, allow: $allow, deny: $deny}')"
    fi
  done
}

if [[ "${operator_available}" == true ]]; then
  general_allow=$(flag_values "${operator_args}" --namespaces)
  general_deny=$(flag_values "${operator_args}" --deny-namespaces)
  prometheus_allow=$(flag_values "${operator_args}" --prometheus-instance-namespaces)
  if [[ -n "${general_allow}" && -n "${general_deny}" ]]; then
    record_failure operator-watch-scope fail InvalidWatchScope "Prometheus Operator 同时配置 --namespaces 和 --deny-namespaces" \
      "$(jq --null-input --arg allow "${general_allow}" --arg deny "${general_deny}" '{allow: $allow, deny: $deny}')"
  fi
  if [[ -n "${prometheus_allow}" ]]; then
    check_watch_scope PrometheusAgent "${agent_namespace}" "${prometheus_allow}" ""
  else
    check_watch_scope PrometheusAgent "${agent_namespace}" "${general_allow}" "${general_deny}"
  fi
  check_watch_scope collection-config "${config_namespaces}" "${general_allow}" "${general_deny}"
fi

# 在确认 Operator 实际 watch scope 后，按四类 selector 解析计划配置并检查同 namespace 引用。
validate_collection_selection
if ((failures > 0)); then finish_failed; fi

check_permission() {
  local check=$1 subject=$2 verb=$3 resource=$4 namespace_scope=$5 subresource=${6:-} output
  local -a scope_args=()
  if [[ "${namespace_scope}" == "*" ]]; then scope_args+=(--all-namespaces); else scope_args+=(--namespace "${namespace_scope}"); fi
  local -a subresource_args=()
  if [[ -n "${subresource}" ]]; then subresource_args+=(--subresource "${subresource}"); fi
  if output=$(kubectl_call auth can-i "${verb}" "${resource}" "${scope_args[@]}" "${subresource_args[@]}" --as "${subject}" 2>&1) && [[ "${output}" == yes ]]; then
    record_pass "${check}" pass RBACAllowed "${subject} 可在 ${namespace_scope} 执行 ${verb} ${resource}${subresource:+/${subresource}}" \
      "$(jq --null-input --arg subject "${subject}" --arg verb "${verb}" --arg resource "${resource}" --arg subresource "${subresource}" --arg namespace "${namespace_scope}" '{subject: $subject, verb: $verb, resource: $resource, subresource: (if $subresource == "" then null else $subresource end), namespace: $namespace}')"
  else
    record_failure "${check}" fail RBACDenied "${subject} 不能在 ${namespace_scope} 执行 ${verb} ${resource}${subresource:+/${subresource}}" \
      "$(jq --null-input --arg subject "${subject}" --arg verb "${verb}" --arg resource "${resource}" --arg subresource "${subresource}" --arg namespace "${namespace_scope}" --arg output "${output}" '{subject: $subject, verb: $verb, resource: $resource, subresource: (if $subresource == "" then null else $subresource end), namespace: $namespace, output: $output}')"
  fi
}

check_nonresource_permission() {
  local check=$1 subject=$2 verb=$3 path=$4 output
  if output=$(kubectl_call auth can-i "${verb}" "${path}" --as "${subject}" 2>&1) && [[ "${output}" == yes ]]; then
    record_pass "${check}" pass RBACAllowed "${subject} 可执行 ${verb} ${path}" \
      "$(jq --null-input --arg subject "${subject}" --arg verb "${verb}" --arg path "${path}" '{subject: $subject, verb: $verb, path: $path}')"
  else
    record_failure "${check}" fail RBACDenied "${subject} 不能执行 ${verb} ${path}" \
      "$(jq --null-input --arg subject "${subject}" --arg verb "${verb}" --arg path "${path}" --arg output "${output}" '{subject: $subject, verb: $verb, path: $path, output: $output}')"
  fi
}

if [[ "${operator_available}" == true ]]; then
  operator_subject="system:serviceaccount:${operator_namespace}:${operator_service_account}"
  for verb in get list watch; do check_permission operator-rbac "${operator_subject}" "${verb}" prometheusagents.monitoring.coreos.com "${agent_namespace}"; done
  for verb in update patch; do
    check_permission operator-rbac "${operator_subject}" "${verb}" prometheusagents.monitoring.coreos.com "${agent_namespace}" status
    check_permission operator-rbac "${operator_subject}" "${verb}" prometheusagents.monitoring.coreos.com "${agent_namespace}" finalizers
  done
  for resource in statefulsets.apps secrets configmaps; do
    for verb in get list watch create update patch delete; do check_permission operator-rbac "${operator_subject}" "${verb}" "${resource}" "${agent_namespace}"; done
  done
  for resource in services endpoints; do
    for verb in get create update delete; do check_permission operator-rbac "${operator_subject}" "${verb}" "${resource}" "${agent_namespace}"; done
  done
  for verb in list delete; do check_permission operator-rbac "${operator_subject}" "${verb}" pods "${agent_namespace}"; done
  for verb in create patch; do check_permission operator-rbac "${operator_subject}" "${verb}" events "${agent_namespace}"; done
  for verb in get list watch; do check_permission operator-rbac "${operator_subject}" "${verb}" namespaces "*"; done
  scope_to_array "${config_namespaces}"; config_scope_values=("${SCOPE_VALUES[@]}")
  if [[ "${config_namespaces}" == "*" ]]; then config_scope_values=("*"); fi
  for namespace in "${config_scope_values[@]}"; do
    for resource in servicemonitors.monitoring.coreos.com podmonitors.monitoring.coreos.com probes.monitoring.coreos.com scrapeconfigs.monitoring.coreos.com; do
      for verb in get list watch; do check_permission operator-rbac "${operator_subject}" "${verb}" "${resource}" "${namespace}"; done
    done
    for resource in secrets configmaps; do
      for verb in get list watch; do check_permission operator-rbac "${operator_subject}" "${verb}" "${resource}" "${namespace}"; done
    done
  done
fi

agent_subject="system:serviceaccount:${agent_namespace}:${agent_service_account}"
scope_to_array "${discovery_namespaces}"; discovery_scope_values=("${SCOPE_VALUES[@]}")
if [[ "${discovery_namespaces}" == "*" ]]; then discovery_scope_values=("*"); fi
for namespace in "${discovery_scope_values[@]}"; do
  for resource in services endpoints pods ingresses.networking.k8s.io endpointslices.discovery.k8s.io; do
    for verb in get list watch; do check_permission runtime-rbac "${agent_subject}" "${verb}" "${resource}" "${namespace}"; done
  done
  check_permission runtime-rbac "${agent_subject}" get configmaps "${namespace}"
done
check_nonresource_permission runtime-rbac "${agent_subject}" get /metrics
if ((failures > 0)); then finish_failed; fi

cleanup_probe() {
  local current current_facts current_uid current_token delete_options delete_output remaining cleanup_error deadline
  if ! current=$(kubectl_call --namespace "${agent_namespace}" get prometheusagent "${probe_name}" --ignore-not-found --output json 2>&1); then
    record_failure reconcile-cleanup fail ProbeCleanupReadFailed "无法在清理前读取临时探针 ${agent_namespace}/${probe_name}" \
      "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg error "${current}" '{namespace: $namespace, name: $name, error: $error}')"
    return
  fi
  if [[ -z "${current}" ]]; then
    probe_created=false
    record_failure reconcile-cleanup fail ProbeDisappeared "临时探针 ${agent_namespace}/${probe_name} 在清理前已不存在"
    return
  fi
  if ! current_facts=$(jq --compact-output --exit-status '
      if type != "object" then error("probe response must be an object") else
        {uid: (.metadata.uid // ""), token: (.metadata.annotations["monitoring.fjsxy.github.io/preflight-token"] // "")}
      end
    ' <<<"${current}" 2>&1); then
    record_failure reconcile-cleanup fail InvalidProbeCleanupResponse "临时探针清理读取响应无法解析" \
      "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg error "${current_facts}" '{namespace: $namespace, name: $name, error: $error}')"
    return
  fi
  current_uid=$(jq --raw-output '.uid' <<<"${current_facts}")
  current_token=$(jq --raw-output '.token' <<<"${current_facts}")
  if [[ "${current_token}" != "${probe_token}" || ( -n "${probe_uid}" && "${current_uid}" != "${probe_uid}" ) ]]; then
    probe_created=false
    record_failure reconcile-cleanup fail ProbeOwnershipLost "临时探针 ${agent_namespace}/${probe_name} 的 UID 或所有权 token 已变化，拒绝删除" \
      "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg expectedUID "${probe_uid}" --arg actualUID "${current_uid}" '{namespace: $namespace, name: $name, expectedUID: $expectedUID, actualUID: $actualUID}')"
    return
  fi
  probe_uid=${current_uid}
  delete_options=$(jq --null-input --arg uid "${probe_uid}" \
    '{apiVersion: "v1", kind: "DeleteOptions", propagationPolicy: "Foreground", preconditions: {uid: $uid}}')
  if ! delete_output=$(kubectl_call delete \
    --raw "/apis/monitoring.coreos.com/v1alpha1/namespaces/${agent_namespace}/prometheusagents/${probe_name}" \
    --filename - <<<"${delete_options}" 2>&1); then
    record_failure reconcile-cleanup fail ProbeCleanupFailed "临时探针 ${agent_namespace}/${probe_name} 清理失败" \
      "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg uid "${probe_uid}" --arg error "${delete_output}" '{namespace: $namespace, name: $name, uid: $uid, error: $error}')"
    return
  fi
  deadline=$(( $(date +%s) + timeout_seconds ))
  cleanup_error=
  while (( $(date +%s) <= deadline )); do
    if remaining=$(kubectl_call --namespace "${agent_namespace}" get prometheusagent "${probe_name}" --ignore-not-found --output json 2>&1); then
      if [[ -z "${remaining}" ]]; then
        probe_created=false
        break
      fi
      if ! jq --exit-status 'type == "object"' <<<"${remaining}" >/dev/null 2>&1; then
        cleanup_error="PrometheusAgent cleanup response is not valid JSON object"
      fi
    else
      cleanup_error=${remaining}
    fi
    sleep 1
  done
  if [[ "${probe_created}" == true ]]; then
    record_failure reconcile-cleanup fail ProbeCleanupTimedOut "临时探针 ${agent_namespace}/${probe_name} 未在 ${timeout_seconds}s 内完成前台级联清理" \
      "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg uid "${probe_uid}" --arg error "${cleanup_error}" '{namespace: $namespace, name: $name, uid: $uid, lastError: $error}')"
    return
  fi
  record_pass reconcile-cleanup pass ProbeCleaned "仅删除了预检创建的 PrometheusAgent ${agent_namespace}/${probe_name}，其外部生成资源由 ownerReference 前台级联清理" \
    "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg uid "${probe_uid}" '{namespace: $namespace, name: $name, uid: $uid, deletionPropagation: "Foreground", uidPrecondition: true}')"
}

cleanup_on_exit() {
  local original_status=$1 failures_before=${failures}
  trap - EXIT
  if [[ "${probe_created}" == true ]]; then cleanup_probe; fi
  if ((failures > failures_before && original_status == 0)); then original_status=1; fi
  exit "${original_status}"
}

probe_token="$(date +%s)-$$-${RANDOM}${RANDOM}"
probe_name="mih-preflight-${probe_token}"
probe_manifest=$(jq --null-input --arg namespace "${agent_namespace}" --arg serviceAccount "${agent_service_account}" --arg name "${probe_name}" --arg token "${probe_token}" '
  {apiVersion: "monitoring.coreos.com/v1alpha1", kind: "PrometheusAgent",
   metadata: {name: $name, namespace: $namespace, labels: {"app.kubernetes.io/managed-by": "monitor-in-hand-preflight"},
              annotations: {"monitoring.fjsxy.github.io/preflight-token": $token}},
   spec: {version: "v2.55.1", image: "quay.io/prometheus/prometheus:v2.55.1", serviceAccountName: $serviceAccount,
          replicas: 0, shards: 1, replicaExternalLabelName: "", externalLabels: {mih_cluster_id: "monitor-in-hand-preflight"},
          remoteWrite: [{name: "preflight", url: "http://127.0.0.1:9090/api/v1/write"}]}}
')
if ! probe_json=$(kubectl_call create --filename - --output json <<<"${probe_manifest}" 2>&1); then
  record_failure reconcile-probe fail ProbeCreateFailed "无法创建临时 PrometheusAgent reconcile 探针" \
    "$(jq --null-input --arg namespace "${agent_namespace}" --arg error "${probe_json}" '{namespace: $namespace, error: $error}')"
  recovery_deadline=$(( $(date +%s) + timeout_seconds ))
  recovered_probe=
  recovery_error=
  recovery_observed_absent=false
  while (( $(date +%s) <= recovery_deadline )); do
    if recovery_result=$(kubectl_call --namespace "${agent_namespace}" get prometheusagent "${probe_name}" --ignore-not-found --output json 2>&1); then
      if [[ -n "${recovery_result}" ]]; then
        recovered_probe=${recovery_result}
        break
      fi
      recovery_observed_absent=true
    else
      recovery_error=${recovery_result}
    fi
    if (( $(date +%s) >= recovery_deadline )); then break; fi
    sleep 1
  done
  if [[ -n "${recovered_probe}" ]]; then
    if recovered_facts=$(jq --compact-output --exit-status '
        if type != "object" then error("probe response must be an object") else
          {uid: (.metadata.uid // ""), token: (.metadata.annotations["monitoring.fjsxy.github.io/preflight-token"] // "")}
        end
      ' <<<"${recovered_probe}" 2>&1); then
      recovered_token=$(jq --raw-output '.token' <<<"${recovered_facts}")
      probe_uid=$(jq --raw-output '.uid' <<<"${recovered_facts}")
    else
      recovered_token=
      probe_uid=
    fi
    if [[ "${recovered_token}" == "${probe_token}" && -n "${probe_uid}" ]]; then
      probe_created=true
      trap 'cleanup_on_exit $?' EXIT
      cleanup_probe
      trap - EXIT
    else
      record_failure reconcile-cleanup fail ProbeOwnershipLost "创建失败后同名对象缺少预期 UID 或所有权 token，拒绝删除" \
        "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg uid "${probe_uid}" --arg error "${recovered_facts:-}" '{namespace: $namespace, name: $name, uid: $uid, error: $error}')"
    fi
  else
    record_failure reconcile-cleanup fail ProbeCreateOutcomeUnknown "创建失败后未能在 ${timeout_seconds}s 内确定临时探针是否已在服务端创建" \
      "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg error "${recovery_error}" --argjson observedAbsent "${recovery_observed_absent}" '{namespace: $namespace, name: $name, lastError: $error, observedAbsent: $observedAbsent}')"
  fi
  emit_diagnostic error summary fail PreflightFailed "部署预检失败" "$(jq --null-input --argjson failures "${failures}" '{failures: $failures}')"
  exit 1
fi
probe_created=true
trap 'cleanup_on_exit $?' EXIT
if create_facts=$(jq --compact-output --exit-status '
    if type != "object" then error("probe response must be an object") else
      {name: (.metadata.name // ""), uid: (.metadata.uid // "")}
    end
  ' <<<"${probe_json}" 2>&1); then
  response_name=$(jq --raw-output '.name' <<<"${create_facts}")
  probe_uid=$(jq --raw-output '.uid' <<<"${create_facts}")
else
  response_name=
  probe_uid=
fi
if [[ "${response_name}" != "${probe_name}" || -z "${probe_uid}" ]]; then
  record_failure reconcile-probe fail InvalidProbeCreateResponse "临时 PrometheusAgent 创建响应缺少或更改 name/UID" \
    "$(jq --null-input --arg error "${create_facts:-}" '{error: $error}')"
  cleanup_probe
  trap - EXIT
  emit_diagnostic error summary fail PreflightFailed "部署预检失败" "$(jq --null-input --argjson failures "${failures}" '{failures: $failures}')"
  exit 1
fi
probe_statefulset="prom-agent-${probe_name}"

deadline=$(( $(date +%s) + timeout_seconds ))
reconciled=false
reconcile_response_valid=true
last_agent='{}'
last_statefulset='{}'
while (( $(date +%s) <= deadline )); do
  if agent_json=$(kubectl_call --namespace "${agent_namespace}" get prometheusagent "${probe_name}" --output json 2>/dev/null); then
    if ! jq --exit-status 'type == "object"' <<<"${agent_json}" >/dev/null 2>&1; then
      record_failure reconcile-probe fail InvalidReconcileResponse "临时 PrometheusAgent 响应无法解析" \
        "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" '{namespace: $namespace, name: $name, resource: "PrometheusAgent"}')"
      reconcile_response_valid=false
      break
    fi
    last_agent=${agent_json}
  else
    agent_json='{}'
  fi
  if statefulset_json=$(kubectl_call --namespace "${agent_namespace}" get statefulset "${probe_statefulset}" --output json 2>/dev/null); then
    if ! jq --exit-status 'type == "object"' <<<"${statefulset_json}" >/dev/null 2>&1; then
      record_failure reconcile-probe fail InvalidReconcileResponse "临时 StatefulSet 响应无法解析" \
        "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_statefulset}" '{namespace: $namespace, name: $name, resource: "StatefulSet"}')"
      reconcile_response_valid=false
      break
    fi
    last_statefulset=${statefulset_json}
  else
    statefulset_json='{}'
  fi
  condition_ready=$(jq --raw-output '.metadata.generation as $generation | any(.status.conditions[]?; .type == "Reconciled" and .status == "True" and .observedGeneration == $generation)' <<<"${agent_json}")
  statefulset_owned=$(jq --raw-output --arg uid "${probe_uid}" 'any(.metadata.ownerReferences[]?; .apiVersion == "monitoring.coreos.com/v1alpha1" and .kind == "PrometheusAgent" and .uid == $uid)' <<<"${statefulset_json}")
  if [[ "${condition_ready}" == true && "${statefulset_owned}" == true ]]; then reconciled=true; break; fi
  sleep 2
done

if [[ "${reconciled}" == true ]]; then
  record_pass reconcile-probe pass MinimumReconcileSucceeded "Prometheus Operator 已调谐临时 PrometheusAgent 并创建由其 UID 所有的 StatefulSet" \
    "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg uid "${probe_uid}" --arg statefulSet "${probe_statefulset}" '{namespace: $namespace, name: $name, uid: $uid, statefulSet: $statefulSet}')"
elif [[ "${reconcile_response_valid}" == true ]]; then
  record_failure reconcile-probe fail MinimumReconcileTimedOut "Prometheus Operator 未在 ${timeout_seconds}s 内完成最小 reconcile" \
    "$(jq --null-input --arg namespace "${agent_namespace}" --arg name "${probe_name}" --arg uid "${probe_uid}" --arg statefulSet "${probe_statefulset}" --argjson agent "${last_agent}" --argjson statefulSetObject "${last_statefulset}" '{namespace: $namespace, name: $name, uid: $uid, statefulSet: $statefulSet, agentStatus: ($agent.status // {}), statefulSetObserved: ($statefulSetObject.metadata.name // null)}')"
fi

cleanup_probe
trap - EXIT
if ((failures > 0)); then
  emit_diagnostic error summary fail PreflightFailed "部署预检失败" "$(jq --null-input --argjson failures "${failures}" '{failures: $failures}')"
  exit 1
fi
emit_diagnostic info summary pass PreflightPassed "部署预检通过，环境属于一期支持范围" \
  "$(jq --null-input --arg kubernetesVersion "${server_git_version}" --arg agentNamespace "${agent_namespace}" --arg serviceAccount "${agent_service_account}" '{kubernetesVersion: $kubernetesVersion, agentNamespace: $agentNamespace, serviceAccount: $serviceAccount}')"
