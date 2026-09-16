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
  sync
  sleep 1
  if ! cmp -s -- "$backup" "$file"; then
    echo "browser mutation restore failed"
    exit 1
  fi
  if ! git diff --quiet -- "$file"; then
    echo "browser mutation left a tracked source diff after restore"
    git diff -- "$file"
    exit 1
  fi
  if grep -q "workspace_member" "$file"; then
    echo "RESTORE_FILE_CHECK_FAILED: restored source still contains mutated string"
    exit 1
  fi
  echo "RESTORE_SOURCE_PASS: browser authorization source matches HEAD"
  stop_api
  rm -f /tmp/austro-api
  echo "building restored API (bounded)"
  go build -o /tmp/austro-api .
  if [ ! -x /tmp/austro-api ]; then
    echo "RESTORE_BUILD_FAILED: API binary not created"
    exit 1
  fi
  echo "RESTORE_BUILD_PASS: binary created, size $(stat -c%s /tmp/austro-api 2>/dev/null || wc -c < /tmp/austro-api)"
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

# Ensure file system sync after mutation
sync
sleep 1

if ! grep -q "workspace_member" "$file"; then
  echo "MUTATION_FILE_CHECK_FAILED: mutated source does not contain expected string after write"
  exit 1
fi
echo "MUTATED_SOURCE_PASS: mutation applied to $file"

echo "preparing fresh fixture for the temporarily mutated browser test"
stop_api
if [ ! -f "$BROWSER_E2E_ENV_FILE" ]; then
  echo "ENV_FILE_MISSING_BEFORE_CLEANUP: $BROWSER_E2E_ENV_FILE not found"
  ls -lh "$BROWSER_E2E_ENV_FILE" || true
  echo "contents of RUNNER_TEMP:"
  ls -lh "$RUNNER_TEMP" | head -20 || true
fi
echo "running cleanup with env file $BROWSER_E2E_ENV_FILE"
AUSTRO_POSTGRES_DSN="$AUSTRO_POSTGRES_DSN" go run ./cmd/browser-e2e-setup -mode=cleanup
echo "cleanup done, running setup"
AUSTRO_POSTGRES_DSN="$AUSTRO_POSTGRES_DSN" go run ./cmd/browser-e2e-setup
echo "setup done, sourcing env"
set -a
. "$BROWSER_E2E_ENV_FILE"
set +a
echo "env sourced, workspace A: $BROWSER_E2E_WORKSPACE_A"

echo "building and starting temporarily mutated API"
rm -f /tmp/austro-api
go build -o /tmp/austro-api .
if [ ! -x /tmp/austro-api ]; then
  echo "MUTATED_BUILD_FAILED: API binary not created"
  exit 1
fi
echo "MUTATED_BUILD_PASS: binary created, size $(stat -c%s /tmp/austro-api 2>/dev/null || wc -c < /tmp/austro-api)"
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
echo "restore completed, preparing fresh fixture for restored journey"
if [ ! -f "$BROWSER_E2E_ENV_FILE" ]; then
  echo "ENV_FILE_MISSING_BEFORE_FINAL_CLEANUP: $BROWSER_E2E_ENV_FILE"
fi
AUSTRO_POSTGRES_DSN="$AUSTRO_POSTGRES_DSN" go run ./cmd/browser-e2e-setup -mode=cleanup
echo "final cleanup done"
AUSTRO_POSTGRES_DSN="$AUSTRO_POSTGRES_DSN" go run ./cmd/browser-e2e-setup
echo "final setup done"
if [ ! -f "$BROWSER_E2E_ENV_FILE" ]; then
  echo "ENV_FILE_MISSING_AFTER_FINAL_SETUP"
  exit 1
fi
cat "$BROWSER_E2E_ENV_FILE"
echo "MUTATION_SUITE_PASS: browser authorization mutation was caught and restored"
