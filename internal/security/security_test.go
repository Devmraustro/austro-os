package security

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRedactSecretRemovesTokenShapes(t *testing.T) {
	cases := []string{
		"Bearer ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789abcdef",
		"api_key=sk-0123456789abcdef0123456789abcdef",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----",
		"the token is 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822c",
	}
	for _, c := range cases {
		out := RedactSecret(c)
		if strings.Contains(out, "ghp_") || strings.Contains(out, "sk-") ||
			strings.Contains(out, "BEGIN RSA") {
			t.Fatalf("secret-shaped substring survived redaction for %q -> %q", c, out)
		}
	}
}

func TestRedactSecretLeavesBlandText(t *testing.T) {
	inputs := []string{
		"hello world",
		"workspace 11111111-1111-1111-1111-111111111111",
		"task updated to review",
		"",
	}
	for _, in := range inputs {
		if out := RedactSecret(in); out != in {
			t.Fatalf("redaction must not alter bland text %q -> %q", in, out)
		}
	}
}

func TestRedactAllFields(t *testing.T) {
	m := map[string]string{
		"title": "Launch post",
		"token": "Bearer AKIAIOSFODNN7EXAMPLEtoken",
	}
	out := RedactAllFields(m)
	if out["title"] != "Launch post" {
		t.Fatalf("title must be untouched: %q", out["title"])
	}
	if strings.Contains(out["token"], "AKIA") {
		t.Fatalf("token must be redacted: %q", out["token"])
	}
}

func TestHashExternalIDDeterministicAndIrreversible(t *testing.T) {
	h := NewExternalIDHasher([]byte("test-key"))
	a, err := h.Hash("stub://stub/digest")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := h.Hash("stub://stub/digest")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a != b {
		t.Fatalf("hash must be deterministic: %q vs %q", a, b)
	}
	if a == "stub://stub/digest" || strings.Contains(a, "/") || strings.Contains(a, ":") {
		t.Fatalf("hash must not expose the raw id: %q", a)
	}
	if _, err := h.Hash(""); err != ErrEmptyValue {
		t.Fatalf("expected ErrEmptyValue, got %v", err)
	}
}

func TestThrottlerFailClosed(t *testing.T) {
	tk := NewThrottler(0, 2) // window default, limit 2
	if !tk.Allow("ws-a:user-1") || !tk.Allow("ws-a:user-1") {
		t.Fatal("first two hits must be allowed")
	}
	if tk.Allow("ws-a:user-1") {
		t.Fatal("third hit within window must be denied")
	}
	// different key unaffected
	if !tk.Allow("ws-b:user-2") {
		t.Fatal("a different key must retain its own budget")
	}
	tk.Reset("ws-a:user-1")
	if !tk.Allow("ws-a:user-1") {
		t.Fatal("after reset the key must be allowed again")
	}
}

// TestThrottlerHonorsWindow verifies the fixed window is actually enforced: a
// key that exhausts its budget is refused, and once the window elapses the same
// key is allowed again without an explicit reset (the historic defect kept the
// budget permanently and never reset the window).
func TestThrottlerHonorsWindow(t *testing.T) {
	tk := NewThrottler(30*time.Millisecond, 1)
	if !tk.Allow("ws:user") {
		t.Fatal("first hit within the window must be allowed")
	}
	if tk.Allow("ws:user") {
		t.Fatal("second hit within the window must be denied")
	}
	time.Sleep(60 * time.Millisecond)
	if !tk.Allow("ws:user") {
		t.Fatal("after the window elapses the key budget must reset")
	}
}

func TestGuardDenyByDefault(t *testing.T) {
	var g Guard
	if err := g.RequireWorkspace(uuid.Nil); err != ErrMissingWorkspace {
		t.Fatalf("expected ErrMissingWorkspace for nil, got %v", err)
	}
	if err := g.RequireWorkspace(uuid.New()); err != nil {
		t.Fatalf("valid workspace must pass: %v", err)
	}
	if err := g.RequireHuman("  "); err != ErrMissingActor {
		t.Fatalf("expected ErrMissingActor for blank, got %v", err)
	}
	if err := g.RequireHuman("alice"); err != nil {
		t.Fatalf("valid actor must pass: %v", err)
	}
}
