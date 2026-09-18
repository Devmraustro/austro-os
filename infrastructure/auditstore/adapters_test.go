package auditstore

import (
	"testing"

	"github.com/google/uuid"
)

// TestParseIDDropsUnparseableIdentifiers pins the convention the memory sink
// now relies on: a client-supplied correlation id that is not a UUID must be
// dropped, not treated as fatal. Actor and workspace ids are server-derived, so
// they arrive parseable; only trace/span come from request headers.
func TestParseIDDropsUnparseableIdentifiers(t *testing.T) {
	valid := uuid.New()
	if got := parseID(valid.String()); got != valid {
		t.Fatalf("parseID(%q) = %s, want %s", valid.String(), got, valid)
	}

	for _, raw := range []string{
		"",
		"not-a-uuid",
		"trace-from-client",
		"1",
		"00000000-0000-0000-0000-00000000000g",
	} {
		if got := parseID(raw); got != uuid.Nil {
			t.Fatalf("parseID(%q) = %s, want uuid.Nil", raw, got)
		}
	}
}
