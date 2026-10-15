package instances

import "testing"

func TestZZShardCanary(t *testing.T) {
	t.Fatal("intentional failure to check that a failing shard turns the test status red")
}
