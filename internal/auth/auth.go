package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"austro-os/internal/config"
	"austro-os/internal/rbac"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// Issuer and audience embedded in tokens so that a token minted by this
	// service is rejected by any other service (and stale tokens are invalidated
	// when the deployment is replaced).
	// These are fixed public JWT metadata values, not credentials. The scanner's
	// G101 heuristic matches the service identifier; no secret is embedded here.
	TokenIssuer   = "austro-os"     // #nosec G101 -- public JWT issuer identifier.
	TokenAudience = "austro-os-api" // #nosec G101 -- public JWT audience identifier.
)

// refreshTokenTTL is the lifetime of a refresh token. Rotation means each
// refresh shortens the remaining lifetime of the chain in practice, and reuse
// detection revokes the family outright.
const refreshTokenTTL = 30 * 24 * time.Hour

var ErrInvalidToken = errors.New("invalid token")

// ErrRefreshTokenReused reports that a refresh token which was already
// consumed or revoked was presented again. It is returned wrapped together with
// ErrInvalidToken so existing callers that test for the generic error keep
// working, while the audit layer can tell a replay apart from an ordinary bad
// token and record the security-relevant distinction.
var ErrRefreshTokenReused = errors.New("refresh token reused")

// Claims are the verified identity claims carried by an access token. They are
// minted from the persisted identity at login/refresh time: Role and
// WorkspaceID always come from the database-backed UserRecord, never from the
// client. No sensitive material (password hashes, refresh-token secrets) is
// ever placed in claims.
type Claims struct {
	jwt.RegisteredClaims
	ID          string              `json:"id"`
	Role        rbac.Role           `json:"role"`
	WorkspaceID string              `json:"workspace_id,omitempty"`
	Permissions map[string][]string `json:"permissions,omitempty"`
}

// StoredRefreshToken is the persisted representation of a refresh token,
// supporting rotation and revocation. It must be stored hashed; the raw token
// is never retained or logged.
type StoredRefreshToken struct {
	ID string `json:"id"`
	// FamilyID identifies the rotation family: the login that started the
	// chain. Every token issued by rotating a member of the family carries the
	// same FamilyID, which is what makes "revoke the whole family" possible
	// when reuse is detected. It is distinct from ID, which is unique per
	// token and therefore cannot identify a chain.
	FamilyID  string    `json:"family_id"`
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
//
// RevokeFamily is part of the contract rather than an optional extra: reuse
// detection is only meaningful if presenting an already-used token revokes
// every descendant of the compromised login, not just the presented token. A
// backend that cannot revoke a family cannot implement refresh-token rotation
// safely.
type RefreshTokenStore interface {
	Save(rt *StoredRefreshToken) error
	Get(tokenHash string) (*StoredRefreshToken, error)
	Revoke(tokenHash string) error
	RevokeFamily(familyID string) error
}

// maxRefreshTokenRecords bounds the in-process refresh-token store so a long
// lived instance cannot grow its session table without limit. 65536 records
// is far above any realistic concurrent-session count for a single instance
// while capping the table at roughly 13 MB, so eviction never fires under
// legitimate load.
const maxRefreshTokenRecords = 1 << 16

// refreshEvictBatch is how many records a single eviction pass removes once
// the sweep threshold is reached. Evicting in batches keeps the cost of
// bounding the table amortised O(1) per insert: scanning the table on every
// insert would make it quadratic.
const refreshEvictBatch = 1 << 12

// refreshSweepThreshold is the size at which Save runs a sweep. It sits one
// batch below the hard bound so a sweep always has room to bring the table
// back down before the bound is reached.
const refreshSweepThreshold = maxRefreshTokenRecords - refreshEvictBatch

type memoryRefreshStore struct {
	mu     sync.RWMutex
	byHash map[string]*StoredRefreshToken
}

// NewMemoryRefreshStore returns an in-memory refresh token store for testing
// and single-instance deployments. It is safe for concurrent use: the HTTP
// refresh endpoint is served concurrently and an unsynchronised map would both
// race and eventually abort the process with a concurrent map write.
func NewMemoryRefreshStore() RefreshTokenStore {
	return &memoryRefreshStore{byHash: map[string]*StoredRefreshToken{}}
}

// Save stores a copy of the record. Storing the caller's pointer would let a
// caller mutate persisted state outside the lock.
func (m *memoryRefreshStore) Save(rt *StoredRefreshToken) error {
	if rt == nil || rt.TokenHash == "" {
		return errors.New("refresh token record requires a token hash")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byHash == nil {
		m.byHash = make(map[string]*StoredRefreshToken)
	}
	if _, exists := m.byHash[rt.TokenHash]; !exists && len(m.byHash) >= refreshSweepThreshold {
		// Keep the table bounded, but only when it is actually near the bound
		// and then in batches, so the sweep cost is amortised across many
		// inserts. Expired records are reclaimed first; the oldest live records
		// are dropped only if expiring was not enough.
		m.evictExpiredLocked(time.Now())
		if len(m.byHash) >= refreshSweepThreshold {
			m.evictOldestLocked(refreshEvictBatch)
		}
	}
	cp := *rt
	m.byHash[rt.TokenHash] = &cp
	return nil
}

// Get returns a copy so callers can never mutate stored state without going
// back through Save.
func (m *memoryRefreshStore) Get(h string) (*StoredRefreshToken, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rt, ok := m.byHash[h]
	if !ok {
		return nil, ErrInvalidToken
	}
	cp := *rt
	return &cp, nil
}

func (m *memoryRefreshStore) Revoke(h string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt, ok := m.byHash[h]; ok {
		rt.Revoked = true
	}
	return nil
}

// RevokeFamily revokes every token descended from one login, which is the
// response to detecting that an already-used token was presented again.
func (m *memoryRefreshStore) RevokeFamily(familyID string) error {
	if familyID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rt := range m.byHash {
		if rt.FamilyID == familyID {
			rt.Revoked = true
		}
	}
	return nil
}

// evictExpiredLocked drops records whose expiry has passed. Caller holds the
// write lock.
func (m *memoryRefreshStore) evictExpiredLocked(now time.Time) {
	for h, rt := range m.byHash {
		if now.After(rt.ExpiresAt) {
			delete(m.byHash, h)
		}
	}
}

// evictOldestLocked removes up to n of the oldest records in a single pass.
// Caller holds the write lock.
func (m *memoryRefreshStore) evictOldestLocked(n int) {
	if n <= 0 || len(m.byHash) == 0 {
		return
	}
	type entry struct {
		hash      string
		createdAt time.Time
	}
	oldest := make([]entry, 0, n)
	for h, rt := range m.byHash {
		if len(oldest) < n {
			oldest = append(oldest, entry{h, rt.CreatedAt})
			continue
		}
		// Replace the newest member of the candidate set when this record is
		// older, keeping the n oldest seen so far.
		newest := 0
		for i := 1; i < len(oldest); i++ {
			if oldest[i].createdAt.After(oldest[newest].createdAt) {
				newest = i
			}
		}
		if rt.CreatedAt.Before(oldest[newest].createdAt) {
			oldest[newest] = entry{h, rt.CreatedAt}
		}
	}
	for _, e := range oldest {
		delete(m.byHash, e.hash)
	}
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

// GenerateAccessToken mints an access token for an identity with its role and
// workspace but no permissions (an authorization-less token that will be denied
// by every rule).
func (s *JWTService) GenerateAccessToken(userID string, workspaceID string, role rbac.Role) (string, error) {
	return s.GenerateAccessTokenWithPermissions(userID, workspaceID, role, nil)
}

// GenerateAccessTokenWithPermissions mints an access token carrying the
// explicit authorization permissions for the identity (action -> resources),
// plus the identity's role. The authorization layer grants a request only when
// an explicit rule exists AND the claims carry the permission AND the role is
// in the rule's allowed set; nil permissions therefore produce a token that
// can authorize nothing.
func (s *JWTService) GenerateAccessTokenWithPermissions(userID string, workspaceID string, role rbac.Role, permissions map[string][]string) (string, error) {
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
		Role:        role,
		WorkspaceID: workspaceID,
		Permissions: permissions,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(s.secret)
}

// IssueRefreshToken creates a signed refresh token and persists a hash of it,
// returning only the raw token to the caller. It starts a new rotation family.
func (s *JWTService) IssueRefreshToken(userID string) (string, error) {
	now := time.Now().UTC()
	rawID := uuid.NewString()
	// A fresh login starts a new family; the family id is the token id of its
	// first member so every descendant can be traced back to this login.
	return s.issueRefreshToken(userID, rawID, rawID, now)
}

// issueRefreshToken mints a refresh token for userID belonging to familyID.
// Rotation reuses this with the family it inherited so the chain stays linked.
func (s *JWTService) issueRefreshToken(userID, tokenID, familyID string, now time.Time) (string, error) {
	claims := &Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    TokenIssuer,
			Subject:   userID,
			Audience:  jwt.ClaimStrings{TokenAudience},
			ExpiresAt: jwt.NewNumericDate(now.Add(refreshTokenTTL)),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        tokenID,
		},
		ID: userID,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.refreshSecret)
	if err != nil {
		return "", err
	}
	rt := &StoredRefreshToken{
		ID:        tokenID,
		FamilyID:  familyID,
		UserID:    userID,
		TokenHash: hashToken(signed),
		CreatedAt: now,
		ExpiresAt: now.Add(refreshTokenTTL),
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
		// Reuse detection: an already-consumed token was presented again, so
		// the chain is compromised. Revoke every token descended from the same
		// login (the family), not merely the presented token — otherwise the
		// attacker keeps working with the successor token that the legitimate
		// holder obtained from the rotation.
		if rt.Used {
			s.revokeFamily(rt.FamilyID)
		}
		return "", fmt.Errorf("%w: %w", ErrRefreshTokenReused, ErrInvalidToken)
	}
	if time.Now().After(rt.ExpiresAt) {
		return "", ErrInvalidToken
	}

	// Rotate: old token is now used/revoked, and the successor stays in the
	// same family so a later reuse still revokes the whole chain.
	familyID := rt.FamilyID
	rt.Used = true
	rt.Revoked = true
	if err := s.refreshStore.Save(rt); err != nil {
		return "", err
	}
	if familyID == "" {
		// Defensive: a record persisted before families existed has no family
		// to inherit, so the successor starts one rather than silently joining
		// a shared empty family id.
		familyID = rt.ID
	}
	return s.issueRefreshToken(claims.ID, uuid.NewString(), familyID, time.Now().UTC())
}

// revokeFamily revokes all tokens sharing the same rotation family ID.
func (s *JWTService) revokeFamily(familyID string) {
	if familyID == "" {
		return
	}
	// RevokeFamily is part of the RefreshTokenStore contract; the error is
	// not actionable here because the presented token is rejected either way,
	// and the caller already receives ErrInvalidToken.
	_ = s.refreshStore.RevokeFamily(familyID)
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

// SubjectFromRefreshToken parses a signed refresh token and returns the user
// identity it was issued to, without consuming (rotating) it. It is used on the
// refresh path to re-mint an access token for the same identity.
func (s *JWTService) SubjectFromRefreshToken(refreshToken string) (string, error) {
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
	if claims.Subject == "" {
		return "", ErrInvalidToken
	}
	return claims.Subject, nil
}

// RevokeRefreshToken revokes a refresh token (logout / session termination).
func (s *JWTService) RevokeRefreshToken(refreshToken string) error {
	return s.refreshStore.Revoke(hashToken(refreshToken))
}

// VerifyAccessToken parses and verifies an access token signature, issuer and
// audience, then validates the identity claims. A token whose role is missing,
// unsupported, or inconsistent with its workspace (a non-founder bound to no
// workspace, or a founder bound to one) is rejected: such a claim set can only
// come from a forged or corrupt token, never from the identity the system
// created.
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
	if err := validateClaimsShape(claims); err != nil {
		return nil, err
	}
	return claims, nil
}

// validateClaimsShape rejects a claim set that could not have been minted for
// a real identity: an unsupported role, or a role/workspace pairing the
// database constraints forbid (founder has no workspace; member/admin always
// do).
func validateClaimsShape(claims *Claims) error {
	if claims == nil || claims.ID == "" {
		return ErrInvalidToken
	}
	if !rbac.ValidRole(claims.Role) {
		return ErrInvalidToken
	}
	switch claims.Role {
	case rbac.RoleFounder:
		if claims.WorkspaceID != "" {
			return ErrInvalidToken
		}
	case rbac.RoleWorkspaceAdmin, rbac.RoleWorkspaceMember:
		if claims.WorkspaceID == "" {
			return ErrInvalidToken
		}
	}
	return nil
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
