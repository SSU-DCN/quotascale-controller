#!/usr/bin/env bash
set -euo pipefail

LOCUSTFILE="${LOCUSTFILE:-example/test/locust/locustfile.py}"
PID_DIR="${PID_DIR:-/tmp}"
NODE_IP="${NODE_IP:-}"

A_TRACE="${A_TRACE:-example/test/locust/worldcup_a.csv}"
A_PID_FILE="${A_PID_FILE:-${PID_DIR}/locust-a.pid}"
A_HOST="${A_HOST:-}"
A_LOG="${A_LOG:-/tmp/locust-a.log}"
A_RPS_SCALE="${A_RPS_SCALE:-30.0}"

B_TRACE="${B_TRACE:-example/test/locust/worldcup_b.csv}"
B_PID_FILE="${B_PID_FILE:-${PID_DIR}/locust-b.pid}"
B_HOST="${B_HOST:-}"
B_LOG="${B_LOG:-/tmp/locust-b.log}"
B_RPS_SCALE="${B_RPS_SCALE:-30.0}"

if [[ -z "${A_HOST}" || -z "${B_HOST}" ]]; then
  : "${NODE_IP:?set NODE_IP to a reachable Kubernetes node IP, or set A_HOST/B_HOST explicitly}"
fi

A_HOST="${A_HOST:-http://${NODE_IP}:30080}"
B_HOST="${B_HOST:-http://${NODE_IP}:30081}"

WORLD_CUP_TRACE_CSV="${A_TRACE}" RPS_SCALE="${A_RPS_SCALE}" \
locust -f "${LOCUSTFILE}" --headless --host "${A_HOST}" PodinfoUser \
  >"${A_LOG}" 2>&1 &
A_PID=$!
printf '%s\n' "${A_PID}" >"${A_PID_FILE}"

WORLD_CUP_TRACE_CSV="${B_TRACE}" RPS_SCALE="${B_RPS_SCALE}" \
locust -f "${LOCUSTFILE}" --headless --host "${B_HOST}" HttpbinUser \
  >"${B_LOG}" 2>&1 &
B_PID=$!
printf '%s\n' "${B_PID}" >"${B_PID_FILE}"

echo "Started Locust A with PID ${A_PID} and log ${A_LOG}"
echo "Started Locust B with PID ${B_PID} and log ${B_LOG}"
echo "PID files:"
echo "  A: ${A_PID_FILE}"
echo "  B: ${B_PID_FILE}"
echo "Trace files:"
echo "  A: ${A_TRACE}"
echo "  B: ${B_TRACE}"
echo "Target hosts:"
echo "  A: ${A_HOST}"
echo "  B: ${B_HOST}"
echo
echo "Replay scales:"
echo "  A RPS_SCALE=${A_RPS_SCALE}"
echo "  B RPS_SCALE=${B_RPS_SCALE}"
echo
echo "Stop both replays with:"
echo "  bash example/test/locust/stop_dual_replay.sh"
