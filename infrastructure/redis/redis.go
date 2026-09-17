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

func Initialize(cfg *config.Config) *redis.Client {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: os.Getenv("AUSTRO_REDIS_PASSWORD"),
		DB:       0,
	})

	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		logger.NewEntry("redis-connect-failed").SetLevel("error").WithError(err).Log()
		os.Exit(1)
	}

	logger.NewEntry("redis-connection-established").Log()
	return client
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
