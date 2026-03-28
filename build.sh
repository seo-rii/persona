#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

go_version=$(go env GOVERSION 2>/dev/null || true)
if [ -z "$go_version" ]; then
  echo "build failed: unable to determine Go version via 'go env GOVERSION'; install Go 1.25+ and retry." >&2
  exit 1
fi
case "$go_version" in
  go[2-9]*)
    ;;
  go1.*)
    go_minor=${go_version#go1.}
    go_minor=${go_minor%%[^0-9]*}
    if [ -z "$go_minor" ] || [ "$go_minor" -lt 25 ]; then
      echo "build failed: Go 1.25+ is required; found $go_version" >&2
      exit 1
    fi
    ;;
  *)
    echo "build failed: unable to parse Go version '$go_version'; install Go 1.25+ and retry." >&2
    exit 1
    ;;
esac

out_dir=${PERSONA_BUILD_DIR:-./bin}
mkdir -p "$out_dir"

bin="$out_dir/persona"
run_persona() {
  if [ -n "${PERSONA_SETCAP_BIN:-}" ]; then
    PERSONA_SETCAP_BIN="$PERSONA_SETCAP_BIN" "$@"
  else
    "$@"
  fi
}

run_persona_activate_with_sudo() {
  if [ -n "${PERSONA_SETCAP_BIN:-}" ]; then
    sudo env "PERSONA_SETCAP_BIN=$PERSONA_SETCAP_BIN" "$bin" activate --binary "$bin"
  else
    sudo "$bin" activate --binary "$bin"
  fi
}

setcap_status() {
  local line=""
  while IFS= read -r line; do
    case "$line" in
      setcap=*)
        printf '%s\n' "${line#setcap=}"
        return 0
        ;;
    esac
  done <<EOF
$(run_persona "$bin" doctor 2>/dev/null || true)
EOF
  printf 'missing\n'
}

if ! go build -o "$bin" ./cmd/persona; then
  echo "build failed; run 'go mod tidy' manually if dependencies changed, then retry." >&2
  exit 1
fi

setcap_state=$(setcap_status)

if [ "$(id -u)" -eq 0 ]; then
  if [ "$setcap_state" != "missing" ]; then
    if activate_out=$(run_persona "$bin" activate --binary "$bin" 2>&1); then
      printf '%s\n' "$activate_out"
    else
      echo "warning: setcap failed (run with sudo or use $bin activate)" >&2
    fi
  else
    echo "warning: setcap not found (install libcap2-bin) - skipping capabilities" >&2
  fi
else
  if [ "$setcap_state" = "missing" ]; then
    echo "warning: setcap not found (install libcap2-bin) - use $bin activate" >&2
  elif [ -t 0 ] && command -v sudo >/dev/null 2>&1; then
    if activate_out=$(run_persona_activate_with_sudo 2>&1); then
      printf '%s\n' "$activate_out"
    else
      echo "warning: sudo setcap failed (use $bin activate)" >&2
    fi
  else
    echo "warning: setcap requires sudo; run in an interactive shell or use $bin activate" >&2
  fi
fi

echo "note: use $bin activate --allow-dac-override only when patch writes must bypass DAC checks" >&2
echo "built: $bin"
