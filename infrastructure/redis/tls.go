package redis

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// tlsConfigFor builds the verified TLS configuration for a rediss:// endpoint.
//
// The policy this encodes, and why each part is not optional:
//
//   - TLS 1.2 is the floor. TLS 1.0 and 1.1 are broken, and a managed service
//     that offers only those is not a service this client should speak to.
//   - Certificates are always verified. InsecureSkipVerify is never set here,
//     which turns a man-in-the-middle into a successful connection that reports
//     success. There is no configuration value that can enable it, because a
//     setting that can disable verification is a setting that will eventually
//     be set to make an error go away.
//   - A private CA is additive. The system roots are kept and the operator's
//     bundle appended, so supplying one CA does not silently remove every
//     public root.
//
// host is the endpoint hostname with no port and no IPv6 brackets: it is the
// name the certificate must be issued to.
func tlsConfigFor(host, caFile string) (*tls.Config, error) {
	// SystemCertPool fails on some minimal images. An empty pool still verifies,
	// against the supplied bundle when there is one, so this is not fatal.
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}

	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("AUSTRO_REDIS_CA_FILE could not be read: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("AUSTRO_REDIS_CA_FILE %q contains no PEM certificate", caFile)
		}
	}

	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: host,
		RootCAs:    pool,
	}, nil
}
