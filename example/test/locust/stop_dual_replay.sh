#!/usr/bin/env bash
set -euo pipefail

PID_DIR="${PID_DIR:-/tmp}"
A_PID_FILE="${A_PID_FILE:-${PID_DIR}/locust-a.pid}"
B_PID_FILE="${B_PID_FILE:-${PID_DIR}/locust-b.pid}"

stop_from_pid_file() {
  local label="$1"
  local pid_file="$2"

  if [[ ! -f "${pid_file}" ]]; then
    echo "${label}: pid file not found: ${pid_file}"
    return 0
  fi

  local pid
  pid="$(cat "${pid_file}")"

  if [[ -z "${pid}" ]]; then
    echo "${label}: pid file is empty: ${pid_file}"
    rm -f "${pid_file}"
    return 0
  fi

  if kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}"
    echo "${label}: stopped pid ${pid}"
  else
    echo "${label}: process ${pid} is not running"
  fi

  rm -f "${pid_file}"
}

stop_from_pid_file "Locust A" "${A_PID_FILE}"
stop_from_pid_file "Locust B" "${B_PID_FILE}"
