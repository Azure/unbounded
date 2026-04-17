#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
FRONTEND_DIR="${REPO_ROOT}/frontend"

POD_SELECTOR='app.kubernetes.io/name=unbounded-net-controller'
REMOTE_PORT='9999'
LOCAL_PORT='19999'
PORT_FORWARD_ADDRESS='127.0.0.1'
PORT_FORWARD_TIMEOUT_SECONDS='20'
PORT_FORWARD_RESTART_DELAY_SECONDS='2'

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl is required but was not found in PATH" >&2
  exit 1
fi

if ! command -v npm >/dev/null 2>&1; then
  echo "npm is required but was not found in PATH" >&2
  exit 1
fi

port_forward_log="$(mktemp)"
cleanup() {
  if [[ -n "${monitor_pid:-}" ]] && kill -0 "${monitor_pid}" >/dev/null 2>&1; then
    kill "${monitor_pid}" >/dev/null 2>&1 || true
    wait "${monitor_pid}" 2>/dev/null || true
  fi
  if [[ -n "${port_forward_pid:-}" ]] && kill -0 "${port_forward_pid}" >/dev/null 2>&1; then
    kill "${port_forward_pid}" >/dev/null 2>&1 || true
    wait "${port_forward_pid}" 2>/dev/null || true
  fi
  rm -f "${port_forward_log}"
}
trap cleanup EXIT INT TERM

resolve_pod_name() {
  local resolved_pod
  resolved_pod="$(kubectl get pod -l "${POD_SELECTOR}" -o name | head -n1)"
  if [[ -z "${resolved_pod}" ]]; then
    return 1
  fi
  printf '%s\n' "${resolved_pod}"
}

start_port_forward() {
  local pod_name
  pod_name="$(resolve_pod_name)" || {
    echo "No controller pod found for selector: ${POD_SELECTOR}" >&2
    return 1
  }

  : >"${port_forward_log}"
  echo "Starting port-forward for ${pod_name} on ${PORT_FORWARD_ADDRESS}:${LOCAL_PORT} -> ${REMOTE_PORT}"
  kubectl port-forward "${pod_name}" "${LOCAL_PORT}:${REMOTE_PORT}" --address "${PORT_FORWARD_ADDRESS}" >"${port_forward_log}" 2>&1 &
  port_forward_pid="$!"

  for _ in $(seq 1 "${PORT_FORWARD_TIMEOUT_SECONDS}"); do
    if ! kill -0 "${port_forward_pid}" >/dev/null 2>&1; then
      echo "kubectl port-forward exited unexpectedly during startup" >&2
      cat "${port_forward_log}" >&2 || true
      return 1
    fi

    if grep -q "Forwarding from ${PORT_FORWARD_ADDRESS}:${LOCAL_PORT}" "${port_forward_log}"; then
      return 0
    fi

    sleep 1
  done

  echo "Timed out waiting for kubectl port-forward to report readiness" >&2
  cat "${port_forward_log}" >&2 || true
  return 1
}

monitor_port_forward() {
  while true; do
    sleep 2
    if kill -0 "${port_forward_pid}" >/dev/null 2>&1; then
      continue
    fi

    echo "kubectl port-forward terminated; attempting restart..." >&2
    cat "${port_forward_log}" >&2 || true

    while ! start_port_forward; do
      echo "Port-forward restart failed; retrying in ${PORT_FORWARD_RESTART_DELAY_SECONDS}s" >&2
      sleep "${PORT_FORWARD_RESTART_DELAY_SECONDS}"
    done
  done
}

while ! start_port_forward; do
  echo "Initial port-forward failed; retrying in ${PORT_FORWARD_RESTART_DELAY_SECONDS}s" >&2
  sleep "${PORT_FORWARD_RESTART_DELAY_SECONDS}"
done

monitor_port_forward &
monitor_pid="$!"

controller_url="http://localhost:${LOCAL_PORT}"
echo "Using VITE_CONTROLLER_URL=${controller_url}"

env VITE_CONTROLLER_URL="${controller_url}" npm --prefix "${FRONTEND_DIR}" run dev
