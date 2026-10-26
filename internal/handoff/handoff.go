// Package handoff passes requests from the server to a user's open editor.
//
// The server cannot write into a game project: the project is on somebody's desktop, and
// importing is the editor's job. So an AI assistant asking Ambar, over MCP, to put an asset
// into the Unreal project leaves a request here. The Ambar plugin of that user's open editor
// long-polls for exactly this, takes the request, imports through its own path (the one the
// browser's buttons use: manifest, credits, folder mirroring) and reports back. The
// assistant's tool call waits for that report.
//
// Everything is kept in memory. A request is an interactive "now", so one that no editor
// took within ClaimWithin is dropped rather than replayed into an editor opened tomorrow. A
// restart dropping the lot gives the same answer.
package handoff

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// ClaimWithin is how long a request waits for an editor to take it.
	ClaimWithin = 2 * time.Minute
	// KeepFor is how long a request is remembered after it was taken, finished or not, so its
	// result can still be asked for.
	KeepFor = 15 * time.Minute
	// SeenWithin is how recently an editor must have polled to count as connected. A poll is
	// held for up to MaxWait and asked again straight away, so this covers a poll and a retry.
	SeenWithin = 75 * time.Second
	// MaxWait caps an editor's long poll.
	MaxWait = 30 * time.Second
)

// ErrUnknown is a result for a request this user's editors were not given.
var ErrUnknown = errors.New("no such request")

// Request is one thing for an editor to do: import these assets.
type Request struct {
	ID int64 `json:"id"`
	// Engine is the editor it is for: "unreal".
	Engine string `json:"engine"`
	// Project, when set, is the one open project that should take it; empty means any.
	Project string `json:"project,omitempty"`
	// Folder overrides where the editor would put the assets.
	Folder   string  `json:"folder,omitempty"`
	AssetIDs []int64 `json:"asset_ids"`
	// Via says who asked, for the editor's notification.
	Via string `json:"via,omitempty"`
}

// Item is what became of one asset.
type Item struct {
	AssetID int64  `json:"asset_id"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	// Path is where it landed: a package (/Game/…) or, for a plain file, the file in the project.
	Path   string `json:"path,omitempty"`
	Folder string `json:"folder,omitempty"`
}

// Result is an editor's report on a request.
type Result struct {
	Project string `json:"project,omitempty"`
	Items   []Item `json:"items"`
}

// State is where a request has got to.
type State int

const (
	// Waiting: no editor has taken it yet.
	Waiting State = iota
	// Taken: an editor is working on it.
	Taken
	// Done: the result is in.
	Done
	// Gone: unknown, expired, or not this user's.
	Gone
)

type pending struct {
	req     Request
	user    int64
	created time.Time
	takenAt time.Time
	takenBy string
	result  *Result
	done    chan struct{}
}

type editorKey struct {
	user            int64
	engine, project string
}

// Broker holds the requests and the editors waiting for them.
type Broker struct {
	mu      sync.Mutex
	next    int64
	reqs    map[int64]*pending
	wake    map[int64]map[chan struct{}]struct{} // per user: editors in a long poll
	seen    map[editorKey]time.Time
	now     func() time.Time
	maxWait time.Duration
}

// New makes an empty broker.
func New() *Broker {
	return &Broker{
		reqs:    map[int64]*pending{},
		wake:    map[int64]map[chan struct{}]struct{}{},
		seen:    map[editorKey]time.Time{},
		now:     time.Now,
		maxWait: MaxWait,
	}
}

// WithClock replaces the clock, for tests.
func (b *Broker) WithClock(now func() time.Time) *Broker {
	b.now = now
	return b
}

// Submit leaves a request for user's editors and returns its id.
func (b *Broker) Submit(user int64, req Request) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	b.next++
	req.ID = b.next
	req.Engine = strings.ToLower(req.Engine)
	b.reqs[req.ID] = &pending{req: req, user: user, created: b.now(), done: make(chan struct{})}
	for ch := range b.wake[user] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	return req.ID
}

// Await waits until the request is done or ctx ends, and reports where it got to.
func (b *Broker) Await(ctx context.Context, user, id int64) (State, *Result) {
	b.mu.Lock()
	p, ok := b.reqs[id]
	b.mu.Unlock()
	if !ok || p.user != user {
		return Gone, nil
	}
	select {
	case <-p.done:
	case <-ctx.Done():
	}
	return b.Status(user, id)
}

// Status reports where a request has got to without waiting.
func (b *Broker) Status(user, id int64) (State, *Result) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	p, ok := b.reqs[id]
	switch {
	case !ok || p.user != user:
		return Gone, nil
	case p.result != nil:
		return Done, p.result
	case !p.takenAt.IsZero():
		return Taken, nil
	default:
		return Waiting, nil
	}
}

// Take is an editor's long poll. It hands over the user's waiting requests for that engine
// and project, waiting up to wait for one to arrive; nothing is an answer too.
func (b *Broker) Take(ctx context.Context, user int64, engine, project string, wait time.Duration) []Request {
	engine = strings.ToLower(engine)
	wait = min(max(wait, 0), b.maxWait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()

	ch := make(chan struct{}, 1)
	b.mu.Lock()
	if b.wake[user] == nil {
		b.wake[user] = map[chan struct{}]struct{}{}
	}
	b.wake[user][ch] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.wake[user], ch)
		if len(b.wake[user]) == 0 {
			delete(b.wake, user)
		}
		b.mu.Unlock()
	}()

	for {
		if out := b.claim(user, engine, project); len(out) > 0 {
			return out
		}
		select {
		case <-ch:
		case <-deadline.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// claim marks the matching waiting requests as taken and returns them.
func (b *Broker) claim(user int64, engine, project string) []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	now := b.now()
	b.seen[editorKey{user, engine, project}] = now

	var out []Request
	for _, p := range b.reqs {
		if p.user != user || !p.takenAt.IsZero() || p.req.Engine != engine {
			continue
		}
		if p.req.Project != "" && !strings.EqualFold(p.req.Project, project) {
			continue
		}
		p.takenAt, p.takenBy = now, project
		out = append(out, p.req)
	}
	// Oldest first, so an editor works through them in the order they were asked for.
	slices.SortFunc(out, func(a, b Request) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Finish records an editor's report on a request it took.
func (b *Broker) Finish(user, id int64, res Result) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.reqs[id]
	if !ok || p.user != user || p.takenAt.IsZero() || p.result != nil {
		return ErrUnknown
	}
	if res.Project == "" {
		res.Project = p.takenBy
	}
	p.result = &res
	close(p.done)
	return nil
}

// Editors lists the projects of user's editors for engine that polled recently.
func (b *Broker) Editors(user int64, engine string) []string {
	engine = strings.ToLower(engine)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prune()
	var out []string
	for k := range b.seen {
		if k.user == user && k.engine == engine {
			out = append(out, k.project)
		}
	}
	slices.Sort(out)
	return out
}

// prune forgets requests nobody took in time, old ones, and editors gone quiet. Callers
// hold the lock.
func (b *Broker) prune() {
	now := b.now()
	for id, p := range b.reqs {
		switch {
		case p.takenAt.IsZero() && now.Sub(p.created) > ClaimWithin:
			delete(b.reqs, id)
		case !p.takenAt.IsZero() && now.Sub(p.takenAt) > KeepFor:
			delete(b.reqs, id)
		}
	}
	for k, at := range b.seen {
		if now.Sub(at) > SeenWithin {
			delete(b.seen, k)
		}
	}
}
