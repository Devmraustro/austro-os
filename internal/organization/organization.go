package organization

import (
	"database/sql"
	"fmt"
	"os"

	"austro-os/internal/config"
	logger "austro-os/internal/log"

	"github.com/google/uuid"
)

type OrgService struct {
	db          *sql.DB
	workspaceID string
}

func Initialize(db *sql.DB, cfg *config.Config, workspaceID string) *OrgService {
	if workspaceID == "" {
		logger.NewEntry("organization-workspace-id-missing").SetLevel("error").Log()
		os.Exit(1)
	}
	return &OrgService{
		db:          db,
		workspaceID: workspaceID,
	}
}

type Founder struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt string    `json:"created_at"`
}

type CEO struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	WorkspaceID string `json:"workspace_id"`
	CreatedAt string    `json:"created_at"`
}

type ExecutiveBoard struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	WorkspaceID string `json:"workspace_id"`
}

type Workspace struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	CreatedAt string    `json:"created_at"`
}

type Department struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	CreatedAt string    `json:"created_at"`
}

type Team struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	DepartmentID uuid.UUID `json:"department_id"`
	CreatedAt string    `json:"created_at"`
}

type AIEmployee struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	TeamID        uuid.UUID `json:"team_id"`
	DepartmentID  uuid.UUID `json:"department_id"`
	WorkspaceID   uuid.UUID `json:"workspace_id"`
	Capabilities  []string  `json:"capabilities,omitempty"`
	Permissions   map[string]bool `json:"permissions,omitempty"`
	MemoryID      uuid.UUID `json:"memory_id,omitempty"`
	CurrentTaskID uuid.UUID `json:"current_task_id,omitempty"`
	CreatedAt     string    `json:"created_at"`
}

func (s *OrgService) CreateFounder(name string) (*Founder, error) {
	id := uuid.New()
	_, err := s.db.Exec(`INSERT INTO founders (id, name) VALUES ($1, $2)`, id, name)
	if err != nil {
		return nil, err
	}
	return &Founder{ID: id, Name: name}, nil
}

func (s *OrgService) CreateCEO(name string) (*CEO, error) {
	id := uuid.New()
	_, err := s.db.Exec(`INSERT INTO ceos (id, name, workspace_id) VALUES ($1, $2, $3)`, id, name, s.workspaceID)
	if err != nil {
		return nil, err
	}
	return &CEO{ID: id, Name: name, WorkspaceID: s.workspaceID}, nil
}

func (s *OrgService) CreateDepartment(name string, workspaceID uuid.UUID) (*Department, error) {
	id := uuid.New()
	_, err := s.db.Exec(`INSERT INTO departments (id, name, workspace_id) VALUES ($1, $2, $3)`, id, name, workspaceID)
	if err != nil {
		return nil, err
	}
	return &Department{ID: id, Name: name, WorkspaceID: workspaceID}, nil
}

func (s *OrgService) CreateTeam(name string, departmentID uuid.UUID) (*Team, error) {
	id := uuid.New()
	_, err := s.db.Exec(`INSERT INTO teams (id, name, department_id) VALUES ($1, $2, $3)`, id, name, departmentID)
	if err != nil {
		return nil, err
	}
	return &Team{ID: id, Name: name, DepartmentID: departmentID}, nil
}

func (s *OrgService) CreateAIEmployee(name, role string, teamID uuid.UUID) (*AIEmployee, error) {
	// Validate invariants: team → department → workspace
	var wsID uuid.UUID
	err := s.db.QueryRow(`SELECT d.workspace_id FROM departments d JOIN teams t ON t.department_id = d.id WHERE t.id = $1`, teamID).Scan(&wsID)
	if err != nil {
		return nil, fmt.Errorf("team not found or workspace mismatch: %w", err)
	}
	
	id := uuid.New()
	_, err = s.db.Exec(`INSERT INTO ai_employees (id, name, role, team_id) VALUES ($1, $2, $3, $4)`, id, name, role, teamID)
	if err != nil {
		return nil, err
	}
	return &AIEmployee{
		ID:            id,
		Name:          name,
		Role:          role,
		TeamID:        teamID,
		WorkspaceID:   wsID,
		Capabilities:  []string{},
		Permissions:   map[string]bool{},
		MemoryID:      uuid.Nil,
		CurrentTaskID: uuid.Nil,
	}, nil
}

func (s *OrgService) ValidateHierarchyInvariant(aiEmployeeID uuid.UUID) error {
	// AI Employee → exactly one Team
	// Team → exactly one Department
	// Department → exactly one Workspace
	var teamID uuid.UUID
	var deptID uuid.UUID
	var workspaceID string
	
	err := s.db.QueryRow(`
		SELECT t.id, d.id, d.workspace_id 
		FROM ai_employees ae 
		JOIN teams t ON ae.team_id = t.id 
		JOIN departments d ON t.department_id = d.id 
		WHERE ae.id = $1
	`, aiEmployeeID).Scan(&teamID, &deptID, &workspaceID)
	
	if err != nil {
		return fmt.Errorf("hierarchy validation failed: %w", err)
	}
	
	// Verify workspace matches
	if workspaceID != s.workspaceID {
		return fmt.Errorf("workspace mismatch: employee workspace %s != current %s", workspaceID, s.workspaceID)
	}
	
	return nil
}