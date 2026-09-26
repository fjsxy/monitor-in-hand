#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
kubernetes_version=${KUBERNETES_VERSION:-}
test_run_id=${TEST_RUN_ID:-}
helm_bin=${HELM_BIN:-helm}
kubectl_bin=${KUBECTL:-kubectl}
kind_bin=${KIND:-kind}
image=docker.io/fjsxy/monitor-in-hand:0.1.0
cluster_name="mih-e2e-${kubernetes_version//./}-${test_run_id}"
context="kind-${cluster_name}"
operator_namespace=monitor-in-hand-system
target_namespace=monitor-in-hand-test
release=monitor-in-hand
instance=default-deployment
resource_name=ksm-default-deployment
port_forward_pid=

cleanup() {
  if [[ -n "${port_forward_pid}" ]]; then
    kill "${port_forward_pid}" >/dev/null 2>&1 || true
    wait "${port_forward_pid}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

case "${kubernetes_version}" in
  1.19)
    scenario_id=E2E-119-KSM-001
    expected_version=2.3.0
    expected_image=k8s.gcr.io/kube-state-metrics/kube-state-metrics:v2.3.0
    expected_rules='[
      {"apiGroups":[""],"resources":["configmaps","secrets","nodes","pods","services","resourcequotas","replicationcontrollers","limitranges","persistentvolumeclaims","persistentvolumes","namespaces","endpoints"],"verbs":["list","watch"]},
      {"apiGroups":["apps"],"resources":["statefulsets","daemonsets","deployments","replicasets"],"verbs":["list","watch"]},
      {"apiGroups":["batch"],"resources":["cronjobs","jobs"],"verbs":["list","watch"]},
      {"apiGroups":["autoscaling"],"resources":["horizontalpodautoscalers"],"verbs":["list","watch"]},
      {"apiGroups":["authentication.k8s.io"],"resources":["tokenreviews"],"verbs":["create"]},
      {"apiGroups":["authorization.k8s.io"],"resources":["subjectaccessreviews"],"verbs":["create"]},
      {"apiGroups":["policy"],"resources":["poddisruptionbudgets"],"verbs":["list","watch"]},
      {"apiGroups":["certificates.k8s.io"],"resources":["certificatesigningrequests"],"verbs":["list","watch"]},
      {"apiGroups":["storage.k8s.io"],"resources":["storageclasses","volumeattachments"],"verbs":["list","watch"]},
      {"apiGroups":["admissionregistration.k8s.io"],"resources":["mutatingwebhookconfigurations","validatingwebhookconfigurations"],"verbs":["list","watch"]},
      {"apiGroups":["networking.k8s.io"],"resources":["networkpolicies","ingresses"],"verbs":["list","watch"]},
      {"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["list","watch"]}
    ]'
    ;;
  1.28)
    scenario_id=E2E-128-KSM-001
    expected_version=2.11.0
    expected_image=registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.11.0
    expected_rules='[
      {"apiGroups":[""],"resources":["configmaps","secrets","nodes","pods","services","serviceaccounts","resourcequotas","replicationcontrollers","limitranges","persistentvolumeclaims","persistentvolumes","namespaces","endpoints"],"verbs":["list","watch"]},
      {"apiGroups":["apps"],"resources":["statefulsets","daemonsets","deployments","replicasets"],"verbs":["list","watch"]},
      {"apiGroups":["batch"],"resources":["cronjobs","jobs"],"verbs":["list","watch"]},
      {"apiGroups":["autoscaling"],"resources":["horizontalpodautoscalers"],"verbs":["list","watch"]},
      {"apiGroups":["authentication.k8s.io"],"resources":["tokenreviews"],"verbs":["create"]},
      {"apiGroups":["authorization.k8s.io"],"resources":["subjectaccessreviews"],"verbs":["create"]},
      {"apiGroups":["policy"],"resources":["poddisruptionbudgets"],"verbs":["list","watch"]},
      {"apiGroups":["certificates.k8s.io"],"resources":["certificatesigningrequests"],"verbs":["list","watch"]},
      {"apiGroups":["storage.k8s.io"],"resources":["storageclasses","volumeattachments"],"verbs":["list","watch"]},
      {"apiGroups":["admissionregistration.k8s.io"],"resources":["mutatingwebhookconfigurations","validatingwebhookconfigurations"],"verbs":["list","watch"]},
      {"apiGroups":["networking.k8s.io"],"resources":["networkpolicies","ingressclasses","ingresses"],"verbs":["list","watch"]},
      {"apiGroups":["coordination.k8s.io"],"resources":["leases"],"verbs":["list","watch"]},
      {"apiGroups":["discovery.k8s.io"],"resources":["endpointslices"],"verbs":["list","watch"]},
      {"apiGroups":["rbac.authorization.k8s.io"],"resources":["clusterrolebindings","clusterroles","rolebindings","roles"],"verbs":["list","watch"]}
    ]'
    ;;
  *)
    echo "KUBERNETES_VERSION 必须是 1.19 或 1.28" >&2
    exit 2
    ;;
esac

if [[ ! "${test_run_id}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
  echo "TEST_RUN_ID 必须是非空 DNS label" >&2
  exit 2
fi

wait_for_json() {
  local resource=$1
  local namespace=$2
  local expression=$3
  local description=$4
  local output

  for _ in $(seq 1 150); do
    if [[ -n "${namespace}" ]]; then
      output=$(${kubectl_bin} --context "${context}" --namespace "${namespace}" get "${resource}" --output json 2>/dev/null || true)
    else
      output=$(${kubectl_bin} --context "${context}" get "${resource}" --output json 2>/dev/null || true)
    fi
    if [[ -n "${output}" ]] && jq --exit-status "${expression}" <<<"${output}" >/dev/null; then
      return 0
    fi
    sleep 2
  done
  echo "${scenario_id} 超时：${description}" >&2
  return 1
}

BUILDKIT_PROGRESS=plain docker build --tag "${image}" "${repo_root}"
"${kind_bin}" load docker-image --name "${cluster_name}" "${image}"
"${kubectl_bin}" --context "${context}" create namespace "${operator_namespace}"
"${helm_bin}" upgrade --install "${release}" "${repo_root}/charts/monitor-in-hand" \
  --kube-context "${context}" \
  --namespace "${operator_namespace}" \
  --wait \
  --timeout 180s

"${kubectl_bin}" --context "${context}" apply --filename - <<EOF
apiVersion: monitoring.fjsxy.github.io/v1alpha1
kind: KubeStateMetrics
metadata:
  name: ${instance}
spec:
  targetNamespace: ${target_namespace}
EOF

wait_for_json "deployment/${resource_name}" "${target_namespace}" \
  '.status.availableReplicas == 1' "受管 Deployment 未达到 availableReplicas=1"
wait_for_json "kubestatemetrics/${instance}" "" \
  '.status.phase == "Ready" and .status.observedGeneration == .metadata.generation and .status.availablePods == 1' \
  "KubeStateMetrics status 未达到 Ready"

instance_uid=$(${kubectl_bin} --context "${context}" get "kubestatemetrics/${instance}" --output jsonpath='{.metadata.uid}')

"${kubectl_bin}" --context "${context}" get "kubestatemetrics/${instance}" --output json \
  | jq --exit-status \
      --arg namespace "${target_namespace}" \
      --arg version "${expected_version}" '
        .metadata.generation as $generation |
        .spec.enabled == true and
        .spec.suspended == false and
        .spec.deployment.mode == "Deployment" and
        .status.appliedTargetNamespace == $namespace and
        .status.kubeStateMetricsVersion == $version and
        .status.deploymentMode == "Deployment" and
        .status.desiredPods == 1 and
        .status.availablePods == 1 and
        (.status.managedResources | length == 5) and
        (.status.conditions | any(.type == "Ready" and .status == "True" and .observedGeneration == $generation)) and
        (.status.conditions | any(.type == "Available" and .status == "True" and .observedGeneration == $generation))
      ' >/dev/null

"${kubectl_bin}" --context "${context}" --namespace "${target_namespace}" get "deployment/${resource_name}" --output json \
  | jq --exit-status \
      --arg image "${expected_image}" '
        .spec.replicas == 1 and
        .spec.template.spec.serviceAccountName == "ksm-default-deployment" and
        .spec.template.spec.nodeSelector["kubernetes.io/os"] == "linux" and
        (.spec.template.spec.containers | length == 1) and
        (.spec.template.spec.containers[0] |
          .image == $image and
          ((.args // []) | length == 0) and
          (.ports | any(.name == "http-metrics" and .containerPort == 8080)) and
          (.ports | any(.name == "telemetry" and .containerPort == 8081)) and
          .livenessProbe.httpGet.path == "/healthz" and
          .livenessProbe.httpGet.port == "http-metrics" and
          .livenessProbe.httpGet.scheme == "HTTP" and
          .livenessProbe.initialDelaySeconds == 5 and
          .livenessProbe.timeoutSeconds == 5 and
          .livenessProbe.periodSeconds == 10 and
          .livenessProbe.successThreshold == 1 and
          .livenessProbe.failureThreshold == 3 and
          .readinessProbe.httpGet.path == "/" and
          .readinessProbe.httpGet.port == "telemetry" and
          .readinessProbe.httpGet.scheme == "HTTP" and
          .readinessProbe.initialDelaySeconds == 5 and
          .readinessProbe.timeoutSeconds == 5 and
          .readinessProbe.periodSeconds == 10 and
          .readinessProbe.successThreshold == 1 and
          .readinessProbe.failureThreshold == 3 and
          .terminationMessagePath == "/dev/termination-log" and
          .terminationMessagePolicy == "File" and
          .securityContext.runAsUser == 65534)
      ' >/dev/null

"${kubectl_bin}" --context "${context}" --namespace "${target_namespace}" get "deployment/${resource_name}" --output json \
  | jq --exit-status --arg version "${kubernetes_version}" '
      if $version == "1.28" then
        .spec.template.spec.automountServiceAccountToken == true and
        (.spec.template.spec.containers[0].securityContext |
          .allowPrivilegeEscalation == false and
          .readOnlyRootFilesystem == true and
          .runAsNonRoot == true and
          .capabilities.drop == ["ALL"] and
          .seccompProfile.type == "RuntimeDefault")
      else
        (.spec.template.spec | has("automountServiceAccountToken") | not) and
        (.spec.template.spec.containers[0].securityContext | keys == ["runAsUser"])
      end
    ' >/dev/null

"${kubectl_bin}" --context "${context}" --namespace "${target_namespace}" get "serviceaccount/${resource_name}" --output json \
  | jq --exit-status --arg version "${kubernetes_version}" '
      if $version == "1.28" then .automountServiceAccountToken == false
      else (has("automountServiceAccountToken") | not) end
    ' >/dev/null

"${kubectl_bin}" --context "${context}" --namespace "${target_namespace}" get "service/${resource_name}" --output json \
  | jq --exit-status '
      .spec.clusterIP == "None" and
      (.spec.ports | length == 2) and
      (.spec.ports | any(.name == "http-metrics" and .port == 8080 and .targetPort == "http-metrics")) and
      (.spec.ports | any(.name == "telemetry" and .port == 8081 and .targetPort == "telemetry")) and
      .spec.selector["app.kubernetes.io/name"] == "kube-state-metrics" and
      .spec.selector["app.kubernetes.io/instance"] == "default-deployment" and
      .spec.selector["monitor-in-hand/endpoint-group"] == "default"
    ' >/dev/null

for object in \
  "deployment.apps/${resource_name}" \
  "service/${resource_name}" \
  "serviceaccount/${resource_name}"; do
  "${kubectl_bin}" --context "${context}" --namespace "${target_namespace}" get "${object}" --output json \
    | jq --exit-status --arg uid "${instance_uid}" --arg version "${expected_version}" '
        .metadata.labels["app.kubernetes.io/name"] == "kube-state-metrics" and
        .metadata.labels["app.kubernetes.io/instance"] == "default-deployment" and
        .metadata.labels["app.kubernetes.io/managed-by"] == "monitor-in-hand" and
        .metadata.labels["app.kubernetes.io/version"] == $version and
        (.metadata.ownerReferences | length == 1) and
        .metadata.ownerReferences[0].uid == $uid and
        .metadata.ownerReferences[0].controller == true
      ' >/dev/null
done

for object in "clusterrole/${resource_name}" "clusterrolebinding/${resource_name}"; do
  "${kubectl_bin}" --context "${context}" get "${object}" --output json \
    | jq --exit-status --arg uid "${instance_uid}" --arg version "${expected_version}" '
        .metadata.labels["app.kubernetes.io/name"] == "kube-state-metrics" and
        .metadata.labels["app.kubernetes.io/instance"] == "default-deployment" and
        .metadata.labels["app.kubernetes.io/managed-by"] == "monitor-in-hand" and
        .metadata.labels["app.kubernetes.io/version"] == $version and
        (.metadata.ownerReferences | length == 1) and
        .metadata.ownerReferences[0].uid == $uid and
        .metadata.ownerReferences[0].controller == true
      ' >/dev/null
done

"${kubectl_bin}" --context "${context}" get "clusterrole/${resource_name}" --output json \
  | jq --exit-status --argjson expected "${expected_rules}" '
      def normalized:
        map({apiGroups: (.apiGroups | sort), resources: (.resources | sort), verbs: (.verbs | sort)}) |
        sort_by((.apiGroups | join(",")), (.resources | join(",")), (.verbs | join(",")));
      (.rules | normalized) == ($expected | normalized)
    ' >/dev/null

"${kubectl_bin}" --context "${context}" get "clusterrolebinding/${resource_name}" --output json \
  | jq --exit-status --arg namespace "${target_namespace}" '
      .roleRef == {apiGroup:"rbac.authorization.k8s.io",kind:"ClusterRole",name:"ksm-default-deployment"} and
      .subjects == [{kind:"ServiceAccount",name:"ksm-default-deployment",namespace:$namespace}]
    ' >/dev/null

"${kubectl_bin}" --context "${context}" --namespace "${target_namespace}" \
  port-forward "service/${resource_name}" 18082:8080 >/tmp/monitor-in-hand-ksm-port-forward.log 2>&1 &
port_forward_pid=$!
metrics_available=false
for _ in $(seq 1 60); do
  metrics=$(curl --fail --silent http://127.0.0.1:18082/metrics 2>/dev/null || true)
  if grep -q '^kube_pod_info' <<<"${metrics}"; then
    metrics_available=true
    break
  fi
  sleep 1
done
if [[ "${metrics_available}" != "true" ]]; then
  echo "${scenario_id} 受管实例未暴露 kube_pod_info" >&2
  sed -n '1,80p' /tmp/monitor-in-hand-ksm-port-forward.log >&2 || true
  exit 1
fi

echo "${scenario_id} PASS: Kubernetes ${kubernetes_version} 使用 kube-state-metrics ${expected_version} 运行默认单副本 Deployment"
