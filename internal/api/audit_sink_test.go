package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"austro-os/internal/audit"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// recordingSink is an in-memory audit.Sink for handler unit tests.
//
// It is not evidence of persistent audit: that is what
// tests/audit_persistence_test.go proves against a real PostgreSQL. Its job
// here is narrower and still necessary -- the handlers now fail closed when a
// security-relevant outcome cannot be audited, so a handler test must supply a
// sink or every success path returns 500.
type recordingSink struct {
	mu      sync.Mutex
	records []audit.Record
	err     error
}

// failingSink simulates a persistence failure, so the fail-closed behaviour can
// be tested without a database.
type failingSink struct{ err error }

func (s *recordingSink) Append(_ context.Context, rec audit.Record) (*audit.AuditEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	s.records = append(s.records, rec)
	now := time.Now().UTC()
	return &audit.AuditEvent{
		EventID:                 uuid.New(),
		Timestamp:               now,
		EventType:               rec.EventType,
		Outcome:                 rec.Outcome,
		ConstitutionalPrinciple: rec.Principle,
	}, nil
}

// byType returns the recorded events of one type.
func (s *recordingSink) byType(eventType string) []audit.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []audit.Record
	for _, r := range s.records {
		if r.EventType == eventType {
			out = append(out, r)
		}
	}
	return out
}

// requireOutcome asserts exactly one event of the given type with the given
// outcome was recorded.
func (s *recordingSink) requireOutcome(t *testing.T, eventType, outcome string) {
	t.Helper()
	got := s.byType(eventType)
	require.Len(t, got, 1, "expected exactly one %s audit record", eventType)
	require.Equal(t, outcome, got[0].Outcome)
}

func (f failingSink) Append(_ context.Context, _ audit.Record) (*audit.AuditEvent, error) {
	return nil, f.err
}

// errTestAudit is the injected persistence failure.
var errTestAudit = errors.New("audit persistence unavailable")

// The fail-closed contract, without a database: when the audit sink cannot
// persist, a successful authentication must not be reported as a success.
func TestLoginFailsClosedWhenAuditSinkFails(t *testing.T) {
	h := testHandler(true)
	boot := doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	require.Equal(t, http.StatusCreated, boot.Code)

	// Baseline: with a working sink the login succeeds.
	ok := doRequest(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"founder","password":"test-founder-password"}`, "")
	require.Equal(t, http.StatusOK, ok.Code)

	// Now make persistence fail.
	h.SetAuditSink(failingSink{err: errTestAudit})
	bad := doRequest(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"founder","password":"test-founder-password"}`, "")
	require.Equal(t, http.StatusInternalServerError, bad.Code,
		"login must not return 200 when its audit record could not be written")
}

// A denied login is still audited, and a failure to persist that denial must
// not turn the refusal into an error page: the caller already got what it
// deserved.
func TestDeniedLoginIsAudited(t *testing.T) {
	h := testHandler(true)
	boot := doRequest(t, h, http.MethodPost, "/api/auth/bootstrap", `{}`, "")
	require.Equal(t, http.StatusCreated, boot.Code)

	sink := &recordingSink{}
	h.SetAuditSink(sink)
	denied := doRequest(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"founder","password":"wrong-password"}`, "")
	require.Equal(t, http.StatusUnauthorized, denied.Code)
	sink.requireOutcome(t, "auth.login", "denied")

	accepted := doRequest(t, h, http.MethodPost, "/api/auth/login",
		`{"username":"founder","password":"test-founder-password"}`, "")
	require.Equal(t, http.StatusOK, accepted.Code)

	logins := sink.byType("auth.login")
	require.Len(t, logins, 2, "both the refusal and the success must be audited")
	require.Equal(t, "denied", logins[0].Outcome)
	require.Equal(t, "success", logins[1].Outcome)
}
