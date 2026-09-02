package austro_os_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"testing"

	"austro-os/internal/audit"
	"austro-os/internal/config"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// auditHMACSecret returns the same secret AppendEvent uses to sign events.
func auditHMACSecret() string {
	return config.Get().JWTSecret
}

// buildChain produces a genesis event followed by n chained events.
func buildChain(t *testing.T, n int) []*audit.AuditEvent {
	t.Helper()
	events := []*audit.AuditEvent{audit.GenesisEvent("Vision First")}
	prev := events[0]
	for i := 0; i < n; i++ {
		next := audit.AppendEvent(prev, "created", "user",
			uuid.New(), "workspace", uuid.New(), "success", "Vision First")
		events = append(events, next)
		prev = next
	}
	return events
}

// TestGenesisHashChain verifies the genesis handler (i==0 uses GENESIS_HASH,
// never events[i-1]) and that subsequent events chain from the prior hash.
func TestGenesisHashChain(t *testing.T) {
	events := buildChain(t, 3)
	require.Len(t, events, 4)
	require.True(t, events[0].Genesis, "first event must be genesis")
	verifyHashChain(t, events)
}

func verifyHashChain(t *testing.T, events []*audit.AuditEvent) {
	t.Helper()
	require.NotEmpty(t, events)
	require.True(t, events[0].Genesis, "first event must be genesis")

	expectedGenesis := sha256.Sum256([]byte(audit.GENESIS_HASH + "." + events[0].Timestamp.Format("2006-01-02T15:04:05Z07:00")))
	require.True(t, hmac.Equal(events[0].HashValue, expectedGenesis[:]), "genesis hash mismatch")

	for i := 1; i < len(events); i++ {
		prev := events[i-1]
		hashInput := fmt.Sprintf("%s|%s|%s|%v|%s|%v|%s|%s|%s",
			prev.HashValue, prev.EventID, prev.Timestamp.Format("2006-01-02T15:04:05Z07:00"),
			prev.Genesis, events[i].EventType, events[i].ActorType,
			events[i].ActorID, events[i].TargetType, events[i].TargetID)
		hashBytes := sha256.Sum256([]byte(hashInput))
		require.True(t, hmac.Equal(events[i].HashValue, hashBytes[:]), "hash chain broken at index %d", i)
	}
}

// TestAuditChainTamperDetected verifies that any tampering with a payload, the
// recorded hash, a previous hash, or the ordering breaks integrity detection.
func TestAuditChainTamperDetected(t *testing.T) {
	base := buildChain(t, 3)

	clone := func() []*audit.AuditEvent {
		out := make([]*audit.AuditEvent, len(base))
		for i := range base {
			c := *base[i]
			c.HashValue = append([]byte(nil), base[i].HashValue...)
			out[i] = &c
		}
		return out
	}

	t.Run("tampered event payload", func(t *testing.T) {
		// Tamper a field that IS covered by the hash input (event_type):
		// integrity detection must trigger. NOTE: the current hash input covers
		// event_type, actor/target ids, and timeline/genesis linkage — not
		// outcome, constitutional_principle, or workspace_id. Those fields are a
		// documented strength gap for the audit chain (see report).
		evs := clone()
		evs[2].EventType = "tampered-event-type"
		require.False(t, audit.VerifyHashChain(evs), "payload tampering must be detected")
	})

	t.Run("tampered current hash", func(t *testing.T) {
		evs := clone()
		evs[1].HashValue[0] ^= 0xFF
		require.False(t, audit.VerifyHashChain(evs), "current-hash tampering must be detected")
	})

	t.Run("tampered previous hash", func(t *testing.T) {
		evs := clone()
		evs[1].HashValue = append([]byte(nil), evs[0].HashValue...)
		require.False(t, audit.VerifyHashChain(evs), "previous-hash tampering must be detected")
	})

	t.Run("reordered events", func(t *testing.T) {
		evs := clone()
		evs[1], evs[2] = evs[2], evs[1]
		require.False(t, audit.VerifyHashChain(evs), "ordering change must be detected")
	})

	t.Run("untampered chain passes", func(t *testing.T) {
		require.True(t, audit.VerifyHashChain(clone()), "untampered chain must verify")
	})
}

// TestAuditHMACAuthenticity verifies the digital signature is an HMAC-SHA256
// authenticity tag (not a public-key signature), and that a wrong key fails.
func TestAuditHMACAuthenticity(t *testing.T) {
	genesis := audit.GenesisEvent("Vision First")
	ev := audit.AppendEvent(genesis, "created", "user",
		uuid.New(), "workspace", uuid.New(), "success", "Vision First")

	secret := []byte(auditHMACSecret())

	require.NotEmpty(t, genesis.DigitalSignature, "genesis event must also carry an HMAC authenticity tag")
	require.NotEmpty(t, ev.DigitalSignature, "event must carry an HMAC authenticity tag")

	verifySigned := func(ev *audit.AuditEvent) {
		expected := hmac.New(sha256.New, secret)
		expected.Write(ev.HashValue)
		require.True(t, hmac.Equal(ev.DigitalSignature, expected.Sum(nil)),
			"digital signature must be HMAC-SHA256 over the hash value")
	}
	verifySigned(genesis)
	verifySigned(ev)

	// Wrong key must not authenticate.
	wrong := hmac.New(sha256.New, []byte("wrong-key"))
	wrong.Write(ev.HashValue)
	require.False(t, hmac.Equal(ev.DigitalSignature, wrong.Sum(nil)),
		"an HMAC produced with a different key must not verify")
}
