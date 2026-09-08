package austro_os_test

import (
	"testing"
	"time"

	"austro-os/internal/auth"
	"austro-os/internal/authfoundation"
	"austro-os/internal/config"
	"austro-os/internal/rbac"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

func testJWTService() *auth.JWTService {
	cfg := &config.Config{
		JWTSecret:        "test-access-secret-at-least-32-chars-long!!",
		JWTRefreshSecret: "test-refresh-secret-at-least-32-chars-long!!",
	}
	return auth.Initialize(cfg)
}

// --- Password hashing (authfoundation) ---

func TestPasswordHashingAndVerification(t *testing.T) {
	ph, err := authfoundation.HashPassword("S3cret-Phrase")
	require.NoError(t, err)
	require.NotEmpty(t, ph.Hash)
	require.NotEmpty(t, ph.Salt)

	// Correct password verifies.
	require.True(t, authfoundation.VerifyPassword("S3cret-Phrase", ph))

	// Incorrect password fails.
	require.False(t, authfoundation.VerifyPassword("wrong", ph))

	// Each hash gets a unique salt (CSPRNG), so equal inputs produce
	// different stored hashes.
	ph2, err := authfoundation.HashPassword("S3cret-Phrase")
	require.NoError(t, err)
	require.NotEqual(t, ph.Hash, ph2.Hash, "salting must make hashes unique per password")
}

func TestNewUserAndLogin(t *testing.T) {
	u, err := authfoundation.NewUser("alice", "SuperSecret42", "alice@example.com")
	require.NoError(t, err)
	require.Equal(t, "alice", u.Username)
	require.True(t, authfoundation.VerifyPassword("SuperSecret42", u.PasswordHash), "login with correct password must succeed")
	require.False(t, authfoundation.VerifyPassword("nope", u.PasswordHash), "login with wrong password must fail")
}

// --- Access token lifecycle (internal/auth) ---

func TestAccessTokenLifecycle(t *testing.T) {
	svc := testJWTService()

	tok, err := svc.GenerateAccessToken("user-1", workspaceA, rbac.RoleWorkspaceMember)
	require.NoError(t, err)

	claims, err := svc.VerifyAccessToken(tok)
	require.NoError(t, err)
	require.Equal(t, "user-1", claims.ID)
	require.Equal(t, workspaceA, claims.WorkspaceID)
	require.Equal(t, rbac.RoleWorkspaceMember, claims.Role)
	require.Equal(t, auth.TokenIssuer, claims.Issuer)
	require.Contains(t, claims.Audience, auth.TokenAudience)
}

func TestAccessTokenRejectsWrongSecret(t *testing.T) {
	svc := testJWTService()
	tok, err := svc.GenerateAccessToken("user-1", workspaceA, rbac.RoleWorkspaceMember)
	require.NoError(t, err)

	// A service with a different secret must reject the token.
	other := auth.Initialize(&config.Config{JWTSecret: "totally-different-secret!!!!!!1", JWTRefreshSecret: "x"})
	_, err = other.VerifyAccessToken(tok)
	require.Error(t, err, "token signed with another secret must be rejected")
}

func TestAccessTokenRejectsUnsupportedRole(t *testing.T) {
	svc := testJWTService()
	// A token with a valid signature but a fabricated role must be rejected as
	// forged: the system can never mint a role that is not supported.
	tok, err := svc.GenerateAccessToken("user-x", workspaceA, rbac.Role("superuser"))
	require.NoError(t, err)
	_, err = svc.VerifyAccessToken(tok)
	require.Error(t, err, "a token with an unsupported role must be rejected")
}

func TestAccessTokenRejectsInconsistentClaims(t *testing.T) {
	svc := testJWTService()

	// A member/admin role with no workspace is an inconsistent identity.
	tokNoWs, err := svc.GenerateAccessToken("user-x", "", rbac.RoleWorkspaceAdmin)
	require.NoError(t, err)
	_, err = svc.VerifyAccessToken(tokNoWs)
	require.Error(t, err, "a non-founder without a workspace must be rejected")

	// A founder bound to a workspace is also inconsistent.
	tokFounderWs, err := svc.GenerateAccessToken("user-x", workspaceA, rbac.RoleFounder)
	require.NoError(t, err)
	_, err = svc.VerifyAccessToken(tokFounderWs)
	require.Error(t, err, "a founder with a workspace must be rejected")
}

func TestAccessTokenRejectsWrongIssuer(t *testing.T) {
	svc := testJWTService()
	// Craft a token signed with the right secret but wrong issuer/audience:
	// the verifier must reject it.
	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "evil-other-system",
			Subject:   "user-1",
			Audience:  jwt.ClaimStrings{"evil-audience"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(15 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		ID:          "user-1",
		Role:        rbac.RoleWorkspaceMember,
		WorkspaceID: workspaceA,
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-access-secret-at-least-32-chars-long!!"))
	require.NoError(t, err)
	_, err = svc.VerifyAccessToken(tok)
	require.Error(t, err, "token with the wrong issuer/audience must be rejected")
}

func TestAccessTokenRejectsExpired(t *testing.T) {
	svc := testJWTService()
	claims := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    auth.TokenIssuer,
			Subject:   "user-1",
			Audience:  jwt.ClaimStrings{auth.TokenAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)), // expired
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
		ID:          "user-1",
		Role:        rbac.RoleWorkspaceMember,
		WorkspaceID: workspaceA,
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("test-access-secret-at-least-32-chars-long!!"))
	require.NoError(t, err)
	_, err = svc.VerifyAccessToken(tok)
	require.Error(t, err, "expired token must be rejected")
}

// --- Refresh token rotation/revocation/reuse (internal/auth) ---

func TestRefreshTokenRotation(t *testing.T) {
	svc := testJWTService()

	rt, err := svc.IssueRefreshToken("user-1")
	require.NoError(t, err)
	require.True(t, svc.VerifyRefreshToken(rt))

	// Rotate: old token becomes invalid, new one replaces it.
	newRT, err := svc.Refresh(rt)
	require.NoError(t, err)
	require.NotEqual(t, rt, newRT)

	require.False(t, svc.VerifyRefreshToken(rt), "old refresh token must be invalidated after rotation")
	require.True(t, svc.VerifyRefreshToken(newRT), "new refresh token must be valid")
}

func TestRefreshTokenReuseDetection(t *testing.T) {
	svc := testJWTService()
	rt, err := svc.IssueRefreshToken("user-2")
	require.NoError(t, err)

	// First use rotates it away.
	newRT, err := svc.Refresh(rt)
	require.NoError(t, err)
	require.True(t, svc.VerifyRefreshToken(newRT))

	// Reusing the already-used token must be rejected.
	_, err = svc.Refresh(rt)
	require.Error(t, err, "reuse of an already-used refresh token must be rejected")
}

func TestRefreshTokenRevocationLogout(t *testing.T) {
	svc := testJWTService()
	rt, err := svc.IssueRefreshToken("user-3")
	require.NoError(t, err)
	require.True(t, svc.VerifyRefreshToken(rt))

	// Logout / session termination revokes the token.
	require.NoError(t, svc.RevokeRefreshToken(rt))
	require.False(t, svc.VerifyRefreshToken(rt), "revoked refresh token must be invalid")

	// Rotating a revoked token must fail.
	_, err = svc.Refresh(rt)
	require.Error(t, err)
}

// TestRefreshTokenStoreHashRoundTrip uses an explicit memory store so the
// rotation/reuse/revoke logic can be exercised without inspecting internals.
func TestRefreshTokenStoreHashRoundTrip(t *testing.T) {
	store := auth.NewMemoryRefreshStore()
	cfg := &config.Config{
		JWTSecret:        "test-access-secret-at-least-32-chars-long!!",
		JWTRefreshSecret: "test-refresh-secret-at-least-32-chars-long!!",
	}
	svc := auth.InitializeWithStore(cfg, store)

	rt, err := svc.IssueRefreshToken("user-5")
	require.NoError(t, err)

	// Rotation through the injected store: old invalid, new valid.
	newRT, err := svc.Refresh(rt)
	require.NoError(t, err)
	require.False(t, svc.VerifyRefreshToken(rt))
	require.True(t, svc.VerifyRefreshToken(newRT))

	// Reuse detection through the injected store.
	_, err = svc.Refresh(rt)
	require.Error(t, err)
}
