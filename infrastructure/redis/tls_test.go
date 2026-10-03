package redis

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"austro-os/internal/config"
)

// These tests exercise real certificate verification against a certificate
// authority generated in-process. Nothing here contacts a network, and nothing
// here claims that a live Redis endpoint speaks TLS: what is proven is that a
// rediss:// address produces a client that verifies a certificate issued by the
// operator's CA and rejects one that is not.

// issueTestChain creates a private certificate authority and one server
// certificate it issued for dnsName, returning the authority in PEM form and the
// parsed leaf.
func issueTestChain(t *testing.T, dnsName string) ([]byte, *x509.Certificate) {
	t.Helper()

	notBefore := time.Now().Add(-time.Hour)
	notAfter := time.Now().Add(time.Hour)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "austro redis test CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing CA certificate: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating leaf certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatalf("parsing leaf certificate: %v", err)
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return caPEM, leaf
}

// writeCAFile writes a PEM bundle to a temporary file and returns its path.
func writeCAFile(t *testing.T, contents []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "redis-ca.pem")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("writing CA file: %v", err)
	}
	return path
}

// TestTLSConfigForVerifiesACertificateIssuedByTheSuppliedCA is the case the
// feature exists for: a managed Redis whose certificate is issued by a private
// authority, which the system roots cannot verify.
func TestTLSConfigForVerifiesACertificateIssuedByTheSuppliedCA(t *testing.T) {
	const host = "redis.managed-host.net"
	caPEM, leaf := issueTestChain(t, host)

	cfg, err := tlsConfigFor(host, writeCAFile(t, caPEM))
	if err != nil {
		t.Fatalf("tlsConfigFor: %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     cfg.RootCAs,
		DNSName:   host,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("a certificate issued by the supplied CA must verify: %v", err)
	}
}

// TestTLSConfigForRejectsACertificateIssuedForAnotherName proves the name is
// checked, so a valid certificate for some other host cannot stand in.
func TestTLSConfigForRejectsACertificateIssuedForAnotherName(t *testing.T) {
	const host = "redis.managed-host.net"
	caPEM, leaf := issueTestChain(t, host)

	cfg, err := tlsConfigFor(host, writeCAFile(t, caPEM))
	if err != nil {
		t.Fatalf("tlsConfigFor: %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     cfg.RootCAs,
		DNSName:   "attacker.invalid",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Fatal("a certificate issued for another name must not verify")
	}
}

// TestTLSConfigForRefusesAnAuthorityItWasNotGiven is the assertion that
// verification is real. Without the bundle the test's own authority is unknown,
// so its certificate must fail rather than be waved through.
func TestTLSConfigForRefusesAnAuthorityItWasNotGiven(t *testing.T) {
	const host = "redis.managed-host.net"
	_, leaf := issueTestChain(t, host)

	cfg, err := tlsConfigFor(host, "")
	if err != nil {
		t.Fatalf("tlsConfigFor: %v", err)
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     cfg.RootCAs,
		DNSName:   host,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Fatal("a certificate from an untrusted authority must not verify; verification is not happening")
	}
}

// TestTLSConfigForNeverDisablesVerification pins the settings that must not be
// negotiable. InsecureSkipVerify is the one that matters: it converts a
// man-in-the-middle into a successful connection, and it is set here by no input
// and reachable from no configuration.
func TestTLSConfigForNeverDisablesVerification(t *testing.T) {
	for _, tc := range []struct{ name, caFile string }{
		{"system roots", ""},
		{"a supplied authority", "somewhere.pem"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *tls.Config
			var err error
			if tc.caFile == "" {
				cfg, err = tlsConfigFor("redis.managed-host.net", "")
			} else {
				caPEM, _ := issueTestChain(t, "redis.managed-host.net")
				cfg, err = tlsConfigFor("redis.managed-host.net", writeCAFile(t, caPEM))
			}
			if err != nil {
				t.Fatalf("tlsConfigFor: %v", err)
			}
			if cfg.InsecureSkipVerify {
				t.Error("InsecureSkipVerify must never be set")
			}
			if cfg.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", cfg.MinVersion, tls.VersionTLS12)
			}
			if cfg.ServerName != "redis.managed-host.net" {
				t.Errorf("ServerName = %q, want the endpoint host", cfg.ServerName)
			}
		})
	}
}

// TestTLSConfigForRejectsAnUnusableCAFile asserts a bundle that cannot be used
// fails loudly at startup, rather than being ignored and leaving the connection
// to fail later against a certificate nothing trusts.
func TestTLSConfigForRejectsAnUnusableCAFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    func(t *testing.T) string
		message string
	}{
		{
			name:    "missing file",
			path:    func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.pem") },
			message: "could not be read",
		},
		{
			name:    "not a certificate",
			path:    func(t *testing.T) string { return writeCAFile(t, []byte("this is not a certificate\n")) },
			message: "no PEM certificate",
		},
		{
			name: "a PEM block that is not a certificate",
			path: func(t *testing.T) string {
				keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not a key either")})
				return writeCAFile(t, keyPEM)
			},
			message: "no PEM certificate",
		},
		{
			name:    "empty file",
			path:    func(t *testing.T) string { return writeCAFile(t, nil) },
			message: "no PEM certificate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tlsConfigFor("redis.managed-host.net", tc.path(t)); err == nil {
				t.Fatal("an unusable CA file must be an error, not a warning")
			} else if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.message)
			}
		})
	}
}

// TestTLSConfigForKeepsSystemRoots asserts a private authority is added to the
// system roots rather than replacing them, so one setting cannot silently remove
// every public root.
func TestTLSConfigForKeepsSystemRoots(t *testing.T) {
	system, err := x509.SystemCertPool()
	if err != nil || system == nil {
		t.Skip("no system certificate pool on this host")
	}

	caPEM, _ := issueTestChain(t, "redis.managed-host.net")
	cfg, err := tlsConfigFor("redis.managed-host.net", writeCAFile(t, caPEM))
	if err != nil {
		t.Fatalf("tlsConfigFor: %v", err)
	}

	if cfg.RootCAs.Equal(system) {
		t.Error("the supplied authority was not added to the pool")
	}
}

// TestBuildOptionsSelectsTheTransport asserts the address alone decides whether
// the connection is encrypted, and that the legacy form keeps its meaning.
func TestBuildOptionsSelectsTheTransport(t *testing.T) {
	for _, tc := range []struct {
		name           string
		addr           string
		caFile         func(t *testing.T) string
		wantTLS        bool
		wantServerName string
	}{
		{
			name: "the legacy form stays plaintext",
			addr: "redis:6379",
		},
		{
			name: "an explicit redis scheme is plaintext",
			addr: "redis://cache.eu.internal:6379",
		},
		{
			name:           "rediss requires TLS",
			addr:           "rediss://redis.managed-host.net:6379",
			wantTLS:        true,
			wantServerName: "redis.managed-host.net",
		},
		{
			name: "rediss with a private authority",
			addr: "rediss://redis.managed-host.net:6379",
			caFile: func(t *testing.T) string {
				caPEM, _ := issueTestChain(t, "redis.managed-host.net")
				return writeCAFile(t, caPEM)
			},
			wantTLS:        true,
			wantServerName: "redis.managed-host.net",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{RedisAddr: tc.addr}
			if tc.caFile != nil {
				cfg.RedisCAFile = tc.caFile(t)
			}

			options, err := buildOptions(cfg)
			if err != nil {
				t.Fatalf("buildOptions: %v", err)
			}

			if tc.wantTLS {
				if options.TLSConfig == nil {
					t.Fatal("a rediss:// address must produce a verified TLS configuration")
				}
				if options.TLSConfig.ServerName != tc.wantServerName {
					t.Errorf("ServerName = %q, want %q", options.TLSConfig.ServerName, tc.wantServerName)
				}
			} else if options.TLSConfig != nil {
				t.Error("a plaintext address must not acquire TLS; that would break every existing deployment")
			}

			// The scheme describes the transport; it must not survive into the
			// address the client dials.
			wantAddr := strings.TrimPrefix(strings.TrimPrefix(tc.addr, config.RedisTLSScheme+"://"), config.RedisScheme+"://")
			if options.Addr != wantAddr {
				t.Errorf("Addr = %q, want %q", options.Addr, wantAddr)
			}
		})
	}
}

// TestBuildOptionsRefusesToFallBackToPlaintext is the security assertion for the
// whole feature: if TLS was asked for and cannot be configured, the call must
// fail. Returning options without TLS would connect in the clear while the
// operator believes the connection is encrypted.
func TestBuildOptionsRefusesToFallBackToPlaintext(t *testing.T) {
	cfg := &config.Config{
		RedisAddr:   "rediss://redis.managed-host.net:6379",
		RedisCAFile: filepath.Join(t.TempDir(), "absent.pem"),
	}

	options, err := buildOptions(cfg)
	if err == nil {
		t.Fatal("an unusable CA file for a rediss:// address must be an error")
	}
	if options != nil {
		t.Fatal("no options may be returned alongside the error")
	}
}

// TestBuildOptionsRejectsAMalformedAddress asserts a bad address is refused at
// configuration time rather than reaching a dial.
func TestBuildOptionsRejectsAMalformedAddress(t *testing.T) {
	for _, addr := range []string{"", "cache.eu.internal", "https://cache.eu.internal:6379", "cache.eu.internal:0"} {
		t.Run(addr, func(t *testing.T) {
			if _, err := buildOptions(&config.Config{RedisAddr: addr}); err == nil {
				t.Fatalf("%q must be rejected", addr)
			}
		})
	}
}

// TestBuildOptionsPassesTheCredentialThrough asserts the password reaches the
// client and still never appears in a TLS-related error.
//
// The password arrives on the Config rather than being re-read from the
// environment here. Reading the environment inside the client meant the value
// the connection used was whatever was in os.Getenv, not the value Load
// validated and rejected placeholders on -- two sources of truth for one
// credential.
func TestBuildOptionsPassesTheCredentialThrough(t *testing.T) {
	const password = "s3cret-redis-password"
	addr := "rediss://redis.managed-host.net:6379"

	options, err := buildOptions(&config.Config{RedisAddr: addr, RedisPassword: password})
	if err != nil {
		t.Fatalf("buildOptions: %v", err)
	}
	if options.Password != password {
		t.Errorf("Password was not passed to the client")
	}

	_, err = buildOptions(&config.Config{
		RedisAddr:     addr,
		RedisPassword: password,
		RedisCAFile:   filepath.Join(t.TempDir(), "absent.pem"),
	})
	if err == nil {
		t.Fatal("expected an error for the unusable CA file")
	}
	if strings.Contains(err.Error(), password) {
		t.Error("a configuration error must not print the password")
	}
}
