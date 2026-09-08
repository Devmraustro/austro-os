package postgres

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"austro-os/internal/auth"
	"austro-os/internal/rbac"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// UserStore is the concrete PostgreSQL adapter for the auth.UserStore port.
// Identity lookups (ByUsername/ByID) run unbound to a workspace context: an
// identity is organization-level, mirroring the founders policy. Credentials
// are stored exclusively as bcrypt hashes in password_hash.
type UserStore struct {
	db *sql.DB
}

// NewUserStore returns a Postgres-backed implementation of auth.UserStore.
func NewUserStore(db *sql.DB) *UserStore {
	return &UserStore{db: db}
}

const userSelectColumns = "id, username, password_hash, display_name, email, is_founder, role, COALESCE(workspace_id::text, ''), created_at, updated_at"

func (s *UserStore) ByUsername(ctx context.Context, username string) (*auth.UserRecord, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userSelectColumns+" FROM users WHERE username = $1", username))
}

func (s *UserStore) ByID(ctx context.Context, id string) (*auth.UserRecord, error) {
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil, auth.ErrUserNotFound
	}
	return scanUser(s.db.QueryRowContext(ctx,
		"SELECT "+userSelectColumns+" FROM users WHERE id = $1", uid))
}

func (s *UserStore) Create(ctx context.Context, u *auth.UserRecord) (*auth.UserRecord, error) {
	if u == nil {
		return nil, errors.New("user is required")
	}
	// The founder is always the founder role and organization-level; any other
	// identity defaults to a workspace member. The database CHECK constraints
	// enforce the same invariant in the persisted row.
	role := auth.RoleFor(u)
	if role == rbac.RoleFounder {
		u.WorkspaceID = ""
	}
	var workspaceID interface{}
	if u.WorkspaceID != "" {
		uid, err := uuid.Parse(u.WorkspaceID)
		if err != nil {
			return nil, err
		}
		workspaceID = uid
	}
	var id uuid.UUID
	var createdAt, updatedAt time.Time
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO users (username, password_hash, display_name, email, is_founder, role, workspace_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at, updated_at`,
		u.Username, u.PasswordHash, u.DisplayName, u.Email, u.IsFounder, string(role), workspaceID).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23514") {
			// Unique violation (23505): username or single-founder conflict.
			// Check violation (23514): a role/workspace pairing the database
			// constraints forbid. Both surface as "already exists" — creating
			// an invalid identity must never look like a legitimate write.
			return nil, auth.ErrUserExists
		}
		return nil, err
	}
	u.ID = id.String()
	u.CreatedAt = createdAt
	u.UpdatedAt = updatedAt
	u.Role = role
	return u, nil
}

type userRowScanner interface {
	Scan(dest ...interface{}) error
}

func scanUser(r userRowScanner) (*auth.UserRecord, error) {
	var u auth.UserRecord
	var id, passwordHash, displayName, email string
	var isFounder bool
	var role string
	var createdAt, updatedAt time.Time
	if err := r.Scan(&id, &u.Username, &passwordHash, &displayName, &email, &isFounder, &role,
		&u.WorkspaceID, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, auth.ErrUserNotFound
		}
		return nil, err
	}
	u.ID = id
	u.PasswordHash = passwordHash
	u.DisplayName = displayName
	u.Email = email
	u.IsFounder = isFounder
	u.Role = rbac.Role(strings.TrimSpace(role))
	u.CreatedAt = createdAt
	u.UpdatedAt = updatedAt
	return &u, nil
}
