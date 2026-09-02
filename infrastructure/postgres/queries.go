package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"austro-os/internal/config"
	"austro-os/internal/repository"

	"github.com/google/uuid"
)

// GENESIS_HASH is the fixed hash value used to anchor the first event in the
// audit hash chain (i==0 case). Subsequent events chain from the previous
// event's hash_value.
const GENESIS_HASH = "0000000000000000000000000000000000000000000000000000000000000000"

// genAuditID returns a fresh random identifier, falling back to a UUID on
// crypto/rand failure.
func genAuditID() uuid.UUID {
	return uuid.New()
}

// hashChainInput builds the canonical string that is hashed to link a child
// event to its parent, consistent with internal/audit.
func hashChainInput(prevHash []byte, prevEventID uuid.UUID, prevTimestamp time.Time, prevGenesis bool, eventType, actorType string, actorID uuid.UUID, targetType string, targetID uuid.UUID) string {
	return fmt.Sprintf("%s|%s|%s|%v|%s|%v|%s|%s|%s",
		prevHash,
		prevEventID,
		prevTimestamp.UTC().Format("2006-01-02T15:04:05Z07:00"),
		prevGenesis,
		eventType,
		actorType,
		actorID,
		targetType,
		targetID,
	)
}

// getPreviousEvent loads the prior audit event by its ID for hash chaining.
func getPreviousEvent(ctx context.Context, db *sql.DB, id uuid.UUID) (*repository.AuditEvent, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, event_id, timestamp, trace_id, span_id, actor_type, actor_id,
		       target_type, target_id, event_type, outcome, constitutional_principle,
		       hash_chain_parent, hash_chain_value, genesis
		FROM audit_events WHERE id = $1`, id)

	var e repository.AuditEvent
	var hashValue []byte
	var traceID, spanID, actorID, targetID, hashParent, eventID, evID uuid.UUID
	var timestamp time.Time
	var genesis bool

	err := row.Scan(&evID, &eventID, &timestamp, &traceID, &spanID, &e.ActorType, &actorID,
		&e.TargetType, &targetID, &e.EventType, &e.Outcome, &e.ConstitutionalPrinciple,
		&hashParent, &hashValue, &genesis)
	if err != nil {
		return nil, err
	}
	e.ID = evID
	e.EventID = eventID
	e.Timestamp = timestamp
	e.TraceID = traceID
	e.SpanID = spanID
	e.ActorID = actorID
	e.TargetID = targetID
	e.HashParent = hashParent
	e.HashValue = hashValue
	e.Genesis = genesis
	return &e, nil
}

// VerifyHashChain verifies the integrity of an ordered sequence of audit events.
func VerifyHashChain(events []*repository.AuditEvent) bool {
	if len(events) == 0 {
		return true
	}
	for i, event := range events {
		if i == 0 {
			if !event.Genesis {
				return false
			}
			expected := sha256.Sum256([]byte(GENESIS_HASH + "." + event.Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00")))
			if !hmac.Equal(event.HashValue, expected[:]) {
				return false
			}
			continue
		}
		prev := events[i-1]
		input := hashChainInput(prev.HashValue, prev.EventID, prev.Timestamp, prev.Genesis,
			event.EventType, event.ActorType, event.ActorID, event.TargetType, event.TargetID)
		expected := sha256.Sum256([]byte(input))
		if !hmac.Equal(event.HashValue, expected[:]) {
			return false
		}
	}
	return true
}

// Workspace queries
func (db *SQLDB) CreateWorkspace(ctx context.Context, name string) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRowContext(ctx, "INSERT INTO workspaces (name) VALUES ($1) RETURNING id", name).Scan(&id)
	return id, err
}

func (db *SQLDB) GetWorkspace(ctx context.Context, id uuid.UUID) (*repository.WorkspaceRecord, error) {
	row := db.QueryRowContext(ctx, "SELECT id, name, created_at FROM workspaces WHERE id = $1", id)
	var r repository.WorkspaceRecord
	err := row.Scan(&r.ID, &r.Name, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *SQLDB) ListWorkspaces(ctx context.Context) ([]*repository.WorkspaceRecord, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, name, created_at FROM workspaces")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*repository.WorkspaceRecord
	for rows.Next() {
		var r repository.WorkspaceRecord
		if err := rows.Scan(&r.ID, &r.Name, &r.CreatedAt); err != nil {
			return nil, err
		}
		results = append(results, &r)
	}
	return results, nil
}

// Department queries
func (db *SQLDB) CreateDepartment(ctx context.Context, name string, workspaceID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRowContext(ctx, "INSERT INTO departments (name, workspace_id) VALUES ($1, $2) RETURNING id", name, workspaceID).Scan(&id)
	return id, err
}

func (db *SQLDB) GetDepartment(ctx context.Context, id uuid.UUID) (*repository.DepartmentRecord, error) {
	row := db.QueryRowContext(ctx, "SELECT id, name, workspace_id, created_at FROM departments WHERE id = $1", id)
	var r repository.DepartmentRecord
	err := row.Scan(&r.ID, &r.Name, &r.WorkspaceID, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *SQLDB) ListDepartmentsByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]*repository.DepartmentRecord, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, name, workspace_id, created_at FROM departments WHERE workspace_id = $1", workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*repository.DepartmentRecord
	for rows.Next() {
		var r repository.DepartmentRecord
		if err := rows.Scan(&r.ID, &r.Name, &r.WorkspaceID, &r.CreatedAt); err != nil {
			return nil, err
		}
		results = append(results, &r)
	}
	return results, nil
}

// Team queries
func (db *SQLDB) CreateTeam(ctx context.Context, name string, departmentID uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRowContext(ctx, "INSERT INTO teams (name, department_id) VALUES ($1, $2) RETURNING id", name, departmentID).Scan(&id)
	return id, err
}

func (db *SQLDB) GetTeam(ctx context.Context, id uuid.UUID) (*repository.TeamRecord, error) {
	row := db.QueryRowContext(ctx, "SELECT id, name, department_id, created_at FROM teams WHERE id = $1", id)
	var r repository.TeamRecord
	err := row.Scan(&r.ID, &r.Name, &r.DepartmentID, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (db *SQLDB) ListTeamsByDepartment(ctx context.Context, departmentID uuid.UUID) ([]*repository.TeamRecord, error) {
	rows, err := db.QueryContext(ctx, "SELECT id, name, department_id, created_at FROM teams WHERE department_id = $1", departmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*repository.TeamRecord
	for rows.Next() {
		var r repository.TeamRecord
		if err := rows.Scan(&r.ID, &r.Name, &r.DepartmentID, &r.CreatedAt); err != nil {
			return nil, err
		}
		results = append(results, &r)
	}
	return results, nil
}

// AI Employee queries
func (db *SQLDB) CreateAIEmployee(ctx context.Context, name, role string, teamID uuid.UUID) (uuid.UUID, error) {
	// Validate invariants first - team -> department -> workspace
	var depID uuid.UUID
	var wsID uuid.UUID
	err := db.QueryRowContext(ctx, `
		SELECT d.id, d.workspace_id FROM departments d
		JOIN teams t ON t.department_id = d.id
		WHERE t.id = $1
	`, teamID).Scan(&depID, &wsID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("team not found or workspace mismatch: %w", err)
	}

	id := genAuditID()
	_, err = db.ExecContext(ctx, `
		INSERT INTO ai_employees (id, name, role, team_id, department_id, workspace_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, id, name, role, teamID, depID, wsID)
	return id, err
}

func (db *SQLDB) GetAIEmployee(ctx context.Context, id uuid.UUID) (*repository.AIEmployeeRecord, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, name, role, team_id, department_id, workspace_id, capabilities,
		       permissions, memory_id, current_task_id, created_at, updated_at
		FROM ai_employees WHERE id = $1
	`, id)
	var r repository.AIEmployeeRecord
	var capabilities []string
	var permissions map[string]bool

	err := row.Scan(&r.ID, &r.Name, &r.Role, &r.TeamID, &r.DepartmentID, &r.WorkspaceID,
		&capabilities, &permissions, &r.MemoryID, &r.CurrentTaskID, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	r.Capabilities = capabilities
	r.Permissions = permissions
	return &r, nil
}

func (db *SQLDB) ListAIEmployeesByTeam(ctx context.Context, teamID uuid.UUID) ([]*repository.AIEmployeeRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, name, role, team_id, department_id, workspace_id, capabilities,
		       permissions, memory_id, current_task_id, created_at, updated_at
		FROM ai_employees WHERE team_id = $1`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*repository.AIEmployeeRecord
	for rows.Next() {
		var r repository.AIEmployeeRecord
		var capabilities []string
		var permissions map[string]bool
		if err := rows.Scan(&r.ID, &r.Name, &r.Role, &r.TeamID, &r.DepartmentID, &r.WorkspaceID,
			&capabilities, &permissions, &r.MemoryID, &r.CurrentTaskID, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Capabilities = capabilities
		r.Permissions = permissions
		results = append(results, &r)
	}
	return results, nil
}

func (db *SQLDB) UpdateAIEmployeeTask(ctx context.Context, employeeID uuid.UUID, taskID uuid.UUID) error {
	_, err := db.ExecContext(ctx, "UPDATE ai_employees SET current_task_id = $1 WHERE id = $2", taskID, employeeID)
	return err
}

func (db *SQLDB) ValidateHierarchyInvariant(ctx context.Context, employeeID uuid.UUID) error {
	var workspaceID string
	err := db.QueryRowContext(ctx, `
		SELECT d.workspace_id FROM ai_employees ae
		JOIN teams t ON ae.team_id = t.id
		JOIN departments d ON t.department_id = d.id
		WHERE ae.id = $1
	`, employeeID).Scan(&workspaceID)
	if err != nil {
		return fmt.Errorf("hierarchy validation failed: %w", err)
	}

	if currentWS, ok := ctx.Value("current_workspace").(string); ok && currentWS != "" && workspaceID != currentWS {
		return fmt.Errorf("workspace mismatch: employee workspace %s != current %s", workspaceID, currentWS)
	}
	return nil
}

// Vector helpers - PGVector encodes float arrays as the literal '[1,2,3]'
func vectorLiteral(embedding []float32) string {
	parts := make([]string, 0, len(embedding))
	for _, v := range embedding {
		parts = append(parts, fmt.Sprintf("%g", v))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (db *SQLDB) StoreVector(ctx context.Context, workspaceID uuid.UUID, key string, embedding []float32, metadata repository.JSONB) error {
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO vector_embeddings (workspace_id, key, embedding, metadata, created_at, updated_at)
		VALUES ($1, $2, $3::vector, $4, NOW(), NOW())
		ON CONFLICT (workspace_id, key)
		DO UPDATE SET embedding = EXCLUDED.embedding, metadata = EXCLUDED.metadata, updated_at = NOW()
	`, workspaceID, key, vectorLiteral(embedding), string(metadataJSON))
	return err
}

func (db *SQLDB) RetrieveVector(ctx context.Context, workspaceID uuid.UUID, key string) ([]float32, error) {
	var embedding string
	err := db.QueryRowContext(ctx, `
		SELECT embedding::text FROM vector_embeddings WHERE workspace_id = $1 AND key = $2
	`, workspaceID, key).Scan(&embedding)
	if err != nil {
		return nil, err
	}
	trimmed := strings.Trim(embedding, "[]")
	if trimmed == "" {
		return []float32{}, nil
	}
	strParts := strings.Split(trimmed, ",")
	result := make([]float32, 0, len(strParts))
	for _, p := range strParts {
		var f float64
		if _, err := fmt.Sscanf(p, "%f", &f); err != nil {
			return nil, err
		}
		result = append(result, float32(f))
	}
	return result, nil
}

func (db *SQLDB) ListVectorsByWorkspace(ctx context.Context, workspaceID uuid.UUID, prefix string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT key FROM vector_embeddings WHERE workspace_id = $1 AND key LIKE $2
	`, workspaceID, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func (db *SQLDB) DeleteVector(ctx context.Context, workspaceID uuid.UUID, key string) error {
	_, err := db.ExecContext(ctx, `
		DELETE FROM vector_embeddings WHERE workspace_id = $1 AND key = $2
	`, workspaceID, key)
	return err
}

// Audit event queries
func (db *SQLDB) StoreEvent(ctx context.Context, event *repository.AuditEvent) error {
	now := time.Now().UTC()
	if event.Timestamp.IsZero() {
		event.Timestamp = now
	}

	// Genesis event handling: i==0 case uses GENESIS_HASH, never events[i-1]
	if event.Genesis {
		event.HashValue = sha256Sum([]byte(GENESIS_HASH + "." + event.Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00")))
		event.HashParent = uuid.Nil
	} else if event.HashParent != uuid.Nil {
		prevEvent, err := getPreviousEvent(ctx, db.DB, event.HashParent)
		if err != nil {
			return fmt.Errorf("no previous event found for hash chain parent %s: %w", event.HashParent, err)
		}
		input := hashChainInput(prevEvent.HashValue, prevEvent.EventID, prevEvent.Timestamp, prevEvent.Genesis,
			event.EventType, event.ActorType, event.ActorID, event.TargetType, event.TargetID)
		event.HashValue = sha256Sum([]byte(input))
	}

	// HMAC-SHA256 authenticity tag over the hash value
	event.DigitalSignature = hmacSHA256([]byte(config.Get().JWTSecret), event.HashValue)

	_, err := db.ExecContext(ctx, `
		INSERT INTO audit_events (event_id, timestamp, trace_id, span_id, actor_type, actor_id,
			target_type, target_id, event_type, outcome,
			constitutional_principle, digital_signature, hash_chain_parent, hash_chain_value, genesis)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
	`, event.EventID, event.Timestamp, event.TraceID, event.SpanID, event.ActorType, event.ActorID,
		event.TargetType, event.TargetID, event.EventType, event.Outcome,
		event.ConstitutionalPrinciple, event.DigitalSignature,
		event.HashParent, event.HashValue, event.Genesis)
	return err
}

func (db *SQLDB) GetAuditEvent(ctx context.Context, id uuid.UUID) (*repository.AuditEvent, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, event_id, timestamp, trace_id, span_id, actor_type, actor_id, target_type,
		       target_id, event_type, outcome,
		       constitutional_principle, digital_signature, hash_chain_parent, hash_chain_value, genesis
		FROM audit_events WHERE id = $1`, id)

	var e repository.AuditEvent
	var hashValue []byte
	var digitalSig []byte

	err := row.Scan(&e.ID, &e.EventID, &e.Timestamp, &e.TraceID, &e.SpanID,
		&e.ActorType, &e.ActorID, &e.TargetType, &e.TargetID, &e.EventType, &e.Outcome,
		&e.ConstitutionalPrinciple,
		&digitalSig, &e.HashParent, &hashValue, &e.Genesis)
	if err != nil {
		return nil, err
	}
	e.HashValue = hashValue
	e.DigitalSignature = digitalSig
	return &e, nil
}

func (db *SQLDB) GetAuditEventByID(ctx context.Context, id uuid.UUID) (*repository.AuditEvent, error) {
	return db.GetAuditEvent(ctx, id)
}

func (db *SQLDB) ListEventsByTrace(ctx context.Context, traceID uuid.UUID) ([]*repository.AuditEvent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, event_id, timestamp, trace_id, span_id, actor_type, actor_id, target_type,
		       target_id, event_type, outcome,
		       constitutional_principle, digital_signature, hash_chain_parent, hash_chain_value, genesis
		FROM audit_events WHERE trace_id = $1 ORDER BY timestamp`, traceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*repository.AuditEvent
	for rows.Next() {
		var e repository.AuditEvent
		var hashValue []byte
		var digitalSig []byte
		if err := rows.Scan(&e.ID, &e.EventID, &e.Timestamp, &e.TraceID, &e.SpanID,
			&e.ActorType, &e.ActorID, &e.TargetType, &e.TargetID, &e.EventType, &e.Outcome,
			&e.ConstitutionalPrinciple,
			&digitalSig, &e.HashParent, &hashValue, &e.Genesis); err != nil {
			return nil, err
		}
		e.HashValue = hashValue
		e.DigitalSignature = digitalSig
		results = append(results, &e)
	}
	return results, nil
}

func (db *SQLDB) CreateAuditEvent(ctx context.Context, event *repository.AuditEvent) error {
	return db.StoreEvent(ctx, event)
}

func (db *SQLDB) VerifyHashChain(ctx context.Context, events []*repository.AuditEvent) bool {
	return VerifyHashChain(events)
}

// Principle queries
func (db *SQLDB) RegisterPrinciple(name string, requiredFor []string, enforcementDescription string) error {
	requiredForJSON, err := json.Marshal(requiredFor)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO constitutional_principles (name, required_for, enforcement_description, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (name) DO UPDATE SET required_for = $2, enforcement_description = $3, updated_at = NOW()
	`, name, string(requiredForJSON), enforcementDescription)
	return err
}

func (db *SQLDB) GetPrinciple(name string) (string, error) {
	var enforcementDesc string
	err := db.QueryRowContext(context.Background(),
		"SELECT enforcement_description FROM constitutional_principles WHERE name = $1", name).Scan(&enforcementDesc)
	return enforcementDesc, err
}

func (db *SQLDB) ListPrinciples() (map[string]string, error) {
	rows, err := db.QueryContext(context.Background(),
		"SELECT name, enforcement_description FROM constitutional_principles")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make(map[string]string)
	for rows.Next() {
		var name, desc string
		if err := rows.Scan(&name, &desc); err != nil {
			return nil, err
		}
		results[name] = desc
	}
	return results, nil
}

func sha256Sum(data []byte) []byte {
	hash := sha256.Sum256(data)
	return hash[:]
}

func hmacSHA256(secret, data []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	return mac.Sum(nil)
}
