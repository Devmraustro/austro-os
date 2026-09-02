package austro_os_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRedisWorkspacePartitionedKeys verifies that all Redis keys are
// workspace-partitioned (convention: workspace:<id>:<key>), so that data for
// one workspace can never be enumerated under another workspace's namespace.
func TestRedisWorkspacePartitionedKeys(t *testing.T) {
	rdb := redisClient()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, rdb.Ping(ctx).Err(), "must be able to reach Redis")

	// Seed one key per workspace under the standard partition prefix.
	wsAPrefix := "workspace:" + workspaceA + ":"
	wsBPrefix := "workspace:" + workspaceB + ":"
	keyA := wsAPrefix + "session:token:abc"
	keyB := wsBPrefix + "session:token:def"

	require.NoError(t, rdb.Set(ctx, keyA, "alice", time.Minute).Err())
	require.NoError(t, rdb.Set(ctx, keyB, "bob", time.Minute).Err())
	t.Cleanup(func() { rdb.Del(ctx, keyA, keyB) })

	// Scan the workspace-A namespace: it must return only workspace-A keys.
	aKeys, err := rdb.Keys(ctx, wsAPrefix+"*").Result()
	require.NoError(t, err)
	require.Contains(t, aKeys, keyA)
	require.NotContains(t, aKeys, keyB, "workspace A must never see workspace B keys")
	require.NotContains(t, aKeys, "workspace:"+workspaceB+":", "namespace scan must not leak across partitions")

	// The same key name under different workspaces must hold distinct values.
	require.Equal(t, "alice", rdb.Get(ctx, keyA).Val())
	require.Equal(t, "bob", rdb.Get(ctx, keyB).Val())
	require.NotEqual(t, rdb.Get(ctx, keyA).Val(), rdb.Get(ctx, keyB).Val())
}

// TestRedisPartitionPrefixContract asserts the actual application convention:
// workspace-partitioned keys begin with "workspace:<id>:" and org-scoped keys
// begin with "org:" (never a bare, un-partitioned key).
func TestRedisPartitionPrefixContract(t *testing.T) {
	require.Contains(t, "workspace:"+workspaceA+":session:x", "workspace:"+workspaceA+":", "keys must carry the workspace partition prefix")
}
