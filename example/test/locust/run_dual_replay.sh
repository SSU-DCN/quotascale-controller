#!/usr/bin/env bash
set -euo pipefail

LOCUSTFILE="${LOCUSTFILE:-example/test/locust/locustfile.py}"

A_TRACE="${A_TRACE:-example/test/locust/worldcup_a.csv}"
A_HOST="${A_HOST:-http://quota-test-app-a.quota-test-a.svc.cluster.local}"
A_LOG="${A_LOG:-/tmp/locust-a.log}"
A_RPS_SCALE="${A_RPS_SCALE:-6.0}"

B_TRACE="${B_TRACE:-example/test/locust/worldcup_b.csv}"
B_HOST="${B_HOST:-http://quota-test-app-b.quota-test-b.svc.cluster.local}"
B_LOG="${B_LOG:-/tmp/locust-b.log}"
B_RPS_SCALE="${B_RPS_SCALE:-6.0}"

WORLD_CUP_TRACE_CSV="${A_TRACE}" RPS_SCALE="${A_RPS_SCALE}" \
locust -f "${LOCUSTFILE}" --headless --host "${A_HOST}" PodinfoUser \
  >"${A_LOG}" 2>&1 &
A_PID=$!

WORLD_CUP_TRACE_CSV="${B_TRACE}" RPS_SCALE="${B_RPS_SCALE}" \
locust -f "${LOCUSTFILE}" --headless --host "${B_HOST}" HttpbinUser \
  >"${B_LOG}" 2>&1 &
B_PID=$!

echo "Started Locust A with PID ${A_PID} and log ${A_LOG}"
echo "Started Locust B with PID ${B_PID} and log ${B_LOG}"
echo "Trace files:"
echo "  A: ${A_TRACE}"
echo "  B: ${B_TRACE}"
echo
echo "Replay scales:"
echo "  A RPS_SCALE=${A_RPS_SCALE}"
echo "  B RPS_SCALE=${B_RPS_SCALE}"
