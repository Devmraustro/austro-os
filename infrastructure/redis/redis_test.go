package redis

import "testing"

// TestPartitionedKeyRoundTripsThroughIsPartitionedKey is the regression test
// for IsPartitionedKey, which sliced 12 bytes and compared them against the
// 10-byte "workspace:" prefix, so it could never match and always returned
// false for every key including ones PartitionedKey had just produced.
func TestPartitionedKeyRoundTripsThroughIsPartitionedKey(t *testing.T) {
	key := PartitionedKey("11111111-1111-1111-1111-111111111111", "session:token:abc")
	if want := "workspace:11111111-1111-1111-1111-111111111111:session:token:abc"; key != want {
		t.Fatalf("PartitionedKey = %q, want %q", key, want)
	}
	if !IsPartitionedKey(key) {
		t.Errorf("a key produced by PartitionedKey must be recognised as partitioned: %q", key)
	}
}

func TestIsPartitionedKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"workspace:ws-a:session:token", true},
		{"workspace:", true},
		{"org:global:setting", false},
		{"", false},
		{"work", false},
		{"workspace", false},
		{"session:workspace:x", false},
	}
	for _, tc := range cases {
		if got := IsPartitionedKey(tc.key); got != tc.want {
			t.Errorf("IsPartitionedKey(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}
