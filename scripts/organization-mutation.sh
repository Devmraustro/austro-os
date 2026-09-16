#!/usr/bin/env bash
# Organization hierarchy mutation tests: 10 security-critical mutations.
# Each mutation is expected to make one existing security test fail.
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
import pathlib, sys
path = pathlib.Path(sys.argv[1])
old = sys.argv[2]
new = sys.argv[3]
text = path.read_text()
count = text.count(old)
if count != 1:
    raise SystemExit(f"mutation target in {path} matched {count} times, want exactly once (old={old!r})")
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

run_rbac() {
  mutate_and_expect_failure \
    "remove RBAC - valid role guard inverted" \
    internal/authz/authz.go \
    'if !rbac.ValidRole(claims.Role) {' \
    'if rbac.ValidRole(claims.Role) {' \
    go test ./internal/authz -run '^TestAuthorizeMeAllRolesGranted$' -count=1
}

run_team_without_dept() {
  mutate_and_expect_failure \
    "allow Team without Department - nil department check removed" \
    internal/team/service.go \
    'if workspaceID == uuid.Nil || departmentID == uuid.Nil {' \
    'if workspaceID == uuid.Nil && departmentID == uuid.Nil {' \
    go test ./internal/team -run '^TestTeamCreateRequiresDepartment$' -count=1
}

run_employee_without_team() {
  mutate_and_expect_failure \
    "allow AI Employee without Team - nil team check removed" \
    internal/aiemployee/service.go \
    'if workspaceID == uuid.Nil || teamID == uuid.Nil {' \
    'if workspaceID == uuid.Nil && teamID == uuid.Nil {' \
    go test ./internal/aiemployee -run '^TestAIEmployeeCreateRequiresTeam$' -count=1
}

run_workspace_check() {
  mutate_and_expect_failure \
    "remove workspace check - team store allows cross-workspace department" \
    internal/team/service.go \
    $'// Verify department exists in this workspace (prevents cross-workspace parent).\n\tif svc.departmentLookup != nil {' \
    $'// Verify department exists in this workspace (prevents cross-workspace parent).\n\tif false {' \
    go test ./internal/team -run '^TestTeamCreateRejectsCrossWorkspaceParent$' -count=1
}

run_cross_parent() {
  mutate_and_expect_failure \
    "allow cross-workspace parent - employee team resolver skipped" \
    internal/aiemployee/service.go \
    $'var deptID uuid.UUID\n\tif svc.teamLookup != nil {' \
    $'var deptID uuid.UUID\n\tif false {' \
    go test ./internal/aiemployee -run '^TestAIEmployeeCreateRejectsCrossWorkspaceTeam$' -count=1
}

run_unauthorized_move() {
  mutate_and_expect_failure \
    "allow unauthorized move - team update skips department lookup" \
    internal/team/service.go \
    $'if *departmentID == uuid.Nil {\n\t\t\treturn nil, ErrInvalidInput\n\t\t}\n\t\tif svc.departmentLookup != nil {' \
    $'if *departmentID == uuid.Nil {\n\t\t\treturn nil, ErrInvalidInput\n\t\t}\n\t\tif false {' \
    go test ./internal/team -run '^TestTeamUpdateRejectsCrossWorkspaceMove$' -count=1
}

run_bypass_uniqueness() {
  mutate_and_expect_failure \
    "bypass uniqueness - department blank name allowed" \
    internal/department/service.go \
    $'func (svc *Service) Create(ctx context.Context, workspaceID uuid.UUID, name string) (*Department, error) {\n\tname = strings.TrimSpace(name)\n\tif name == "" {' \
    $'func (svc *Service) Create(ctx context.Context, workspaceID uuid.UUID, name string) (*Department, error) {\n\tname = strings.TrimSpace(name)\n\tif false {' \
    go test ./internal/department -run '^TestDepartmentCreateRequiresName$' -count=1
}

run_remove_audit() {
  mutate_and_expect_failure \
    "remove audit - department handler skips audit sink" \
    internal/api/department_handlers.go \
    $'func (h *DepartmentHandler) record(r *http.Request, claims *auth.Claims, action string, deptID, ws uuid.UUID, outcome, detail string) {\n\tif h.auditSink == nil {' \
    $'func (h *DepartmentHandler) record(r *http.Request, claims *auth.Claims, action string, deptID, ws uuid.UUID, outcome, detail string) {\n\tif true {' \
    go test ./internal/api -run '^TestDepartmentCreateIsAudited$' -count=1
}

run_undocumented_route() {
  mutate_and_expect_failure \
    "undocumented route - arbitrary department approve route added" \
    internal/api/routes.go \
    '{http.MethodDelete, "/departments/{id}"},' \
    $'{http.MethodDelete, "/departments/{id}"},\n\t\t{http.MethodPatch, "/departments/{id}/approve"},' \
    go test ./tests -run '^TestOpenAPIMatchesRegisteredRoutes$' -count=1
}

run_browser_control() {
  mutate_and_expect_failure \
    "forbidden browser control - pipeline approve visible to member" \
    internal/webui/static/app.js \
    'if (pipeline.status === "awaiting_approval" && currentRole === "workspace_admin") {' \
    'if (pipeline.status === "awaiting_approval" && (currentRole === "workspace_admin" || currentRole === "workspace_member")) {' \
    go test ./internal/webui -run '^TestPipelineApproveIsAdminOnly$' -count=1
}

run_one() {
  case "$1" in
    rbac) run_rbac ;;
    team_without_dept) run_team_without_dept ;;
    employee_without_team) run_employee_without_team ;;
    workspace) run_workspace_check ;;
    cross_parent) run_cross_parent ;;
    unauthorized_move) run_unauthorized_move ;;
    uniqueness) run_bypass_uniqueness ;;
    audit) run_remove_audit ;;
    route) run_undocumented_route ;;
    browser) run_browser_control ;;
    *) echo "unknown mutation selector: $1" >&2; exit 2 ;;
  esac
}

selected=${MUTATION_ONLY:-all}
if [ "$selected" = "all" ]; then
  for mutation in rbac team_without_dept employee_without_team workspace cross_parent unauthorized_move uniqueness audit route browser; do
    run_one "$mutation"
  done
else
  run_one "$selected"
fi

git checkout -- go.mod go.sum
git diff --check
if ! git diff --exit-code -- . ':!scripts/organization-mutation.sh' ':!scripts/creator-pipeline-mutation.sh' ':!scripts/browser-e2e-mutation.sh'; then
  echo "FINAL_DIFF_START"
  git diff -- . ':!scripts/organization-mutation.sh' ':!scripts/creator-pipeline-mutation.sh' ':!scripts/browser-e2e-mutation.sh'
  echo "FINAL_DIFF_END"
  exit 1
fi
echo "MUTATION_SUITE_PASS: $selected organization mutation case(s) were caught and restored byte-for-byte"
