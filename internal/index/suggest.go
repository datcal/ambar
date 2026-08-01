package index

import (
	"context"
	"fmt"
	"strings"
)

// Search suggestions (M16).
//
// The toolbar's search box had no completion at all. The only suggestions in the
// application came from `/api/v1/tags/suggest` into a `<datalist>` on the *tag* inputs, so
// using the search meant remembering both the vocabulary of a 6,000-asset library and the
// query syntax that filters it. Everything here exists to answer "what can I even type".
//
// The kinds are separate because they answer different questions: a keyword tells you the
// syntax exists, a tag or a pack tells you the vocabulary of *this* library, and a filename
// gets you to one specific thing. Mixing them into one ranked list would bury the rare
// exact-filename match under forty tags.

// SuggestKind labels a group of suggestions.
type SuggestKind string

const (
	SuggestKeyword  SuggestKind = "keyword"
	SuggestTag      SuggestKind = "tag"
	SuggestPack     SuggestKind = "pack"
	SuggestFolder   SuggestKind = "folder"
	SuggestFilename SuggestKind = "file"
)

// Label is the group heading the UI shows.
func (k SuggestKind) Label() string {
	switch k {
	case SuggestKeyword:
		return "Filters"
	case SuggestTag:
		return "Tags"
	case SuggestPack:
		return "Packs"
	case SuggestFolder:
		return "Folders"
	default:
		return "Files"
	}
}

// Suggestion is one row in the dropdown.
type Suggestion struct {
	Kind SuggestKind
	// Insert is what replaces the current token in the search box.
	Insert string
	// Display is what the row shows; equal to Insert unless a hint reads better.
	Display string
	// Detail is the muted right-hand side: a count, or an explanation of a keyword.
	Detail string
}

// suggestKeywords is the query language, as completions.
//
// This doubles as the only place the syntax is documented in the UI now that the
// five-example placeholder and the no-results paragraph are gone: you type "t", you see
// what "type:" does. Ordered by how often they are actually useful, not alphabetically.
var suggestKeywords = []Suggestion{
	{Kind: SuggestKeyword, Insert: "type:", Display: "type:", Detail: "image, model, audio, font…"},
	{Kind: SuggestKeyword, Insert: "style:pixel-art", Display: "style:pixel-art", Detail: "pixel art only"},
	{Kind: SuggestKeyword, Insert: "has:alpha", Display: "has:alpha", Detail: "transparency"},
	{Kind: SuggestKeyword, Insert: "has:animation", Display: "has:animation", Detail: "more than one frame"},
	{Kind: SuggestKeyword, Insert: "has:source-file", Display: "has:source-file", Detail: "an editable .psd/.aseprite exists"},
	{Kind: SuggestKeyword, Insert: "dim:32x32", Display: "32x32", Detail: "exact pixel size — or just type it"},
	{Kind: SuggestKeyword, Insert: "width:>=64", Display: "width:>=64", Detail: "compare a number"},
	{Kind: SuggestKeyword, Insert: "colors:<=16", Display: "colors:<=16", Detail: "palette size"},
	{Kind: SuggestKeyword, Insert: "frames:>1", Display: "frames:>1", Detail: "animation length"},
	{Kind: SuggestKeyword, Insert: "tris:<5000", Display: "tris:<5000", Detail: "triangle budget (3D)"},
	{Kind: SuggestKeyword, Insert: "materials:>1", Display: "materials:>1", Detail: "material count (3D)"},
	{Kind: SuggestKeyword, Insert: "duration:<2000", Display: "duration:<2000", Detail: "milliseconds (audio)"},
	{Kind: SuggestKeyword, Insert: "color:#8b3a3a", Display: "color:#8b3a3a", Detail: "contains this colour"},
	{Kind: SuggestKeyword, Insert: "added:>2026-01", Display: "added:>2026-01", Detail: "indexed after a date"},
	{Kind: SuggestKeyword, Insert: "license:cc0", Display: "license:cc0", Detail: "a tag, if you tag licences"},
}

// SuggestLimit caps each group. Deliberately small: a dropdown you scroll is a list you
// read, and reading is slower than typing another letter.
const SuggestLimit = 6

// Suggest completes the last token of a query.
//
// Only the last token, because the rest is already-committed context: someone typing
// `type:model tur` wants turret, not a re-run of what they have already narrowed to. The
// caller replaces that token with Insert and leaves the prefix alone.
//
// Every branch is a prefix match on an indexed column. That matters on the target hardware:
// this runs on a keystroke, and §8's grid is already the expensive page.
func (ix *Indexer) Suggest(ctx context.Context, query string) ([]Suggestion, error) {
	token := lastToken(query)
	if token == "" {
		return nil, nil
	}
	lower := strings.ToLower(token)

	var out []Suggestion

	// Keywords first: they are the answer to "what can I type", and they cost nothing.
	for _, kw := range suggestKeywords {
		if len(out) >= SuggestLimit {
			break
		}
		if strings.HasPrefix(strings.ToLower(kw.Insert), lower) ||
			strings.HasPrefix(strings.ToLower(kw.Display), lower) {
			out = append(out, kw)
		}
	}

	// A token that already names a field is a syntax question, not a vocabulary one — no
	// point offering filenames that happen to contain "type:".
	if strings.Contains(token, ":") {
		return out, nil
	}

	like := escapeLike(lower) + "%"

	tags, err := ix.suggestTags(ctx, like)
	if err != nil {
		return nil, err
	}
	out = append(out, tags...)

	packs, err := ix.suggestPacks(ctx, like)
	if err != nil {
		return nil, err
	}
	out = append(out, packs...)

	files, err := ix.suggestFilenames(ctx, like)
	if err != nil {
		return nil, err
	}
	out = append(out, files...)

	return out, nil
}

// lastToken is the word being typed: everything after the final space, unless a quote is
// open, in which case there is nothing useful to complete.
func lastToken(query string) string {
	if strings.Count(query, `"`)%2 == 1 {
		return ""
	}
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return ""
	}
	last := fields[len(fields)-1]
	// A trailing space means the token is finished and a new one has not started.
	if strings.HasSuffix(query, " ") {
		return ""
	}
	return last
}

// escapeLike neutralises the LIKE wildcards so a search for "50%" does not match everything.
// The queries below pair this with `ESCAPE '\'`.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "%", `\%`)
	s = strings.ReplaceAll(s, "_", `\_`)
	return s
}

func (ix *Indexer) suggestTags(ctx context.Context, like string) ([]Suggestion, error) {
	// Counted, because a tag with three assets behind it and one with four hundred are
	// different offers. The count is over asset_tags, which is indexed by tag.
	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT t.namespace || ':' || t.name AS canonical, count(at.asset_id)
		FROM tags t
		JOIN asset_tags at ON at.tag_id = t.id
		WHERE lower(t.namespace || ':' || t.name) LIKE ? ESCAPE '\'
		   OR lower(t.name) LIKE ? ESCAPE '\'
		GROUP BY t.id
		ORDER BY count(at.asset_id) DESC, canonical
		LIMIT ?`, like, like, SuggestLimit)
	if err != nil {
		return nil, fmt.Errorf("suggest tags: %w", err)
	}
	defer rows.Close()

	var out []Suggestion
	for rows.Next() {
		var canonical string
		var n int
		if err := rows.Scan(&canonical, &n); err != nil {
			return nil, err
		}
		out = append(out, Suggestion{
			Kind: SuggestTag, Insert: canonical, Display: canonical,
			Detail: fmt.Sprintf("%d", n),
		})
	}
	return out, rows.Err()
}

func (ix *Indexer) suggestPacks(ctx context.Context, like string) ([]Suggestion, error) {
	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT p.name, count(a.id)
		FROM packs p
		LEFT JOIN assets a ON a.pack_id = p.id AND a.missing_since IS NULL
		WHERE lower(p.name) LIKE ? ESCAPE '\'
		GROUP BY p.id
		ORDER BY count(a.id) DESC, p.name
		LIMIT ?`, like, SuggestLimit)
	if err != nil {
		return nil, fmt.Errorf("suggest packs: %w", err)
	}
	defer rows.Close()

	var out []Suggestion
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out = append(out, Suggestion{
			Kind: SuggestPack, Insert: name, Display: name,
			Detail: fmt.Sprintf("%d", n),
		})
	}
	return out, rows.Err()
}

func (ix *Indexer) suggestFilenames(ctx context.Context, like string) ([]Suggestion, error) {
	// DISTINCT because a pack of 200 frames named idle_00.png…idle_99.png would otherwise
	// fill the whole list with near-identical rows.
	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT DISTINCT a.filename
		FROM assets a
		WHERE a.missing_since IS NULL AND lower(a.filename) LIKE ? ESCAPE '\'
		ORDER BY length(a.filename), a.filename
		LIMIT ?`, like, SuggestLimit)
	if err != nil {
		return nil, fmt.Errorf("suggest filenames: %w", err)
	}
	defer rows.Close()

	var out []Suggestion
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, Suggestion{Kind: SuggestFilename, Insert: name, Display: name})
	}
	return out, rows.Err()
}
