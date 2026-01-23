#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

go_bin="$(command -v go)"

"$go_bin" test ./internal/... -v

if [[ ${EUID:-$(id -u)} -ne 0 ]]; then
  exec sudo -E env "PATH=$PATH" PERSONA_INTEGRATION=1 ${PERSONA_TEST_VERBOSE:+PERSONA_TEST_VERBOSE=$PERSONA_TEST_VERBOSE} \
    "$go_bin" test ./integration -run TestPersonaIntegration -v
fi

PERSONA_INTEGRATION=1 "$go_bin" test ./integration -run TestPersonaIntegration -v
