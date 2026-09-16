#!/usr/bin/env bash
# Proves the real browser authorization assertion is sensitive to a critical
# UI authorization regression. The mutation is temporary, the browser test is
# required to fail under it, the exact source is restored, and the API binary is
# rebuilt from the restored source before the script returns.
set -euo pipefail

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"

file=internal/webui/static/app.js
old='if (pipeline.status === "awaiting_approval" && currentRole === "workspace_admin") {'
new='if (pipeline.status === "awaiting_approval" && (currentRole === "workspace_admin" || currentRole === "workspace_member")) {'
backup=$(mktemp)
cp -- "$file" "$backup"

stop_api() {
  local pid
  pid=$(cat /tmp/austro-api.pid)
  if ! kill -0 "$pid" 2>/dev/null; then
    return 0
  fi
  kill "$pid"
  for _ in $(seq 1 30); do
    if ! kill -0 "$pid" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  kill -KILL "$pid"
  for _ in $(seq 1 10); do
    if ! kill -0 "$pid" 2>/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "API did not stop after SIGTERM/SIGKILL"
  return 1
}

start_api() {
  nohup /tmp/austro-api > /tmp/austro-api.log 2>&1 &
  echo $! > /tmp/austro-api.pid
  for _ in $(seq 1 60); do
    if curl --fail --silent --show-error http://127.0.0.1:8080/health/ready >/dev/null; then
      return 0
    fi
    sleep 2
  done
  echo "API did not become ready"
  tail -n 120 /tmp/austro-api.log
  return 1
}

restore() {
  echo "restoring browser authorization source"
  cp -- "$backup" "$file"
  if ! cmp -s -- "$backup" "$file"; then
    echo "browser mutation restore failed"
    exit 1
  fi
  echo "RESTORE_SOURCE_PASS: browser authorization source matches HEAD"
  stop_api
  rm -f /tmp/austro-api
  go build -o /tmp/austro-api .
  if [ ! -x /tmp/austro-api ]; then
    echo "RESTORE_BUILD_FAILED: API binary not created"
    exit 1
  fi
  start_api
  echo "RESTORE_API_PASS: API rebuilt from restored source and ready"
  rm -f -- "$backup"
}
trap restore EXIT

python3 - "$file" "$old" "$new" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
old = sys.argv[2]
new = sys.argv[3]
text = path.read_text()
if text.count(old) != 1:
    raise SystemExit("browser authorization mutation target did not match exactly once")
path.write_text(text.replace(old, new, 1))
PY

echo "preparing fresh fixture for the temporarily mutated browser test"
stop_api
AUSTRO_POSTGRES_DSN="$AUSTRO_POSTGRES_DSN" go run ./cmd/browser-e2e-setup -mode=cleanup
AUSTRO_POSTGRES_DSN="$AUSTRO_POSTGRES_DSN" go run ./cmd/browser-e2e-setup
set -a
. "$BROWSER_E2E_ENV_FILE"
set +a

echo "building and starting temporarily mutated API"
rm -f /tmp/austro-api
go build -o /tmp/austro-api .
if [ ! -x /tmp/austro-api ]; then
  echo "MUTATED_BUILD_FAILED: API binary not created"
  exit 1
fi
echo "MUTATED_BUILD_PASS: binary created"
start_api
echo "MUTATED_API_PASS: mutated API ready"

echo "running browser test; failure is required for this mutation"
set +e
npm --prefix browser-e2e test -- --reporter=line > /tmp/browser-e2e-mutation.log 2>&1
status=$?
set -e
cat /tmp/browser-e2e-mutation.log
if [ "$status" -eq 0 ]; then
  echo "SURVIVED: browser unauthorized approval-control mutation"
  exit 1
fi
echo "CAUGHT: browser unauthorized approval-control mutation"

# Restore now, verify exact equivalence, and rebuild the API before preparing a
# fresh isolated dataset for the non-mutated browser journey.
trap - EXIT
restore
AUSTRO_POSTGRES_DSN="$AUSTRO_POSTGRES_DSN" go run ./cmd/browser-e2e-setup -mode=cleanup
AUSTRO_POSTGRES_DSN="$AUSTRO_POSTGRES_DSN" go run ./cmd/browser-e2e-setup

echo "MUTATION_SUITE_PASS: browser authorization mutation was caught and restored"
