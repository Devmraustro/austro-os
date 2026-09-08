package austro_os_test

import (
	"testing"

	"austro-os/internal/auth"
	"austro-os/internal/config"
	"austro-os/internal/rbac"

	"github.com/stretchr/testify/require"
)

// TestJWTAuthenticationRoundTrip proves the end-to-end JWT auth contract used
// by the Phase 1 gate: a signed access token round-trips through verification,
// and the refresh token is rotated (old invalidated, new issued).
func TestJWTAuthenticationRoundTrip(t *testing.T) {
	cfg := &config.Config{
		JWTSecret:        "test-access-secret-at-least-32-chars-long!!",
		JWTRefreshSecret: "test-refresh-secret-at-least-32-chars-long!!",
	}
	store := auth.NewMemoryRefreshStore()
	svc := auth.InitializeWithStore(cfg, store)

	// Access token: mint then verify and recover claims.
	access, err := svc.GenerateAccessToken("user-42", workspaceB, rbac.RoleWorkspaceAdmin)
	require.NoError(t, err)
	claims, err := svc.VerifyAccessToken(access)
	require.NoError(t, err, "a freshly-minted access token must verify")
	require.Equal(t, "user-42", claims.ID)
	require.Equal(t, workspaceB, claims.WorkspaceID)
	require.Equal(t, rbac.RoleWorkspaceAdmin, claims.Role, "the minted token must carry the identity's role")

	// Refresh token lifecycle: issue -> rotate -> old invalidated, new valid.
	refresh, err := svc.IssueRefreshToken("user-42")
	require.NoError(t, err)
	require.True(t, svc.VerifyRefreshToken(refresh))

	newRefresh, err := svc.Refresh(refresh)
	require.NoError(t, err)
	require.NotEqual(t, refresh, newRefresh, "rotation must yield a new refresh token")
	require.False(t, svc.VerifyRefreshToken(refresh), "reused refresh token must be invalidated")
	require.True(t, svc.VerifyRefreshToken(newRefresh), "rotated refresh token must be accepted")
}
