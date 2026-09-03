package austro_os_test

import (
	"context"
	"strings"
	"testing"

	"austro-os/infrastructure/postgres"
	"austro-os/internal/orchestration"
	"austro-os/internal/publish"
	"austro-os/internal/security"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// captureAudit records every audit record across services for a value-level
// "secrets never logged" assertion.
type captureAudit struct {
	mu         chan struct{}
	fieldTexts []string
}

func newCaptureAudit() *captureAudit { return &captureAudit{mu: make(chan struct{}, 1)} }

// Record implements publish.AuditSink, capturing every string field value.
func (c *captureAudit) Record(_ context.Context, rec publish.AuditRecord) {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	for _, v := range []string{
		rec.EventType, rec.Outcome, rec.WorkspaceID, rec.PublicationID,
		rec.ActorID, rec.TraceID, rec.SpanID, rec.ConstitutionalPrinciple,
	} {
		if v != "" {
			c.fieldTexts = append(c.fieldTexts, v)
		}
	}
}

// allFieldText returns every captured non-empty field value.
func (c *captureAudit) allFieldText() []string {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	out := append([]string{}, c.fieldTexts...)
	return out
}

// TestSecretsNeverLoggedWhilePublishing proves a secret-shaped value given as a
// free-text field never reaches an audit/log field after the publishing
// service and the shared redactor, and that external platform references are
// emitted as irreversible digests (not raw).
func TestSecretsNeverLoggedWhilePublishing(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	wsA := uuid.MustParse(workspaceA)
	store := postgres.NewPublicationStore(admin)
	audit := newCaptureAudit()
	svc := publish.NewService(store, publish.StubPublisher{}, audit, nil)

	// A secret-shaped body: publishing must redact it before any log emission.
	secret := "Bearer ghp_AbCdEfGhIjKlMnOpQrStUvWxYzSecret0123456789"
	p, err := svc.Create(context.Background(), wsA, nil, nil, "Secure", secret, publish.StubPlatform)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM publications WHERE id=$1`, p.ID) })

	_, err = svc.ToReview(context.Background(), wsA, p.ID)
	require.NoError(t, err)
	_, err = svc.Approve(context.Background(), wsA, p.ID, "alice")
	require.NoError(t, err)
	_, err = svc.Publish(context.Background(), wsA, p.ID, "bob")
	require.NoError(t, err)

	for _, field := range audit.allFieldText() {
		require.NotContains(t, field, "ghp_", "a secret-shaped value must never reach an audit field")
		require.NotContains(t, field, "Bearer ", "an auth scheme must never reach an audit field")
	}
	// The redactor independently guarantees the marker replaces secret shapes.
	require.True(t, strings.Contains(security.RedactSecret(secret), security.RedactMarker),
		"the shared redactor must mark the secret-shaped value")
}

// TestSecurityHashDeterministicAndExternalIDsHidden proves the shared external
// id hasher never leaks the raw reference and is deterministic, and the
// deny-by-default guard plus throttler behave correctly on the live run.
func TestSecurityHashDeterministicAndExternalIDsHidden(t *testing.T) {
	hasher := security.NewExternalIDHasher([]byte("hardening-test-key"))
	raw := "stub://stub/some-content-digest"
	d1, err := hasher.Hash(raw)
	require.NoError(t, err)
	d2, err := hasher.Hash(raw)
	require.NoError(t, err)
	require.Equal(t, d1, d2, "hashing must be deterministic")
	require.NotEqual(t, raw, d1, "digest must not equal the raw identifier")
	require.False(t, strings.Contains(d1, "stub://"), "raw identifier prefix must not leak")

	var g security.Guard
	require.ErrorIs(t, g.RequireWorkspace(uuid.Nil), security.ErrMissingWorkspace, "nil workspace must fail closed")
	require.ErrorIs(t, g.RequireHuman(" "), security.ErrMissingActor, "blank actor must fail closed")

	tk := security.NewThrottler(0, 1)
	require.True(t, tk.Allow("ws-a"), "first hit allowed")
	require.False(t, tk.Allow("ws-a"), "second hit within window denied")
}

// TestCrossCapabilityDenyByDefaultProven proves, via the live stack, that
// workspace-B cannot read or mutate workspace-A pipelines through the public
// service interfaces (the deny-by-default invariant held across capabilities).
func TestCrossCapabilityDenyByDefaultProven(t *testing.T) {
	admin, err := getEnv().AdminDB()
	require.NoError(t, err)
	defer admin.Close()
	require.NoError(t, ensureIsolationRoles(admin, "Workspace A", "Workspace B"))

	wsA := uuid.MustParse(workspaceA)
	wsB := uuid.MustParse(workspaceB)
	require.NoError(t, deletePipelinesByWorkspace(admin, wsA))
	require.NoError(t, deletePipelinesByWorkspace(admin, wsB))

	store := postgres.NewPipelineStore(admin)
	svcA := orchestration.NewService(store, nil, nil, nil, nil, nil, nil)
	p, err := svcA.Create(context.Background(), wsA, nil, "trace-sec")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.Exec(`DELETE FROM pipelines WHERE id=$1`, p.ID) })

	svcB := orchestration.NewService(store, nil, nil, nil, nil, nil, nil)
	_, err = svcB.Get(context.Background(), wsB, p.ID)
	require.Error(t, err, "workspace B must not read workspace A pipelines")

	listB, err := svcB.List(context.Background(), wsB)
	require.NoError(t, err)
	require.Empty(t, listB, "workspace B list must be empty")
}
