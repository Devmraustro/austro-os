package authfoundation

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"
)

// PasswordHash represents a hashed password with salt
type PasswordHash struct {
	Hash     string    `json:"hash"`
	Salt     string    `json:"salt"`
	Iterations int     `json:"iterations"`
}

// HashPassword hashes a plaintext password using SHA256 with salt
func HashPassword(plaintext string) (*PasswordHash, error) {
	salt := generateSalt(16)
	hash := sha256Sum([]byte(plaintext + string(salt)))
	
	return &PasswordHash{
		Hash:     base64.URLEncoding.EncodeToString(hash),
		Salt:     base64.URLEncoding.EncodeToString(salt),
		Iterations: 1,
	}, nil
}

// VerifyPassword checks a plaintext password against a stored hash
func VerifyPassword(plaintext string, stored *PasswordHash) bool {
	salt, err := base64.URLEncoding.DecodeString(stored.Salt)
	if err != nil {
		return false
	}
	hash := sha256Sum([]byte(plaintext + string(salt)))
	expected, err := base64.URLEncoding.DecodeString(stored.Hash)
	if err != nil {
		return false
	}
	return hmac.Equal(hash, expected)
}

// Session represents an active user session
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}

// RefreshToken represents a refresh token with rotation support
type RefreshToken struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
	Used      bool      `json:"used"` // for reuse detection
}

// GenerateRefreshToken creates a new refresh token
func GenerateRefreshToken(userID string, ttl time.Duration) *RefreshToken {
	now := time.Now()
	return &RefreshToken{
		ID:        fmt.Sprintf("rt-%s-%d", userID, now.Unix()),
		UserID:    userID,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
		Revoked:   false,
		Used:      false,
	}
}

// IsValid checks if a refresh token is valid (not expired, not revoked, not used)
func (rt *RefreshToken) IsValid() bool {
	if rt.Revoked || rt.Used {
		return false
	}
	if time.Now().After(rt.ExpiresAt) {
		return false
	}
	return true
}

// RotateRefreshToken marks the old token as used/revoked and returns a new one
func (rt *RefreshToken) Rotate() (*RefreshToken, error) {
	if !rt.IsValid() {
		return nil, fmt.Errorf("cannot rotate invalid refresh token")
	}
	
	// Mark old token as used
	rt.Used = true
	rt.Revoked = true
	
	// Create new token
	newToken := GenerateRefreshToken(rt.UserID, 30*24*time.Hour)
	newToken.ID = fmt.Sprintf("rt-new-%s-%d", rt.UserID, time.Now().Unix())
	
	return newToken, nil
}

// IsRevoked checks if a refresh token has been revoked
func (rt *RefreshToken) IsRevoked() bool {
	return rt.Revoked
}

// AuthAuditEvent represents an authentication audit event
type AuthAuditEvent struct {
	ID        string    `json:"id"`
	EventID   string    `json:"event_id"`
	Timestamp time.Time `json:"timestamp"`
	TraceID   string    `json:"trace_id,omitempty"`
	SpanID    string    `json:"span_id,omitempty"`
	ActorType string    `json:"actor_type"`
	ActorID   string    `json:"actor_id"`
	Action    string    `json:"action"` // login, logout, token_refresh, token_revoke, password_change, password_verify
	Outcome   string    `json:"outcome"` // success, failure
	Details   string    `json:"details,omitempty"` // e.g., "password_verify_success", "refresh_rotation"
}

// NewAuthAuditEvent creates a new authentication audit event
func NewAuthAuditEvent(actorType, actorID, action, outcome string, details ...string) *AuthAuditEvent {
	event := &AuthAuditEvent{
		ID:        generateID(),
		EventID:   generateID(),
		Timestamp: time.Now().UTC(),
		ActorType: actorType,
		ActorID:   actorID,
		Action:    action,
		Outcome:   outcome,
	}
	
	if len(details) > 0 {
		event.Details = details[0]
	}
	
	return event
}

// Authenticator defines the interface for authentication operations
type Authenticator interface {
	// Password operations
	HashPassword(plaintext string) (*PasswordHash, error)
	VerifyPassword(plaintext string, stored *PasswordHash) bool
	
	// Session operations
	CreateSession(userID string, ttl time.Duration) *Session
	ValidateSession(session *Session) bool
	RevokeSession(session *Session)
	
	// Refresh token operations
	CreateRefreshToken(userID string, ttl time.Duration) *RefreshToken
	ValidateRefreshToken(rt *RefreshToken) bool
	RotateRefreshToken(rt *RefreshToken) (*RefreshToken, error)
	RevokeRefreshToken(rt *RefreshToken)
	
	// Authentication audit
	LogAuthEvent(event *AuthAuditEvent)
	
	// User operations (stubbed for foundation)
	CreateUser(username, password, email string) error
	GetUser(username string) (*User, error)
}

// User represents a system user
type User struct {
	ID        string
	Username  string
	Email     string
	PasswordHash *PasswordHash
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewUser creates a new user with hashed password
func NewUser(username, password, email string) (*User, error) {
	ph, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	
	return &User{
		Username:    username,
		Email:       email,
		PasswordHash: ph,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}, nil
}

var idCounter int

func generateID() string {
	idCounter++
	return fmt.Sprintf("evt-%d", idCounter)
}

func generateSalt(length int) []byte {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failure: %v", err))
	}
	return b
}

func sha256Sum(data []byte) []byte {
	hash := sha256.Sum256(data)
	return hash[:]
}