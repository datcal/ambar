package auth

import (
	"sync"
	"time"
)

// Login rate limits (§11). Keyed separately by client IP and by username, and
// both must pass: the per-IP limit stops one host spraying many accounts, the
// per-username limit stops a distributed attempt on one account.
const (
	LoginAttemptsPerIP       = 10
	LoginAttemptsPerUsername = 5
	LoginWindow              = 15 * time.Minute
)

// Limiter is a fixed-window counter.
//
// Fixed windows let roughly twice the nominal rate through at a window
// boundary. For slowing down password guessing that is irrelevant, and it is a
// fraction of the code of a sliding window or a token bucket.
//
// In-memory by design: the counters are cheap to rebuild and a restart clearing
// them is not a weakness worth a database write on every failed login.
type Limiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	entries map[string]*limiterEntry
	now     func() time.Time

	// calls counts operations since the last sweep, so the map cannot grow
	// without bound under a spray of distinct usernames.
	calls int
}

type limiterEntry struct {
	count   int
	resetAt time.Time
}

// sweepInterval is how many operations pass between full map sweeps.
const sweepInterval = 512

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{
		max:     max,
		window:  window,
		entries: make(map[string]*limiterEntry),
		now:     time.Now,
	}
}

// WithClock replaces the clock, for tests.
func (l *Limiter) WithClock(now func() time.Time) *Limiter {
	l.now = now
	return l
}

// Allowed reports whether another attempt may be made for key, and if not, how
// long until it may be.
//
// It does not consume the attempt. Only a *failed* login records against the
// limit, so someone typing their own password correctly is never locked out by
// their earlier typos.
func (l *Limiter) Allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.maybeSweep(now)

	e, ok := l.entries[key]
	if !ok || !now.Before(e.resetAt) {
		return true, 0
	}
	if e.count < l.max {
		return true, 0
	}
	return false, e.resetAt.Sub(now)
}

// RecordFailure counts one failed attempt against key.
func (l *Limiter) RecordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.maybeSweep(now)

	e, ok := l.entries[key]
	if !ok || !now.Before(e.resetAt) {
		l.entries[key] = &limiterEntry{count: 1, resetAt: now.Add(l.window)}
		return
	}
	e.count++
}

// Reset clears the counter for key, called on a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// maybeSweep drops expired entries. Caller holds the lock.
func (l *Limiter) maybeSweep(now time.Time) {
	l.calls++
	if l.calls < sweepInterval {
		return
	}
	l.calls = 0
	for k, e := range l.entries {
		if !now.Before(e.resetAt) {
			delete(l.entries, k)
		}
	}
}
