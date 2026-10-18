#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

go_bin="$(command -v go)"

"$go_bin" test ./cmd/... ./internal/... -v

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  if ! command -v sudo >/dev/null 2>&1; then
    echo "integration required: sudo not found; run in a root mount namespace to execute ./integration" >&2
    exit 1
  fi
  exec sudo -E env "PATH=$PATH" PERSONA_INTEGRATION=1 ${PERSONA_TEST_VERBOSE:+PERSONA_TEST_VERBOSE=$PERSONA_TEST_VERBOSE} \
    "$go_bin" test -count=1 ./integration -v
fi

PERSONA_INTEGRATION=1 "$go_bin" test -count=1 ./integration -v
