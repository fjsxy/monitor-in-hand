#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
helm_bin=${HELM_BIN:-helm}
image=${IMAGE:-docker.io/fjsxy/monitor-in-hand:0.1.0}
cluster_name=${KIND_CLUSTER_NAME:-"monitor-in-hand-smoke-${$}"}
node_image=${KIND_NODE_IMAGE:-kindest/node:v1.28.15@sha256:a7c05c7ae043a0b8c818f5a06188bc2c4098f6cb59ca7d1856df00375d839251}
namespace=monitor-in-hand-smoke
second_namespace=monitor-in-hand-smoke-second
release=monitor-in-hand-smoke
context="kind-${cluster_name}"
port_forward_pid=
cluster_created=false

cleanup() {
  exit_code=$?
  set +e
  if [[ -n "${port_forward_pid}" ]]; then
    kill "${port_forward_pid}" >/dev/null 2>&1
  fi
  if [[ "${cluster_created}" == "true" && "${KEEP_SMOKE_CLUSTER:-0}" != "1" ]]; then
    kind delete cluster --name "${cluster_name}" >/dev/null 2>&1
  elif [[ "${cluster_created}" == "true" ]]; then
    echo "保留 smoke 集群：${cluster_name}"
  fi
  exit "${exit_code}"
}
trap cleanup EXIT

if kind get clusters | grep -Fxq "${cluster_name}"; then
  echo "拒绝复用已有 kind 集群：${cluster_name}" >&2
  exit 1
fi

cluster_created=true
kind create cluster --name "${cluster_name}" --image "${node_image}" --wait 120s
BUILDKIT_PROGRESS=plain docker build --tag "${image}" "${repo_root}"
kind load docker-image --name "${cluster_name}" "${image}"

"${helm_bin}" install "${release}" "${repo_root}/charts/monitor-in-hand" \
  --kube-context "${context}" \
  --namespace "${namespace}" \
  --create-namespace \
  --wait \
  --timeout 120s

kubectl --context "${context}" --namespace "${namespace}" \
  wait deployment "${release}-monitor-in-hand" --for=condition=Available --timeout=120s
kubectl --context "${context}" --namespace "${namespace}" \
  get lease monitor-in-hand.monitoring.fjsxy.github.io >/dev/null

kubectl --context "${context}" --namespace "${namespace}" \
  port-forward deployment/"${release}-monitor-in-hand" 18081:8081 >/tmp/monitor-in-hand-smoke-port-forward.log 2>&1 &
port_forward_pid=$!

probe_ready=false
for _ in $(seq 1 30); do
  if curl --fail --silent http://127.0.0.1:18081/healthz >/dev/null \
    && curl --fail --silent http://127.0.0.1:18081/readyz >/dev/null; then
    probe_ready=true
    break
  fi
  sleep 1
done
if [[ "${probe_ready}" != "true" ]]; then
  echo "健康端点未在期限内就绪" >&2
  exit 1
fi

log_file=/tmp/monitor-in-hand-smoke-manager.log
kubectl --context "${context}" --namespace "${namespace}" \
  logs deployment/"${release}-monitor-in-hand" >"${log_file}"
if ! jq --exit-status --slurp 'length > 0 and all(.[]; type == "object" and has("level") and has("msg"))' \
  <"${log_file}" >/dev/null; then
  echo "Operator 日志不是完整 JSON 流，原始日志：" >&2
  sed -n '1,80p' "${log_file}" >&2
  exit 1
fi

kill "${port_forward_pid}" >/dev/null 2>&1 || true
wait "${port_forward_pid}" 2>/dev/null || true
port_forward_pid=
kubectl --context "${context}" --namespace "${namespace}" \
  rollout restart deployment/"${release}-monitor-in-hand" >/dev/null
kubectl --context "${context}" --namespace "${namespace}" \
  rollout status deployment/"${release}-monitor-in-hand" --timeout=120s >/dev/null

kubectl --context "${context}" create namespace "${second_namespace}" >/dev/null
if "${helm_bin}" install monitor-in-hand-second "${repo_root}/charts/monitor-in-hand" \
  --kube-context "${context}" \
  --namespace "${second_namespace}" >/tmp/monitor-in-hand-second-install.log 2>&1; then
  echo "第二套 Operator 安装不应成功" >&2
  exit 1
fi
if ! grep -q 'monitor-in-hand-manager-role' /tmp/monitor-in-hand-second-install.log; then
  echo "第二套 Operator 安装失败，但原因不是集群级安装锁冲突" >&2
  sed -n '1,80p' /tmp/monitor-in-hand-second-install.log >&2
  exit 1
fi

echo "最小安装 smoke 通过：Deployment、Lease、healthz、readyz、JSON 日志与单实例约束均已验证"
