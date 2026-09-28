package execution

import (
	"context"

	"github.com/google/uuid"
)

// MaterializedRequest is the sanitized request materialized from the workspace
// before a worker runs. The admission path snapshotted only non-secret context
// into execution_contexts; the worker reconstructs the instruction from those
// fields and never touches raw provider credentials or secrets.
type MaterializedRequest struct {
	TaskTitle       string
	TaskDescription string
	AIEmployeeName  string
	AIEmployeeRole  string
	TeamName        string
	DepartmentName  string
	Capabilities    []string
}

// Materializer reconstructs the request for an execution from the workspace
// database. It is workspace-scoped like every other runtime port.
type Materializer interface {
	Materialize(ctx context.Context, workspaceID, executionID uuid.UUID) (*MaterializedRequest, error)
}
