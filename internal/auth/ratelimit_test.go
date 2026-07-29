package auth

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestLimiterAllowsUntilTheMaxIsReached(t *testing.T) {
	l := NewLimiter(3, time.Minute)

	// Allowed does not consume, so asking repeatedly changes nothing.
	for i := 0; i < 10; i++ {
		if ok, _ := l.Allowed("alice"); !ok {
			t.Fatalf("Allowed consumed an attempt on call %d", i)
		}
	}

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allowed("alice"); !ok {
			t.Fatalf("blocked after only %d failures, max is 3", i)
		}
		l.RecordFailure("alice")
	}

	ok, retryAfter := l.Allowed("alice")
	if ok {
		t.Error("a fourth attempt was allowed past a max of 3")
	}
	if retryAfter <= 0 || retryAfter > time.Minute {
		t.Errorf("retryAfter = %s, want a positive value within the window", retryAfter)
	}
}

// TestLimiterKeysAreIndependent is the §11 requirement that one account being
// attacked must not lock out the other user.
func TestLimiterKeysAreIndependent(t *testing.T) {
	l := NewLimiter(2, time.Minute)

	for i := 0; i < 5; i++ {
		l.RecordFailure("alice")
	}

	if ok, _ := l.Allowed("alice"); ok {
		t.Error("alice should be limited")
	}
	if ok, _ := l.Allowed("bob"); !ok {
		t.Error("bob was limited by alice's failures")
	}
}

func TestLimiterWindowExpires(t *testing.T) {
	now := time.Now()
	l := NewLimiter(2, time.Minute).WithClock(func() time.Time { return now })

	l.RecordFailure("alice")
	l.RecordFailure("alice")
	if ok, _ := l.Allowed("alice"); ok {
		t.Fatal("not limited after reaching the max")
	}

	now = now.Add(59 * time.Second)
	if ok, _ := l.Allowed("alice"); ok {
		t.Error("the window expired early")
	}

	now = now.Add(2 * time.Second)
	if ok, _ := l.Allowed("alice"); !ok {
		t.Error("still limited after the window passed")
	}
}

// TestLimiterResetOnSuccess: earlier typos must not count against a correct
// password.
func TestLimiterResetOnSuccess(t *testing.T) {
	l := NewLimiter(3, time.Minute)

	l.RecordFailure("alice")
	l.RecordFailure("alice")
	l.Reset("alice")

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allowed("alice"); !ok {
			t.Fatalf("still limited after a reset, at attempt %d", i)
		}
		l.RecordFailure("alice")
	}
	if ok, _ := l.Allowed("alice"); ok {
		t.Error("the limit stopped applying after the reset")
	}
}

// TestLimiterRecordAfterWindowStartsFresh checks that a failure landing after
// the window has passed opens a new window rather than topping up the old one.
func TestLimiterRecordAfterWindowStartsFresh(t *testing.T) {
	now := time.Now()
	l := NewLimiter(2, time.Minute).WithClock(func() time.Time { return now })

	l.RecordFailure("alice")
	l.RecordFailure("alice")

	now = now.Add(2 * time.Minute)
	l.RecordFailure("alice")

	// One failure in the new window, so there is still room.
	if ok, _ := l.Allowed("alice"); !ok {
		t.Error("the new window inherited the old count")
	}
}

// TestLimiterEvictsExpiredEntries: a spray of distinct usernames must not grow
// the map without bound.
func TestLimiterEvictsExpiredEntries(t *testing.T) {
	now := time.Now()
	l := NewLimiter(5, time.Minute).WithClock(func() time.Time { return now })

	for i := 0; i < sweepInterval; i++ {
		l.RecordFailure(fmt.Sprintf("user-%d", i))
	}
	before := len(l.entries)
	if before == 0 {
		t.Fatal("nothing was recorded")
	}

	// Move past the window, then touch the limiter enough to trigger a sweep.
	now = now.Add(2 * time.Minute)
	for i := 0; i < sweepInterval+1; i++ {
		l.Allowed("someone")
	}

	if after := len(l.entries); after >= before {
		t.Errorf("map holds %d entries after the sweep, was %d; expired entries were not evicted",
			after, before)
	}
}

// TestLimiterIsConcurrencySafe exists because this is shared mutable state on
// the request path. Run with -race.
func TestLimiterIsConcurrencySafe(t *testing.T) {
	l := NewLimiter(100, time.Minute)

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			key := fmt.Sprintf("user-%d", g%4)
			for i := 0; i < 100; i++ {
				l.Allowed(key)
				l.RecordFailure(key)
				if i%10 == 0 {
					l.Reset(key)
				}
			}
		}(g)
	}
	wg.Wait()
}

// TestLoginLimitsMatchTheSpec pins the §11 numbers, so a change is deliberate.
func TestLoginLimitsMatchTheSpec(t *testing.T) {
	if LoginAttemptsPerIP < LoginAttemptsPerUsername {
		t.Error("the per-IP limit should not be stricter than the per-username limit; " +
			"two people behind one NAT share an IP")
	}
	if LoginWindow < time.Minute {
		t.Errorf("LoginWindow = %s, too short to slow down guessing", LoginWindow)
	}
}
