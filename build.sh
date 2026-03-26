#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

out_dir=${PERSONA_BUILD_DIR:-./bin}
mkdir -p "$out_dir"

bin="$out_dir/persona"

if ! go build -o "$bin" ./cmd/persona; then
  echo "build failed; run 'go mod tidy' manually if dependencies changed, then retry." >&2
  exit 1
fi

default_caps="cap_sys_admin+ep"
setcap_bin=""
for candidate in /usr/sbin/setcap /usr/bin/setcap /sbin/setcap /bin/setcap; do
  if [ -x "$candidate" ] && [ ! -d "$candidate" ]; then
    setcap_bin="$candidate"
    break
  fi
done

if [ "$(id -u)" -eq 0 ]; then
  if [ -n "$setcap_bin" ]; then
    if "$setcap_bin" "$default_caps" "$bin" 2>/dev/null; then
      echo "capabilities set: $bin"
    else
      echo "warning: setcap failed (run with sudo or use $bin activate)" >&2
    fi
  else
    echo "warning: setcap not found (install libcap2-bin) - skipping capabilities" >&2
  fi
else
  if [ -t 0 ] && command -v sudo >/dev/null 2>&1; then
    if [ -n "$setcap_bin" ] && sudo "$setcap_bin" "$default_caps" "$bin"; then
      echo "capabilities set: $bin"
    else
      echo "warning: sudo setcap failed (use $bin activate)" >&2
    fi
  else
    echo "warning: setcap requires sudo; run in an interactive shell or use $bin activate" >&2
  fi
fi

echo "note: use $bin activate --allow-dac-override only when patch writes must bypass DAC checks" >&2
echo "built: $bin"
