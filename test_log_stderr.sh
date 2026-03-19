#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

log_arg=${1:-/tmp/persona-test.log}
ts=$(date +%Y%m%d_%H%M%S)
log="${log_arg%.log}-${ts}.log"

go_bin="$(command -v go)"
gocache_dir=${GOCACHE:-/tmp/gocache-persona}

use_script=0
if command -v script >/dev/null 2>&1; then
  use_script=1
fi

log_line() {
  printf '%s\n' "$1" | tee -a "$log"
}

unit_status=0
log_line "== Unit tests: ./cmd/... ./internal/... =="
if ! "$go_bin" test ./cmd/... ./internal/... -v 2>&1 | tee -a "$log"; then
  unit_status=${PIPESTATUS[0]}
fi

integration_status=0
integration_skipped=0
log_line "== Integration tests: ./integration (root required) =="
if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    log_line "SKIP: sudo not found; run as root or install sudo to execute ./integration"
    integration_skipped=1
  elif [[ $use_script -eq 1 ]]; then
    script -q -a "$log" -c "sudo -E env \"PATH=$PATH\" GOCACHE=\"$gocache_dir\" PERSONA_INTEGRATION=1 PERSONA_TEST_VERBOSE=1 \"$go_bin\" test ./integration -run TestPersonaIntegration -count=1 -v" || integration_status=$?
  else
    sudo -E env "PATH=$PATH" GOCACHE="$gocache_dir" PERSONA_INTEGRATION=1 PERSONA_TEST_VERBOSE=1 "$go_bin" test ./integration -run TestPersonaIntegration -count=1 -v 2>&1 | tee -a "$log" || integration_status=${PIPESTATUS[0]}
  fi
else
  if [[ $use_script -eq 1 ]]; then
    script -q -a "$log" -c "env \"PATH=$PATH\" GOCACHE=\"$gocache_dir\" PERSONA_INTEGRATION=1 PERSONA_TEST_VERBOSE=1 \"$go_bin\" test ./integration -run TestPersonaIntegration -count=1 -v" || integration_status=$?
  else
    env "PATH=$PATH" GOCACHE="$gocache_dir" PERSONA_INTEGRATION=1 PERSONA_TEST_VERBOSE=1 "$go_bin" test ./integration -run TestPersonaIntegration -count=1 -v 2>&1 | tee -a "$log" || integration_status=${PIPESTATUS[0]}
  fi
fi

if grep -q "FAIL[[:space:]]\\+persona/integration" "$log"; then
  integration_status=1
elif grep -q "ok[[:space:]]\\+persona/integration" "$log"; then
  integration_status=0
fi

overall=0
if [[ $unit_status -ne 0 || $integration_status -ne 0 ]]; then
  overall=1
fi

unit_label="PASS"
if [[ $unit_status -ne 0 ]]; then
  unit_label="FAIL"
fi
integration_label="PASS"
if [[ $integration_skipped -ne 0 ]]; then
  integration_label="SKIP"
elif [[ $integration_status -ne 0 ]]; then
  integration_label="FAIL"
fi
overall_label="PASS"
if [[ $overall -ne 0 ]]; then
  overall_label="FAIL"
fi

summary="SUMMARY: unit=${unit_label}(${unit_status}) integration=${integration_label}(${integration_status}) overall=${overall_label}(${overall})"
log_line "$summary"
log_line "log: $log"

if [[ ! -s "$log" ]]; then
  rm -f "$log" 2>/dev/null || true
  echo "log: (not created; no output)"
fi

exit $overall
