package redis

import (
	"strings"
	"testing"

	"austro-os/internal/config"
	redisclient "github.com/redis/go-redis/v9"
)

// The client must authenticate with the password configuration carries. A
// setting that loads correctly but never reaches redis.Options is the failure
// mode this file exists to catch: the process starts cleanly, readiness passes,
// and every command fails at runtime with NOAUTH.

// optionsFor builds options through the same entry point the constructor uses.
func optionsFor(t *testing.T, addr, password string) *redisclient.Options {
	t.Helper()
	opts, err := buildOptions(&config.Config{RedisAddr: addr, RedisPassword: password})
	if err != nil {
		t.Fatalf("buildOptions(%q): %v", addr, err)
	}
	return opts
}

// The options type comes from the same client the constructor uses.\r\n
func TestBuildOptionsCarriesTheConfiguredPassword(t *testing.T) {
	const pw = "redis-auth-2f8c1d4b6a"
	opts := optionsFor(t, "127.0.0.1:6379", pw)
	if opts.Password != pw {
		t.Errorf("Password = %q, want %q", opts.Password, pw)
	}
}

// TestBuildOptionsAllowsAnEmptyPassword proves the passwordless deployment shape
// survives: no password means "do not send AUTH", not "send an empty AUTH".
func TestBuildOptionsAllowsAnEmptyPassword(t *testing.T) {
	opts := optionsFor(t, "127.0.0.1:6379", "")
	if opts.Password != "" {
		t.Errorf("Password = %q, want empty for a passwordless server", opts.Password)
	}
}

// TestBuildOptionsNeverTakesCredentialsFromTheAddress pins the separation: the
// address carries host and port only, so there is exactly one place a password
// can come from.
func TestBuildOptionsNeverTakesCredentialsFromTheAddress(t *testing.T) {
	const pw = "only-source-of-truth-7d3e9a1c"
	for _, addr := range []string{
		"127.0.0.1:6379",
		"redis://127.0.0.1:6379",
		"rediss://127.0.0.1:6379",
	} {
		opts := optionsFor(t, addr, pw)
		if opts.Password != pw {
			t.Errorf("%s: Password = %q, want %q", addr, opts.Password, pw)
		}
		if strings.Contains(opts.Addr, "@") {
			t.Errorf("%s: Addr = %q must not contain userinfo", addr, opts.Addr)
		}
	}
}

// TestBuildOptionsRejectsAnAddressCarryingCredentials proves a password placed
// in the address is refused rather than quietly preferred over the real one.
func TestBuildOptionsRejectsAnAddressCarryingCredentials(t *testing.T) {
	_, err := buildOptions(&config.Config{
		RedisAddr:     "redis://user:pass@127.0.0.1:6379",
		RedisPassword: "real-password-8c2a",
	})
	if err == nil {
		t.Fatal("buildOptions accepted credentials in the address; " +
			"the address is host:port only and AUSTRO_REDIS_PASSWORD is the single source")
	}
}
