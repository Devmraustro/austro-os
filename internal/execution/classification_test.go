package execution

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestClassifyKnownCauses(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		code  ErrorCode
		retry bool
	}{
		{"nil is none", nil, CodeNone, false},
		{"deadline exceeded is retryable", context.DeadlineExceeded, CodeDeadlineExceeded, true},
		{"context canceled is not retryable", context.Canceled, CodeCanceled, false},
		{"sql no rows is not retryable by default", sql.ErrNoRows, CodeNotFound, false},
		{"wrapped deadline maps through errors.Is", errors.Join(errors.New("outer"), context.DeadlineExceeded), CodeDeadlineExceeded, true},
		{"unrecognized is unknown", errors.New("mystery"), CodeUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Classify(tt.cause)
			if c.Code != tt.code {
				t.Errorf("Classify(%v).Code = %d, want %d", tt.cause, c.Code, tt.code)
			}
			if c.Retry != tt.retry {
				t.Errorf("Classify(%v).Retry = %v, want %v", tt.cause, c.Retry, tt.retry)
			}
		})
	}
}

func TestRetryableCodes(t *testing.T) {
	for code, want := range map[ErrorCode]bool{
		CodeNone:               false,
		CodeDeadlineExceeded:   true,
		CodeProviderTimeout:    true,
		CodeRateLimit:          true,
		CodeUnavailable:        true,
		CodeCanceled:           false,
		CodeNotFound:           false,
		CodeUnknown:            false,
		CodeWorkerLeaseExpired: true,
	} {
		if got := code.Retryable(); got != want {
			t.Errorf("%s.Retryable() = %v, want %v", code, got, want)
		}
	}
}

func TestClassifiedErrorIsMatchesNotRetryable(t *testing.T) {
	ce := NewClassifiedError(CodeUnknown, errors.New("boom"))
	if !errors.Is(ce, ErrNotRetryable) {
		t.Fatal("non-retryable classified error must match ErrNotRetryable")
	}
	if errors.Is(NewClassifiedError(CodeRateLimit, errors.New("throttled")), ErrNotRetryable) {
		t.Fatal("retryable classified error must NOT match ErrNotRetryable")
	}
	if err := errors.Join(ce, context.Canceled); !errors.Is(err, ErrNotRetryable) {
		t.Fatal("joined error must still match ErrNotRetryable through unwrap")
	}
}

func TestWorkerLeaseExpiredSentinel(t *testing.T) {
	if !ErrWorkerLeaseExpired.Retry {
		t.Fatal("worker_lease_expired must be retryable")
	}
	if ErrWorkerLeaseExpired.Code != CodeWorkerLeaseExpired {
		t.Fatalf("code = %d, want %d", ErrWorkerLeaseExpired.Code, CodeWorkerLeaseExpired)
	}
	if !ErrWorkerLeaseExpired.Retry {
		t.Fatal("worker_lease_expired must be retryable")
	}
}

func TestStatusValidAndTerminal(t *testing.T) {
	if !ValidStatus(StatusPending) || !ValidStatus(StatusRunning) {
		t.Fatal("pending and running must be valid")
	}
	for _, s := range []Status{StatusCompleted, StatusFailed, StatusCancelled} {
		if !ValidStatus(s) || !Terminal(s) {
			t.Errorf("%s must be valid and terminal", s)
		}
	}
	if Terminal(StatusPending) || Terminal(StatusRunning) {
		t.Fatal("pending and running are not terminal")
	}
	if ValidStatus(Status("draft")) {
		t.Fatal("draft must not be a valid status")
	}
}

func TestRetryStateAndOutcomeValidation(t *testing.T) {
	for _, s := range []RetryState{RetryPending, RetryClaimed, RetryDispatched, RetryProcessed} {
		if !ValidRetryState(s) {
			t.Errorf("%s must be a valid retry state", s)
		}
	}
	if ValidRetryState(RetryState("queued")) {
		t.Fatal("queued must not be a valid retry state")
	}
	for _, o := range []RetryOutcome{RetrySuccess, RetryFailed, RetryCancelled} {
		if !ValidRetryOutcome(o) {
			t.Errorf("%s must be a valid outcome", o)
		}
	}
}

func TestErrorCodeString(t *testing.T) {
	_ = CodeWorkerLeaseExpired.String()
	_ = ErrorCode(99).String()
}
