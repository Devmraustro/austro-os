package redis

import (
	"context"
	"fmt"
	"os"

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
	return fmt.Sprintf("workspace:%s:%s", workspaceID, key)
}

func IsPartitionedKey(key string) bool {
	return len(key) > 12 && key[:12] == "workspace:"
}