package instances

import "testing"

func TestShardAggregatorCanary(t *testing.T) {
	t.Fatal("intentional failure to check that one failing shard turns the test status red")
}
