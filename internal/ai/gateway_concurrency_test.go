package ai_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"austro-os/internal/ai"

	"github.com/stretchr/testify/require"
)

// recordingProvider wraps the deterministic StubProvider and counts every
// provider call atomically. It is used to prove the provider is only invoked
// for requests the usage guard actually accepted.
type recordingProvider struct {
	ai.StubProvider
	calls atomic.Int64
}

func (p *recordingProvider) Classify(ctx context.Context, req ai.ClassificationRequest) (ai.ClassificationResult, error) {
	p.calls.Add(1)
	return p.StubProvider.Classify(ctx, req)
}

// concurrencyAPI returns a Gateway whose allowed requests are counted by an
// atomic, thread-safe recording provider, together with its guard.
func concurrencyAPI(t *testing.T, budget uint64) (*ai.Gateway, *recordingProvider, *ai.ConfigurableUsageGuard) {
	t.Helper()
	prov := &recordingProvider{}
	guard := ai.NewConfigurableUsageGuard(ai.Config{MaxPerWorkspace: budget})
	gw, err := ai.NewGateway(ai.ConfigAPI{Provider: prov, Guard: guard, Sink: ai.NullSink{}})
	require.NoError(t, err)
	return gw, prov, guard
}

// burst launches n concurrent classify requests for the same workspace, all
// released by a closed barrier channel so they genuinely compete, and returns
// the counts of accepted, usage-limit-exceeded and other outcomes. No timing or
// scheduling assumptions are made: the totals are fixed by the budget semantics.
func burst(gw *ai.Gateway, workspaceID string, n int) (accepted, exceeded, other int64) {
	release := make(chan struct{})
	var wg sync.WaitGroup
	var acc, exc, oth atomic.Int64
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-release // barrier: wait for the broadcast start
			_, err := gw.Classify(context.Background(), ai.ClassificationRequest{
				Scope:      ai.Scope{WorkspaceID: workspaceID},
				Categories: []string{"compliant", "needs_review"},
				Input:      "deterministic input",
			})
			switch {
			case err == nil:
				acc.Add(1)
			case errors.Is(err, ai.ErrUsageLimitExceeded):
				exc.Add(1)
			default:
				oth.Add(1)
			}
		}()
	}
	close(release) // broadcast release; any goroutine arriving later still proceeds
	wg.Wait()
	return acc.Load(), exc.Load(), oth.Load()
}

// TestGatewayEnforcesBudgetUnderConcurrentBurst proves a concurrent burst cannot
// collectively exceed the per-workspace budget, that rejected requests never
// reach the provider, and that the total provider calls equal the accepted
// requests exactly.
func TestGatewayEnforcesBudgetUnderConcurrentBurst(t *testing.T) {
	const (
		budget  = 10
		workers = 200
	)
	gw, prov, guard := concurrencyAPI(t, budget)

	accepted, exceeded, other := burst(gw, "ws-concurrent", workers)

	require.Equal(t, int64(budget), accepted, "accepted requests must equal the configured budget")
	require.Equal(t, int64(workers-budget), exceeded, "every remaining request must be refused as budget-exceeded")
	require.Zero(t, other, "no other error category may appear")
	require.Equal(t, int64(budget), prov.calls.Load(), "the provider must be called exactly once per accepted request and never for a rejected one")
	require.Equal(t, uint64(budget), guard.Used("ws-concurrent"), "usage accounting must be deterministic")
}

// TestGatewayWorkspaceIsolationUnderConcurrency proves budgets are scoped per
// workspace: concurrent bursts on two workspaces cannot borrow from each other.
func TestGatewayWorkspaceIsolationUnderConcurrency(t *testing.T) {
	const workers = 200
	gw, prov, guard := concurrencyAPI(t, 1) // one operation per workspace

	start := make(chan struct{})
	var wg sync.WaitGroup
	var accA, accB atomic.Int64
	for _, ws := range []string{"ws-a", "ws-b"} {
		for i := 0; i < workers; i++ {
			ws := ws
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := gw.Classify(context.Background(), ai.ClassificationRequest{
					Scope:      ai.Scope{WorkspaceID: ws},
					Categories: []string{"a", "b"},
					Input:      "x",
				})
				if err == nil {
					if ws == "ws-a" {
						accA.Add(1)
					} else {
						accB.Add(1)
					}
				} else {
					require.ErrorIs(t, err, ai.ErrUsageLimitExceeded)
				}
			}()
		}
	}
	close(start)
	wg.Wait()

	require.Equal(t, int64(1), accA.Load(), "workspace A gets exactly its own allowance")
	require.Equal(t, int64(1), accB.Load(), "workspace B gets exactly its own allowance")
	require.Equal(t, int64(2), prov.calls.Load(), "provider sees exactly the two permitted calls")
	require.Equal(t, uint64(1), guard.Used("ws-a"))
	require.Equal(t, uint64(1), guard.Used("ws-b"))
}

// TestGatewayNoProviderCallAfterExhaustion proves that once a workspace budget
// is exhausted every further request is refused without any provider call.
func TestGatewayNoProviderCallAfterExhaustion(t *testing.T) {
	gw, prov, _ := concurrencyAPI(t, 1)

	// First request consumes the single operation.
	_, err := gw.Classify(context.Background(), ai.ClassificationRequest{
		Scope:      ai.Scope{WorkspaceID: "ws-exhausted"},
		Categories: []string{"a", "b"},
		Input:      "x",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), prov.calls.Load())

	// A concurrent burst of refusals must not trigger a single provider call.
	accepted, exceeded, other := burst(gw, "ws-exhausted", 100)
	require.Zero(t, accepted)
	require.Equal(t, int64(100), exceeded)
	require.Zero(t, other)
	require.Equal(t, int64(1), prov.calls.Load(), "no hidden provider call may occur after exhaustion")
}

// TestGatewayDeterministicAccountingAcrossRounds proves the budget is a
// lifetime per-workspace cap: the first burst spends it exactly, later rounds
// are fully refused, and the recorded/observed totals never drift.
func TestGatewayDeterministicAccountingAcrossRounds(t *testing.T) {
	const (
		budget  = 3
		workers = 64
		rounds  = 3
	)
	gw, prov, guard := concurrencyAPI(t, budget)

	for round := 1; round <= rounds; round++ {
		accepted, exceeded, other := burst(gw, "ws-rounds", workers)

		// The first round (64 > 3 workers) deterministically consumes the whole
		// lifetime budget; every later round is refused in full.
		expectedAccepted := int64(0)
		if round == 1 {
			expectedAccepted = budget
		}
		require.Equal(t, expectedAccepted, accepted, "round %d: accepted", round)
		require.Equal(t, int64(workers)-expectedAccepted, exceeded, "round %d: refused", round)
		require.Zero(t, other, "round %d: no other error", round)
		require.Equal(t, int64(budget), prov.calls.Load(), "provider calls never exceed the lifetime budget")
		require.LessOrEqual(t, guard.Used("ws-rounds"), uint64(budget), "round %d: recorded usage always within budget", round)
		require.Equal(t, uint64(budget), guard.Used("ws-rounds"), "round %d: recorded usage settles exactly at the cap", round)
	}
}
