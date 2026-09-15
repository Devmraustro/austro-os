#!/usr/bin/env bash
# Proves the real browser authorization assertion is sensitive to a critical
# UI authorization regression. The mutation is temporary, the browser test is
# required to fail under it, the exact source is restored, and the API image is
# rebuilt from the restored source before the script returns.
set -euo pipefail

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"

file=internal/webui/static/app.js
old='if (pipeline.status === "awaiting_approval" && currentRole === "workspace_admin") {'
new='if (pipeline.status === "awaiting_approval" && (currentRole === "workspace_admin" || currentRole === "workspace_member")) {'
backup=$(mktemp)
cp -- "$file" "$backup"

restore() {
  cp -- "$backup" "$file"
  if ! cmp -s -- "$backup" "$file"; then
    echo "browser mutation restore failed"
    exit 1
  fi
  docker compose -f docker-compose.yml -f docker-compose.browser-e2e.yml up -d --build api
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

docker compose -f docker-compose.yml -f docker-compose.browser-e2e.yml up -d --build api
for _ in $(seq 1 60); do
  if curl --fail --silent --show-error http://127.0.0.1:8080/health/ready >/dev/null; then
    break
  fi
  sleep 2
done
if ! curl --fail --silent --show-error http://127.0.0.1:8080/health/ready >/dev/null; then
  echo "mutated API did not become ready"
  exit 1
fi

set +e
npm --prefix browser-e2e test -- --reporter=line
status=$?
set -e
if [ "$status" -eq 0 ]; then
  echo "SURVIVED: browser unauthorized approval-control mutation"
  exit 1
fi
echo "CAUGHT: browser unauthorized approval-control mutation"

# Restore now, verify exact equivalence, and rebuild the API before preparing a
# fresh isolated dataset for the non-mutated browser journey.
trap - EXIT
restore
AUSTRO_POSTGRES_DSN="$BROWSER_E2E_OWNER_DSN" go run ./cmd/browser-e2e-setup -mode=cleanup
AUSTRO_POSTGRES_DSN="$BROWSER_E2E_OWNER_DSN" go run ./cmd/browser-e2e-setup

echo "MUTATION_SUITE_PASS: browser authorization mutation was caught and restored"
