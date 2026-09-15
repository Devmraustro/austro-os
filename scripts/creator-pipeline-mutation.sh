#!/usr/bin/env bash
# Dedicated Creator/Pipelines mutation checks.
#
# Each mutation is deliberately small and is expected to make one existing
# security/reliability test fail. The source file is restored and byte-compared
# after every case; the final git diff check prevents a mutation from escaping.
# MUTATION_ONLY may select one case for isolated CI execution, or "all".
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
  echo "BEGIN mutation: $name"

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

run_transition() {
  mutate_and_expect_failure \
    "transition validation permits a skipped research-to-review transition" \
    internal/orchestration/pipeline.go \
    $'if stage != StageScript {\n\t\t\treturn ErrInvalidTransition' \
    $'if stage != StageScript && stage != StageReview {\n\t\t\treturn ErrInvalidTransition' \
    go test ./internal/orchestration -run '^TestInvalidAndSkippedTransitions$' -count=1
}

run_rbac() {
  mutate_and_expect_failure \
    "RBAC valid-role guard is inverted" \
    internal/authz/authz.go \
    'if !rbac.ValidRole(claims.Role) {' \
    'if rbac.ValidRole(claims.Role) {' \
    go test ./internal/authz -run '^TestAuthorizeMeAllRolesGranted$' -count=1
}

run_workspace() {
  mutate_and_expect_failure \
    "workspace store isolation check is removed" \
    internal/orchestration/pipeline_test.go \
    $'if p.WorkspaceID != workspaceID {\n\t\treturn nil, ErrNotFound' \
    $'if false {\n\t\treturn nil, ErrNotFound' \
    go test ./internal/orchestration -run '^TestWorkspaceIsolation$' -count=1
}

run_route() {
  mutate_and_expect_failure \
    "undocumented arbitrary pipeline PATCH route is added" \
    internal/api/routes.go \
    $'{http.MethodPost, "/pipelines/{id}/retry"},' \
    $'{http.MethodPost, "/pipelines/{id}/retry"},\n\t\t{http.MethodPatch, "/pipelines/{id}"},' \
    go test ./tests -run '^TestOpenAPIMatchesRegisteredRoutes$' -count=1
}

run_approval() {
  mutate_and_expect_failure \
    "approval idempotency branch rejects an already-approved pipeline" \
    internal/orchestration/service.go \
    'if p.Status == StatusApproved && p.Approved() {' \
    'if p.Status == StatusApproved && !p.Approved() {' \
    go test ./internal/orchestration -run '^TestApprovalIsIdempotentAndRepublishesEvent$' -count=1
}

run_worker_ack() {
  mutate_and_expect_failure \
    "successful worker delivery is negatively acknowledged and requeued" \
    internal/worker/worker.go \
    $'\tmsg.Ack(false)\n\n\tduration := time.Since(start)' \
    $'\tmsg.Nack(false, true)\n\n\tduration := time.Since(start)' \
    go test ./internal/worker -run '^TestProcessMessageSettling$' -count=1
}

run_retry() {
  mutate_and_expect_failure \
    "retry count increments by two instead of one" \
    internal/orchestration/service.go \
    'p.RetryCount++' \
    'p.RetryCount += 2' \
    go test ./internal/orchestration -run '^TestFailedStageCanOnlyRecoverThroughRetry$' -count=1
}

run_audit() {
  mutate_and_expect_failure \
    "successful login ignores durable audit persistence failure" \
    internal/api/handler.go \
    'if err := h.recordAudit(r, authEvent(u.ID, "auth.login", "success", "issued")); err != nil {' \
    'if false {' \
    go test ./internal/api -run '^TestLoginFailsClosedWhenAuditSinkFails$' -count=1
}

run_duplicate() {
  mutate_and_expect_failure \
    "duplicate worker delivery does not republish the persisted follower event" \
    internal/orchestration/handler.go \
    'if current.Status != StatusFailed && IsAfter(current.Stage, currentStage) {' \
    'if current.Status != StatusFailed && !IsAfter(current.Stage, currentStage) {' \
    go test ./internal/orchestration -run '^TestDuplicateWorkerDeliveryRepairsLostFollowerEvent$' -count=1
}

run_one() {
  case "$1" in
    transition) run_transition ;;
    rbac) run_rbac ;;
    workspace) run_workspace ;;
    route) run_route ;;
    approval) run_approval ;;
    worker_ack) run_worker_ack ;;
    retry) run_retry ;;
    audit) run_audit ;;
    duplicate) run_duplicate ;;
    *) echo "unknown mutation selector: $1" >&2; exit 2 ;;
  esac
}

selected=${MUTATION_ONLY:-all}
if [ "$selected" = "all" ]; then
  for mutation in transition rbac workspace route approval worker_ack retry audit duplicate; do
    run_one "$mutation"
  done
else
  run_one "$selected"
fi

# `go test -mod=mod` may materialize missing transitive sums or normalize the
# module graph while compiling the live-test package. Those are test-runner
# artifacts, not mutation output; the committed module files are the intended
# baseline and are restored before the byte-equivalence check.
git checkout -- go.mod go.sum
git diff --check
if ! git diff --exit-code -- . ':!scripts/creator-pipeline-mutation.sh'; then
  echo "FINAL_DIFF_START"
  git diff -- . ':!scripts/creator-pipeline-mutation.sh'
  echo "FINAL_DIFF_END"
  exit 1
fi
echo "MUTATION_SUITE_PASS: $selected mutation case(s) were caught and restored byte-for-byte"
