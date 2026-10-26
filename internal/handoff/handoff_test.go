package handoff

import (
	"context"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// TestAnEditorTakesWhatItsUserAskedFor is the round trip: a request waits, the user's
// editor takes it, reports, and whoever waits on it gets the report.
func TestAnEditorTakesWhatItsUserAskedFor(t *testing.T) {
	b := New()
	ctx := context.Background()

	id := b.Submit(1, Request{Engine: "Unreal", AssetIDs: []int64{7, 8}})
	if got := b.Take(ctx, 2, "unreal", "Game", 0); len(got) != 0 {
		t.Fatalf("another user's editor took %v", got)
	}
	got := b.Take(ctx, 1, "unreal", "Game", 0)
	if len(got) != 1 || got[0].ID != id || len(got[0].AssetIDs) != 2 {
		t.Fatalf("the user's editor took %v, want request %d", got, id)
	}
	if again := b.Take(ctx, 1, "unreal", "Game", 0); len(again) != 0 {
		t.Errorf("a taken request was handed out again: %v", again)
	}
	if state, _ := b.Status(1, id); state != Taken {
		t.Errorf("state after the take = %v, want Taken", state)
	}

	if err := b.Finish(2, id, Result{}); err != ErrUnknown {
		t.Errorf("another user finished the request: %v", err)
	}
	if err := b.Finish(1, id, Result{Items: []Item{{AssetID: 7, OK: true, Path: "/Game/Ambar/x"}}}); err != nil {
		t.Fatal(err)
	}
	state, res := b.Await(ctx, 1, id)
	if state != Done || res == nil || res.Project != "Game" || res.Items[0].Path != "/Game/Ambar/x" {
		t.Errorf("await = %v %+v, want Done with the editor's report from Game", state, res)
	}
	if state, _ := b.Await(ctx, 2, id); state != Gone {
		t.Errorf("another user could read the result: %v", state)
	}
}

// TestALongPollWakesForANewRequest: an editor waiting in a poll gets a request submitted
// while it waits, without waiting out the poll.
func TestALongPollWakesForANewRequest(t *testing.T) {
	b := New()
	got := make(chan []Request, 1)
	go func() { got <- b.Take(context.Background(), 1, "unreal", "Game", 10*time.Second) }()

	// Until the editor is seen, a submit could land before the poll starts; either way it
	// must come back well inside the poll.
	time.Sleep(20 * time.Millisecond)
	b.Submit(1, Request{Engine: "unreal", AssetIDs: []int64{1}})
	select {
	case reqs := <-got:
		if len(reqs) != 1 {
			t.Errorf("the poll returned %v", reqs)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll did not wake for the new request")
	}
}

// TestAProjectNamedRequestWaitsForThatProject: with two editors open, a request naming one
// is not taken by the other; a request naming none goes to whichever asks first.
func TestAProjectNamedRequestWaitsForThatProject(t *testing.T) {
	b := New()
	ctx := context.Background()
	b.Submit(1, Request{Engine: "unreal", Project: "OceanRaiders", AssetIDs: []int64{1}})
	if got := b.Take(ctx, 1, "unreal", "OtherGame", 0); len(got) != 0 {
		t.Errorf("OtherGame took a request for OceanRaiders: %v", got)
	}
	if got := b.Take(ctx, 1, "unreal", "oceanraiders", 0); len(got) != 1 {
		t.Errorf("OceanRaiders did not get its request (project names are not case-sensitive)")
	}
	if got := b.Take(ctx, 1, "godot", "OceanRaiders", 0); len(got) != 0 {
		t.Errorf("a Godot editor took an Unreal request: %v", got)
	}
	if editors := b.Editors(1, "unreal"); len(editors) != 2 {
		t.Errorf("connected Unreal editors = %v, want both", editors)
	}
}

// TestNobodyTakesAnOldRequest: a request no editor took within ClaimWithin is gone, so an
// editor opened later does not import something asked for long ago; editors that stopped
// polling stop counting as connected.
func TestNobodyTakesAnOldRequest(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	b := New().WithClock(c.now)
	ctx := context.Background()

	b.Take(ctx, 1, "unreal", "Game", 0)
	id := b.Submit(1, Request{Engine: "unreal", AssetIDs: []int64{1}, Project: "Elsewhere"})
	c.t = c.t.Add(ClaimWithin + time.Second)
	if state, _ := b.Status(1, id); state != Gone {
		t.Errorf("an unclaimed request after %v is %v, want Gone", ClaimWithin, state)
	}
	if got := b.Take(ctx, 1, "unreal", "Elsewhere", 0); len(got) != 0 {
		t.Errorf("an expired request was handed out: %v", got)
	}
	c.t = c.t.Add(SeenWithin + time.Second)
	if editors := b.Editors(1, "unreal"); len(editors) != 0 {
		t.Errorf("editors that stopped polling still count: %v", editors)
	}
}

// TestAwaitEndsWithItsContext: a caller that stops waiting learns the request was taken
// but is not finished, and can ask again later.
func TestAwaitEndsWithItsContext(t *testing.T) {
	b := New()
	id := b.Submit(1, Request{Engine: "unreal", AssetIDs: []int64{1}})
	b.Take(context.Background(), 1, "unreal", "Game", 0)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if state, res := b.Await(ctx, 1, id); state != Taken || res != nil {
		t.Errorf("await after the context ended = %v %v, want Taken", state, res)
	}
	if err := b.Finish(1, id, Result{Items: []Item{{AssetID: 1, OK: true}}}); err != nil {
		t.Fatal(err)
	}
	if state, _ := b.Status(1, id); state != Done {
		t.Errorf("status after the late report = %v, want Done", state)
	}
	if err := b.Finish(1, id, Result{}); err != ErrUnknown {
		t.Errorf("a second report was accepted: %v", err)
	}
}
