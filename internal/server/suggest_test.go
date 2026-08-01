package server

import (
	"strings"
	"testing"
)

// Search completion (M16). The box had none, so using the search meant remembering both the
// library's vocabulary and the query syntax.
func TestSearchSuggest(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{
		"kenney-sci-fi/PNG/turret.png": "t",
		"kenney-sci-fi/PNG/tank.png":   "k",
	})

	// A filename prefix finds the file, and says which group it is in.
	body := ts.body(t, ts.get(t, "/api/v1/suggest?q=tur"))
	if !strings.Contains(body, "turret.png") {
		t.Errorf("no filename suggestion for 'tur': %s", body)
	}
	if !strings.Contains(body, "Files") {
		t.Error("suggestions are not grouped")
	}

	// A pack name is offered with the number of assets behind it, because a pack of two and a
	// pack of four hundred are different offers.
	packs := ts.body(t, ts.get(t, "/api/v1/suggest?q=ken"))
	if !strings.Contains(packs, "kenney-sci-fi") || !strings.Contains(packs, "Packs") {
		t.Errorf("no pack suggestion for 'ken': %s", packs)
	}

	// The query language is offered too — this is the only place the syntax is documented in
	// the UI now that the placeholder no longer lists five examples.
	keywords := ts.body(t, ts.get(t, "/api/v1/suggest?q=ty"))
	if !strings.Contains(keywords, "type:") || !strings.Contains(keywords, "Filters") {
		t.Errorf("no keyword completion for 'ty': %s", keywords)
	}

	// Only the last token is completed: the rest is context the user already committed to.
	scoped := ts.body(t, ts.get(t, "/api/v1/suggest?q=type%3Amodel+tur"))
	if !strings.Contains(scoped, "turret.png") {
		t.Errorf("the last token was not completed: %s", scoped)
	}

	// An empty box has nothing to complete; recent searches live in the browser.
	if empty := ts.body(t, ts.get(t, "/api/v1/suggest?q=")); strings.Contains(empty, "suggest-item") {
		t.Errorf("an empty query produced suggestions: %s", empty)
	}
}

// A LIKE wildcard in the query must be a literal, or "%" would suggest the entire library.
func TestSearchSuggestEscapesWildcards(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedLibrary(t, map[string]string{"pack/hero.png": "h"})

	body := ts.body(t, ts.get(t, "/api/v1/suggest?q=%25"))
	if strings.Contains(body, "hero.png") {
		t.Errorf("'%%' matched everything; the LIKE pattern is not escaped: %s", body)
	}
}
