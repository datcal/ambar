package server

import (
	"strings"
	"testing"
)

// Previous/next on the detail page (M16).
//
// The page used to be a dead end: §8 asked for keyboard navigation and there was no way out
// of an asset except the browser's back button. These assertions pin down the three things
// that make the links trustworthy — the order matches the grid's, the ends of the list stop
// rather than wrap, and the filters you arrived with travel with you.
func TestAssetPrevNextFollowsTheGridOrder(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/a.png": "a",
		"pack/b.png": "bb",
		"pack/c.png": "ccc",
	})

	first := ts.assetID(t, "pack/a.png")
	middle := ts.assetID(t, "pack/b.png")
	last := ts.assetID(t, "pack/c.png")

	// The middle asset has both neighbours, and they are the ones either side of it in
	// filename order — the order ListGroups uses.
	body := ts.body(t, ts.get(t, itoa("/assets/%d", middle)))
	if !strings.Contains(body, itoa(`href="/assets/%d"`, first)) {
		t.Errorf("previous does not point at a.png (id %d)", first)
	}
	if !strings.Contains(body, itoa(`href="/assets/%d"`, last)) {
		t.Errorf("next does not point at c.png (id %d)", last)
	}

	// The ends stop. A wrap-around would make "next" a loop with no way to tell you had
	// seen everything, which is the one thing this control is for.
	firstBody := ts.body(t, ts.get(t, itoa("/assets/%d", first)))
	if !strings.Contains(firstBody, `data-role="next-asset"`) {
		t.Error("the first asset has no next link")
	}
	if strings.Contains(firstBody, `data-role="prev-asset"`) {
		t.Error("the first asset offers a previous link; the list should stop, not wrap")
	}
	lastBody := ts.body(t, ts.get(t, itoa("/assets/%d", last)))
	if strings.Contains(lastBody, `data-role="next-asset"`) {
		t.Error("the last asset offers a next link; the list should stop, not wrap")
	}
}

func TestAssetPrevNextKeepsTheFilters(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"pack/aaa.png": "a",
		"pack/bbb.png": "bb",
		"pack/zzz.png": "zzz",
	})

	middle := ts.assetID(t, "pack/bbb.png")

	// Reached from a search: the neighbours must be the search's neighbours, and the links
	// must carry the query so the next page can do the same.
	body := ts.body(t, ts.get(t, itoa("/assets/%d?q=b", middle)))
	if !strings.Contains(body, "q=b") {
		t.Error("the browse links dropped the search")
	}
	if strings.Contains(body, itoa(`/assets/%d?q=b`, ts.assetID(t, "pack/zzz.png"))) {
		t.Error("a neighbour outside the search was offered")
	}
}
