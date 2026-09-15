#!/usr/bin/env bash
# Dedicated Creator/Pipelines mutation checks.
#
# Each mutation is deliberately small and is expected to make one existing
# security/reliability test fail. The source file is restored and byte-compared
# after every case; the final git diff check prevents a mutation from escaping.
set -euo pipefail

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo"

mutate_and_expect_failure() {
  local name=$1 file=$2 old=$3 new=$4
  shift 4
  local backup log
  backup=$(mktemp)
  log=$(mktemp)
  cp -- "$file" "$backup"

  if ! python3 - "$file" "$old" "$new" <<'PY'
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
old = sys.argv[2]
new = sys.argv[3]
text = path.read_text()
count = text.count(old)
if count != 1:
    raise SystemExit(f"mutation target in {path} matched {count} times, want exactly once")
path.write_text(text.replace(old, new, 1))
PY
  then
    echo "MUTATION TARGET FAILED: $name"
    cp -- "$backup" "$file"
    rm -f -- "$backup" "$log"
    exit 1
  fi

  echo "=== mutation: $name ==="
  if "$@" >"$log" 2>&1; then
    echo "SURVIVED: $name"
    cat "$log"
    cp -- "$backup" "$file"
    cmp -s -- "$backup" "$file"
    rm -f -- "$backup" "$log"
    exit 1
  fi
  echo "CAUGHT: $name"
  cat "$log"
  cp -- "$backup" "$file"
  if ! cmp -s -- "$backup" "$file"; then
    echo "RESTORE FAILED: $name"
    rm -f -- "$backup" "$log"
    exit 1
  fi
  echo "RESTORED BYTE-FOR-BYTE: $name"
  rm -f -- "$backup" "$log"
}

mutate_and_expect_failure \
  "transition validation permits a skipped research-to-review transition" \
  internal/orchestration/pipeline.go \
  $'if stage != StageScript {\n\t\t\treturn ErrInvalidTransition' \
  $'if stage != StageScript && stage != StageReview {\n\t\t\treturn ErrInvalidTransition' \
  go test ./internal/orchestration -run '^TestInvalidAndSkippedTransitions$' -count=1

mutate_and_expect_failure \
  "RBAC valid-role guard is inverted" \
  internal/authz/authz.go \
  'if !rbac.ValidRole(claims.Role) {' \
  'if rbac.ValidRole(claims.Role) {' \
  go test ./internal/authz -run '^TestAuthorizeMeAllRolesGranted$' -count=1

mutate_and_expect_failure \
  "workspace store isolation check is removed" \
  internal/orchestration/pipeline_test.go \
  $'if p.WorkspaceID != workspaceID {\n\t\treturn nil, ErrNotFound' \
  $'if false {\n\t\treturn nil, ErrNotFound' \
  go test ./internal/orchestration -run '^TestWorkspaceIsolation$' -count=1

mutate_and_expect_failure \
  "undocumented arbitrary pipeline PATCH route is added" \
  internal/api/routes.go \
  $'{http.MethodPost, "/pipelines/{id}/retry"},' \
  $'{http.MethodPost, "/pipelines/{id}/retry"},\n\t\t{http.MethodPatch, "/pipelines/{id}"},' \
  go test ./tests -run '^TestOpenAPIMatchesRegisteredRoutes$' -count=1

mutate_and_expect_failure \
  "approval idempotency branch rejects an already-approved pipeline" \
  internal/orchestration/service.go \
  'if p.Status == StatusApproved && p.Approved() {' \
  'if p.Status == StatusApproved && !p.Approved() {' \
  go test ./internal/orchestration -run '^TestApprovalIsIdempotentAndRepublishesEvent$' -count=1

mutate_and_expect_failure \
  "successful worker delivery is negatively acknowledged and requeued" \
  internal/worker/worker.go \
  $'\tmsg.Ack(false)\n\n\tduration := time.Since(start)' \
  $'\tmsg.Nack(false, true)\n\n\tduration := time.Since(start)' \
  go test ./internal/worker -run '^TestProcessMessageSettling$' -count=1

mutate_and_expect_failure \
  "retry count increments by two instead of one" \
  internal/orchestration/service.go \
  'p.RetryCount++' \
  'p.RetryCount += 2' \
  go test ./internal/orchestration -run '^TestFailedStageCanOnlyRecoverThroughRetry$' -count=1

mutate_and_expect_failure \
  "successful login ignores durable audit persistence failure" \
  internal/api/handler.go \
  'if err := h.recordAudit(r, authEvent(u.ID, "auth.login", "success", "issued")); err != nil {' \
  'if false {' \
  go test ./internal/api -run '^TestLoginFailsClosedWhenAuditSinkFails$' -count=1

mutate_and_expect_failure \
  "duplicate worker delivery does not republish the persisted follower event" \
  internal/orchestration/handler.go \
  'if current.Status != StatusFailed && IsAfter(current.Stage, currentStage) {' \
  'if current.Status != StatusFailed && !IsAfter(current.Stage, currentStage) {' \
  go test ./internal/orchestration -run '^TestDuplicateWorkerDeliveryRepairsLostFollowerEvent$' -count=1

git diff --check
git diff --exit-code -- . ':!scripts/creator-pipeline-mutation.sh'
echo "MUTATION_SUITE_PASS: all nine mutations were caught and restored byte-for-byte"
