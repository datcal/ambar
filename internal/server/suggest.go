package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/datcal/ambar/internal/index"
)

// handleSearchSuggest returns the search box's completion list as an HTML fragment.
//
// HTML rather than JSON because the island that renders it is twenty lines of DOM handling
// either way, and a fragment keeps the markup — grouping, escaping, the count column — in the
// template with everything else. §2's "no SPA" applies to search as much as to the grid.
//
// GET with no side effects, so it needs no CSRF token; it is behind auth like every other
// route, because the suggestions are the library's contents.
//
// The box has a scope as well as text: the folder and kind chips, which the island sends as
// dir and kind along with the view settings it would otherwise lose. Two kinds of row act on
// that scope instead of on the text. A folder row makes the folder the scope — type "spy",
// press Enter, and you are searching inside POLYGON - Spy Kit — and a kind row narrows to
// models, images, and so on, counted inside the scope.
func (s *Server) handleSearchSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("q"))
	current := url.Values{}
	for _, k := range []string{"dir", "kind", "sub", "view", "sort", "per"} {
		if v := q.Get(k); v != "" {
			current.Set(k, v)
		}
	}

	data := s.newPageData(r)

	// An empty box inside a folder offers the kinds there, which is the next thing anyone
	// scoping to a pack wants. Otherwise there is nothing to complete: the island shows
	// recent searches, which it holds itself — the server has no business remembering what
	// one browser typed.
	if query == "" {
		if current.Get("dir") != "" && current.Get("kind") == "" {
			data.Suggestions = s.kindSuggestions(r, current, "", "")
		}
		s.renderPartial(w, r, "assets.html", "search-suggestions", http.StatusOK, data)
		return
	}

	suggestions, err := s.index.Suggest(r.Context(), query)
	if err != nil {
		// A failed suggestion is not a failed search: log it and return an empty list so
		// the box keeps working.
		s.log.ErrorContext(r.Context(), "suggest failed", "error", err)
		suggestions = nil
	}

	// Keywords stay first — they answer "what can I type" — then the rows that move the
	// scope, then the vocabulary.
	var keywords, rest []index.Suggestion
	for _, sg := range suggestions {
		if sg.Kind == index.SuggestKeyword {
			keywords = append(keywords, sg)
		} else {
			rest = append(rest, sg)
		}
	}

	// What the folder rows match: the whole box when it is plain words ("spy kit"), only
	// the token being typed once there is syntax in it ("type:model spy"). Whatever it
	// matched is taken out of the search when the folder becomes the scope.
	folderText, remainder := query, ""
	if strings.Contains(query, ":") {
		token := lastWord(query)
		if strings.Contains(token, ":") {
			folderText = ""
		} else {
			folderText, remainder = token, strings.TrimSpace(strings.TrimSuffix(query, token))
		}
	}
	var folders []index.Suggestion
	seen := map[string]bool{}
	if folderText != "" {
		for _, hit := range s.findFolders(r.Context(), folderText, current) {
			if len(folders) >= index.SuggestLimit {
				break
			}
			seen[strings.ToLower(hit.Name)] = true
			detail := hit.Parent
			if detail == "" {
				detail = "library root"
			}
			folders = append(folders, index.Suggestion{
				Kind: index.SuggestFolder, Display: hit.Name,
				Detail: fmt.Sprintf("%s · %d", detail, hit.Count),
				Href:   libraryLink(current, "dir", hit.Path, "q", remainder),
			})
		}
	}

	// "mod" offers Models, counted where you are.
	var kinds []index.Suggestion
	if token := strings.ToLower(lastWord(query)); token != "" && !strings.Contains(token, ":") {
		kinds = s.kindSuggestions(r, current, token, strings.TrimSpace(strings.TrimSuffix(query, lastWord(query))))
	}

	out := append(keywords, folders...)
	out = append(out, kinds...)
	for _, sg := range rest {
		// A pack row is a text search for the pack's name; when the same name is already
		// offered as a folder, the folder is the better answer and the pack is noise.
		if sg.Kind == index.SuggestPack && seen[strings.ToLower(sg.Display)] {
			continue
		}
		out = append(out, sg)
	}
	data.Suggestions = out
	s.renderPartial(w, r, "assets.html", "search-suggestions", http.StatusOK, data)
}

// kindSuggestions offers the kinds within the current scope whose names start with prefix
// (all of them for ""), counted, each linking to the scope narrowed to it with remainder as
// the search text.
func (s *Server) kindSuggestions(r *http.Request, current url.Values, prefix, remainder string) []index.Suggestion {
	if current.Get("kind") != "" {
		return nil
	}
	// Cheap test first: no kind starts with this, so no query.
	var wanted []string
	for kind, label := range kindLabels {
		if prefix == "" || strings.HasPrefix(kind, prefix) || strings.HasPrefix(strings.ToLower(label), prefix) {
			wanted = append(wanted, kind)
		}
	}
	if len(wanted) == 0 || (prefix != "" && len(prefix) < 2) {
		return nil
	}

	// The folder's counts come from the cached tree, as on the page itself; this runs on a
	// keystroke.
	var tally index.KindTally
	if root := s.folderTree(r.Context()); root != nil {
		if node := root.Find(current.Get("dir")); node != nil {
			tally = node.Tally(current.Get("sub") == "0")
		}
	}
	where := "the library"
	if dir := current.Get("dir"); dir != "" {
		where = dir[strings.LastIndex(dir, "/")+1:]
	}
	var out []index.Suggestion
	for _, k := range tally.Kinds {
		if k.Count == 0 || !contains(wanted, k.Kind) {
			continue
		}
		out = append(out, index.Suggestion{
			Kind: index.SuggestNarrow, Display: kindLabel(k.Kind),
			Detail: fmt.Sprintf("%d in %s", k.Count, where),
			Href:   libraryLink(current, "kind", k.Kind, "q", remainder),
		})
	}
	return out
}

func lastWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
