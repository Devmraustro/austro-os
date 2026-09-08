package auth

import (
	"sync"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the work factor used for every stored password hash. It is the
// balance the standard library recommends for interactive login; the cost is an
// explicit constant so a future upgrade is a reviewed decision, not an
// incidental one.
const bcryptCost = bcrypt.DefaultCost

// dummyHash is a valid bcrypt hash used as the decoy for username enumeration:
// a login for an unknown username still performs a real bcrypt comparison,
// keeping verification work indistinguishable from a known-username attempt.
var (
	dummyHashOnce sync.Once
	dummyHash     string
)

func dummyBcryptHash() string {
	dummyHashOnce.Do(func() {
		h, err := bcrypt.GenerateFromPassword([]byte("austro-invalid-credential"), bcryptCost)
		if err != nil {
			// bcrypt only fails on an invalid cost (<0 or >31); bcryptCost is a
			// constant that can never trip that path.
			panic("auth: bcrypt dummy hash generation failed")
		}
		dummyHash = string(h)
	})
	return dummyHash
}

// HashPassword returns an argon-style bcrypt hash for a plaintext password.
// Callers persist the returned string; the plaintext is never retained.
func HashPassword(plaintext string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPasswordHash reports whether a plaintext password matches a stored
// bcrypt hash. It is intentionally a strict compare; failures never reveal
// whether the username or the password was wrong.
func CheckPasswordHash(hashed, plaintext string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hashed), []byte(plaintext)) == nil
}

// DummyPasswordHash returns a valid bcrypt hash for decoy comparison. A login
// for an unknown username compares the presented password against it so that
// failed attempts consume verification work indistinguishable from a
// known-username attempt (mitigating timing-based username enumeration).
func DummyPasswordHash() string {
	return dummyBcryptHash()
}
