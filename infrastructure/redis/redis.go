package redis

import (
	"context"
	"fmt"
	"os"
	"strings"

	"austro-os/internal/config"
	logger "austro-os/internal/log"

	"github.com/redis/go-redis/v9"
)

// Initialize opens the memory bank's Redis connection, or exits.
//
// The connection is described by one setting, AUSTRO_REDIS_ADDR, whose form
// decides whether it is encrypted: rediss:// requires TLS with the server
// certificate verified, redis:// and the legacy bare host:port are plaintext.
// Before this, the client had no way to ask for TLS at all, so a managed Redis
// that requires it could not be reached by this application.
func Initialize(cfg *config.Config) *redis.Client {
	options, err := buildOptions(cfg)
	if err != nil {
		logger.NewEntry("redis-configuration-invalid").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	endpoint, err := config.ParseRedisAddr(cfg.RedisAddr)
	if err != nil {
		// Unreachable: buildOptions already rejected this address.
		logger.NewEntry("redis-address-invalid").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	client := redis.NewClient(options)

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		logger.NewEntry("redis-connect-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	if endpoint.TLS {
		logger.NewEntry("redis-tls-established").With("host", endpoint.Host).Log()
	}
	logger.NewEntry("redis-connection-established").Log()
	return client
}

// buildOptions turns the configuration into client options.
//
// It is separated from Initialize so the decision that matters - whether this
// connection is encrypted, and against which roots - can be asserted without a
// Redis server to talk to. Every error it returns is fatal by construction:
// there is no plaintext fallback, because a client that quietly retries without
// TLS would report success on a connection the operator believes is private.
func buildOptions(cfg *config.Config) (*redis.Options, error) {
	endpoint, err := config.ParseRedisAddr(cfg.RedisAddr)
	if err != nil {
		return nil, err
	}

	options := &redis.Options{
		Addr:     endpoint.Addr,
		Password: os.Getenv("AUSTRO_REDIS_PASSWORD"),
		DB:       0,
	}

	if endpoint.TLS {
		options.TLSConfig, err = tlsConfigFor(endpoint.Host, cfg.RedisCAFile)
		if err != nil {
			return nil, err
		}
	}

	return options, nil
}

func PartitionedKey(workspaceID string, key string) string {
	return fmt.Sprintf("%s%s:%s", workspaceKeyPrefix, workspaceID, key)
}

// workspaceKeyPrefix is the namespace prefix PartitionedKey applies.
const workspaceKeyPrefix = "workspace:"

// IsPartitionedKey reports whether a key carries the workspace namespace
// prefix. The previous comparison sliced 12 bytes and compared them against the
// 10-byte prefix, so it could never match and the helper always returned false.
func IsPartitionedKey(key string) bool {
	return strings.HasPrefix(key, workspaceKeyPrefix)
}
