package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type Repository interface {
	// Workspace methods
	CreateWorkspace(ctx context.Context, name string) (uuid.UUID, error)
	GetWorkspace(ctx context.Context, id uuid.UUID) (*WorkspaceRecord, error)
	ListWorkspaces(ctx context.Context) ([]*WorkspaceRecord, error)

	// Department methods
	CreateDepartment(ctx context.Context, name string, workspaceID uuid.UUID) (uuid.UUID, error)
	GetDepartment(ctx context.Context, id uuid.UUID) (*DepartmentRecord, error)
	ListDepartmentsByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]*DepartmentRecord, error)

	// Team methods
	CreateTeam(ctx context.Context, name string, departmentID uuid.UUID) (uuid.UUID, error)
	GetTeam(ctx context.Context, id uuid.UUID) (*TeamRecord, error)
	ListTeamsByDepartment(ctx context.Context, departmentID uuid.UUID) ([]*TeamRecord, error)

	// AI Employee methods
	CreateAIEmployee(ctx context.Context, name, role string, teamID uuid.UUID) (uuid.UUID, error)
	GetAIEmployee(ctx context.Context, id uuid.UUID) (*AIEmployeeRecord, error)
	ListAIEmployeesByTeam(ctx context.Context, teamID uuid.UUID) ([]*AIEmployeeRecord, error)
	UpdateAIEmployeeTask(ctx context.Context, employeeID uuid.UUID, taskID uuid.UUID) error
	ValidateHierarchyInvariant(ctx context.Context, employeeID uuid.UUID) error

	// Vector store - PGVector
	StoreVector(ctx context.Context, workspaceID uuid.UUID, key string, embedding []float32, metadata JSONB) error
	RetrieveVector(ctx context.Context, workspaceID uuid.UUID, key string) ([]float32, error)
	ListVectorsByWorkspace(ctx context.Context, workspaceID uuid.UUID, prefix string) ([]string, error)
	DeleteVector(ctx context.Context, workspaceID uuid.UUID, key string) error

	// Memory methods - Session
	StoreSessionMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error
	RetrieveSessionMemory(ctx context.Context, key string) ([]byte, error)
	DeleteSessionMemory(ctx context.Context, key string) error
	ListSessionMemoryPrefix(ctx context.Context, prefix string) ([]string, error)

	// Memory methods - Long-Term
	StoreLongTermMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error
	RetrieveLongTermMemory(ctx context.Context, key string) ([]byte, error)
	DeleteLongTermMemory(ctx context.Context, key string) error
	ListLongTermMemoryPrefix(ctx context.Context, prefix string) ([]string, error)

	// Memory methods - Workspace (partitioned)
	StoreWorkspaceMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error
	RetrieveWorkspaceMemory(ctx context.Context, key string) ([]byte, error)
	DeleteWorkspaceMemory(ctx context.Context, key string) error
	ListWorkspaceMemoryPrefix(ctx context.Context, prefix string) ([]string, error)

	// Memory methods - Organizational
	StoreOrganizationalMemory(ctx context.Context, key string, value []byte, ttl time.Duration) error
	RetrieveOrganizationalMemory(ctx context.Context, key string) ([]byte, error)
	DeleteOrganizationalMemory(ctx context.Context, key string) error
	ListOrganizationalMemoryPrefix(ctx context.Context, prefix string) ([]string, error)

	// Event methods
	StoreEvent(ctx context.Context, event *AuditEvent) error
	GetEvent(ctx context.Context, eventID uuid.UUID) (*AuditEvent, error)
	ListEventsByTrace(ctx context.Context, traceID uuid.UUID) ([]*AuditEvent, error)

	// Audit methods
	CreateAuditEvent(ctx context.Context, event *AuditEvent) error
	GetAuditEvent(ctx context.Context, id uuid.UUID) (*AuditEvent, error)
	VerifyHashChain(ctx context.Context, events []*AuditEvent) bool

	// Principle methods
	RegisterPrinciple(name string, requiredFor []string, enforcementDescription string) error
	GetPrinciple(name string) (string, error)
	ListPrinciples() (map[string]string, error)
}

type WorkspaceRecord struct {
	ID        uuid.UUID
	Name      string
	CreatedAt time.Time
}

type DepartmentRecord struct {
	ID          uuid.UUID
	Name        string
	WorkspaceID uuid.UUID
	CreatedAt   time.Time
}

type TeamRecord struct {
	ID           uuid.UUID
	Name         string
	DepartmentID uuid.UUID
	CreatedAt    time.Time
}

type AIEmployeeRecord struct {
	ID            uuid.UUID
	Name          string
	Role          string
	TeamID        uuid.UUID
	DepartmentID  uuid.UUID
	WorkspaceID   uuid.UUID
	Capabilities  []string
	Permissions   map[string]bool
	MemoryID      uuid.UUID
	CurrentTaskID uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type JSONB map[string]interface{}

type AuditEvent struct {
	ID                      uuid.UUID `json:"id"`
	EventID                 uuid.UUID `json:"event_id"`
	Timestamp               time.Time `json:"timestamp"`
	TraceID                 uuid.UUID `json:"trace_id,omitempty"`
	SpanID                  uuid.UUID `json:"span_id,omitempty"`
	ActorType               string    `json:"actor_type"`
	ActorID                 uuid.UUID `json:"actor_id"`
	TargetType              string    `json:"target_type"`
	TargetID                uuid.UUID `json:"target_id"`
	EventType               string    `json:"event_type"`
	Outcome                 string    `json:"outcome"`
	HashParent              uuid.UUID `json:"hash_parent,omitempty"`
	HashValue               []byte    `json:"hash_value,omitempty"`
	DigitalSignature        []byte    `json:"digital_signature,omitempty"`
	Genesis                 bool      `json:"genesis,omitempty"`
	ConstitutionalPrinciple string    `json:"constitutional_principle"`
}
