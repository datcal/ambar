package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/index"
)

// The sidebar cache is the only concurrency in the server, and it stands between every page view
// and 250 ms of aggregate queries, so its three behaviours get pinned down here.
func TestSidebarCacheServesStaleAndRefreshesBehind(t *testing.T) {
	var builds atomic.Int64
	build := func(context.Context) (*sidebarData, error) {
		n := builds.Add(1)
		return &sidebarData{Stats: index.Stats{Assets: int(n)}}, nil
	}

	c := &sidebarCache{}

	// First call has nothing to serve, so it waits for the real thing.
	first, err := c.get(context.Background(), build)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stats.Assets != 1 {
		t.Fatalf("first snapshot = %d, want 1", first.Stats.Assets)
	}

	// Inside the TTL: served from memory, no rebuild.
	if _, err := c.get(context.Background(), build); err != nil {
		t.Fatal(err)
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("%d builds inside the TTL, want 1", got)
	}

	// Stale: the *old* snapshot comes back immediately and a refresh runs behind it. That is
	// the point — the first click after a write must not be the slow one.
	c.mu.Lock()
	c.at = time.Now().Add(-2 * sidebarTTL)
	c.mu.Unlock()

	stale, err := c.get(context.Background(), build)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Stats.Assets != 1 {
		t.Errorf("a stale read returned %d; it should serve the old snapshot, not wait",
			stale.Stats.Assets)
	}

	// The refresh lands shortly after.
	deadline := time.Now().Add(2 * time.Second)
	for builds.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := builds.Load(); got != 2 {
		t.Fatalf("%d builds after going stale, want 2", got)
	}
	fresh, err := c.get(context.Background(), build)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Stats.Assets != 2 {
		t.Errorf("after the background refresh the snapshot is %d, want 2", fresh.Stats.Assets)
	}
}

// invalidate must make the next read exact, not merely stale: "you pressed a button and the
// number moved" is the whole reason writes call it.
func TestSidebarCacheInvalidateIsSynchronous(t *testing.T) {
	var builds atomic.Int64
	build := func(context.Context) (*sidebarData, error) {
		n := builds.Add(1)
		return &sidebarData{Stats: index.Stats{Assets: int(n)}}, nil
	}

	c := &sidebarCache{}
	if _, err := c.get(context.Background(), build); err != nil {
		t.Fatal(err)
	}
	c.invalidate()

	got, err := c.get(context.Background(), build)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stats.Assets != 2 {
		t.Errorf("after invalidate the read returned %d, want the rebuilt 2", got.Stats.Assets)
	}
}

// A failed rebuild must not blank the navigation on a page that is otherwise fine.
func TestSidebarCacheKeepsTheLastGoodSnapshotOnError(t *testing.T) {
	var fail atomic.Bool
	build := func(context.Context) (*sidebarData, error) {
		if fail.Load() {
			return nil, errors.New("database is having a moment")
		}
		return &sidebarData{Stats: index.Stats{Assets: 7}}, nil
	}

	c := &sidebarCache{}
	if _, err := c.get(context.Background(), build); err != nil {
		t.Fatal(err)
	}

	fail.Store(true)
	c.mu.Lock()
	c.at = time.Now().Add(-2 * sidebarTTL)
	c.mu.Unlock()

	got, err := c.get(context.Background(), build)
	if err != nil {
		t.Fatalf("a stale read should not surface the refresh error: %v", err)
	}
	if got.Stats.Assets != 7 {
		t.Errorf("snapshot = %d, want the last good 7", got.Stats.Assets)
	}
}
