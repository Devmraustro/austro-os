package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"austro-os/internal/config"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// Issuer and audience embedded in tokens so that a token minted by this
	// service is rejected by any other service (and stale tokens are invalidated
	// when the deployment is replaced).
	TokenIssuer   = "austro-os"
	TokenAudience = "austro-os-api"
)

var ErrInvalidToken = errors.New("invalid token")

type Claims struct {
	jwt.RegisteredClaims
	ID           string            `json:"id"`
	WorkspaceID  string            `json:"workspace_id,omitempty"`
	Permissions  map[string][]string `json:"permissions,omitempty"`
}

// StoredRefreshToken is the persisted representation of a refresh token,
// supporting rotation and revocation. It must be stored hashed; the raw token
// is never retained or logged.
type StoredRefreshToken struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	TokenHash string    `json:"token_hash"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
	Used      bool      `json:"used"` // reuse detection: a used token must never be accepted again
}

// RefreshTokenStore persists refresh tokens and their rotation/revocation
// state. The default implementation is in-memory; production uses a durable
// backend. Only the SHA-256 hash of the token is stored.
type RefreshTokenStore interface {
	Save(rt *StoredRefreshToken) error
	Get(tokenHash string) (*StoredRefreshToken, error)
	Revoke(tokenHash string) error
}

type memoryRefreshStore struct {
	byHash map[string]*StoredRefreshToken
}

// NewMemoryRefreshStore returns an in-memory refresh token store for testing
// and single-instance deployments.
func NewMemoryRefreshStore() RefreshTokenStore {
	return &memoryRefreshStore{byHash: map[string]*StoredRefreshToken{}}
}

func (m *memoryRefreshStore) Save(rt *StoredRefreshToken) error {
	m.byHash[rt.TokenHash] = rt
	return nil
}
func (m *memoryRefreshStore) Get(h string) (*StoredRefreshToken, error) {
	rt, ok := m.byHash[h]
	if !ok {
		return nil, ErrInvalidToken
	}
	return rt, nil
}
func (m *memoryRefreshStore) Revoke(h string) error {
	if rt, ok := m.byHash[h]; ok {
		rt.Revoked = true
	}
	return nil
}

type JWTService struct {
	secret        []byte
	refreshSecret []byte
	refreshStore  RefreshTokenStore
}

func Initialize(cfg *config.Config) *JWTService {
	return &JWTService{
		secret:        []byte(cfg.JWTSecret),
		refreshSecret: []byte(cfg.JWTRefreshSecret),
		refreshStore:  NewMemoryRefreshStore(),
	}
}

// InitializeWithStore configures the service with a custom refresh token
// store (used by tests to exercise persistence-backed rotation).
func InitializeWithStore(cfg *config.Config, store RefreshTokenStore) *JWTService {
	s := Initialize(cfg)
	s.refreshStore = store
	return s
}

func (s *JWTService) GenerateAccessToken(userID string, workspaceID string) (string, error) {
	now := time.Now().UTC()
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    TokenIssuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{TokenAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        uuid.NewString(),
		},
		ID:          userID,
		WorkspaceID: workspaceID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secret)
}

// IssueRefreshToken creates a signed refresh token and persists a hash of it,
// returning only the raw token to the caller.
func (s *JWTService) IssueRefreshToken(userID string) (string, error) {
	now := time.Now().UTC()
	rawID := uuid.NewString()
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    TokenIssuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{TokenAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(30 * 24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        rawID,
		},
		ID: userID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.refreshSecret)
	if err != nil {
		return "", err
	}
	rt := &StoredRefreshToken{
		ID:        rawID,
		UserID:    userID,
		TokenHash: hashToken(signed),
		CreatedAt: now,
		ExpiresAt: now.Add(30 * 24 * time.Hour),
	}
	if err := s.refreshStore.Save(rt); err != nil {
		return "", err
	}
	return signed, nil
}

// Refresh rotates a refresh token: it validates the presented token, rejects
// reused/revoked tokens, revokes the old one, and issues a new one.
func (s *JWTService) Refresh(refreshToken string) (string, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(refreshToken, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Method)
		}
		return s.refreshSecret, nil
	}, jwt.WithIssuer(TokenIssuer), jwt.WithAudience(TokenAudience))
	if err != nil {
		return "", err
	}
	if !token.Valid {
		return "", ErrInvalidToken
	}

	// Look up the persisted record by its hash.
	rt, err := s.refreshStore.Get(hashToken(refreshToken))
	if err != nil {
		return "", ErrInvalidToken
	}
	if rt.Used || rt.Revoked {
		// Reuse detection: mark the entire token family revoked.
		if rt.Used {
			s.revokeFamily(rt.ID)
		}
		return "", ErrInvalidToken
	}
	if time.Now().After(rt.ExpiresAt) {
		return "", ErrInvalidToken
	}

	// Rotate: old token is now used/revoked.
	rt.Used = true
	rt.Revoked = true
	if err := s.refreshStore.Save(rt); err != nil {
		return "", err
	}
	return s.IssueRefreshToken(claims.ID)
}

// revokeFamily revokes all tokens sharing the same rotation family ID.
func (s *JWTService) revokeFamily(familyID string) {
	if fs, ok := s.refreshStore.(*memoryRefreshStore); ok {
		for _, rt := range fs.byHash {
			if rt.ID == familyID {
				rt.Revoked = true
			}
		}
	}
}

// VerifyRefreshToken reports whether a refresh token is currently valid and
// not revoked/reused.
func (s *JWTService) VerifyRefreshToken(refreshToken string) bool {
	rt, err := s.refreshStore.Get(hashToken(refreshToken))
	if err != nil {
		return false
	}
	return !rt.Revoked && !rt.Used && time.Now().Before(rt.ExpiresAt)
}

// RevokeRefreshToken revokes a refresh token (logout / session termination).
func (s *JWTService) RevokeRefreshToken(refreshToken string) error {
	return s.refreshStore.Revoke(hashToken(refreshToken))
}

func (s *JWTService) VerifyAccessToken(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Method)
		}
		return s.secret, nil
	}, jwt.WithIssuer(TokenIssuer), jwt.WithAudience(TokenAudience))
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, ErrInvalidToken
	}
	return claims, nil
}

// HashRefreshToken exposes the deterministic digest for tests that must
// inspect that raw tokens are never stored.
func HashRefreshToken(token string) string {
	return hashToken(token)
}

// claimsContextKey is the request-context key for verified claims.
type claimsContextKey struct{}

// WithClaims attaches verified claims to a request context.
func WithClaims(r *http.Request, claims *Claims) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), claimsContextKey{}, claims))
}

// ClaimsFromContext returns verified claims stored on the request context.
func ClaimsFromContext(r *http.Request) (*Claims, bool) {
	claims, ok := r.Context().Value(claimsContextKey{}).(*Claims)
	return claims, ok
}

// FromRequest resolves and verifies the caller's claims from the request. It
// returns the claims and true when a valid bearer token is present; otherwise
// (nil, false). Malformed or missing credentials never produce claims.
func FromRequest(r *http.Request) (*Claims, bool) {
	if claims, ok := ClaimsFromContext(r); ok && claims != nil {
		return claims, true
	}
	return nil, false
}

// BearerClaims extracts claims from an Authorization: Bearer <token> header.
// It returns (nil, false) when the header is absent or malformed.
func BearerClaims(bearer string, verify func(token string) (*Claims, error)) (*Claims, bool) {
	const prefix = "Bearer "
	token := strings.TrimSpace(bearer)
	if !strings.HasPrefix(token, prefix) {
		return nil, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(token, prefix))
	if raw == "" {
		return nil, false
	}
	claims, err := verify(raw)
	if err != nil {
		return nil, false
	}
	return claims, true
}

func hashToken(token string) string {
	// SHA-256 hashing of the raw refresh token; only this digest is persisted.
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
