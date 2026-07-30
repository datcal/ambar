// Package search is the §7 query language: a real parser, not a naive LIKE.
//
// A query is one or more OR-separated groups; the terms within a group are
// implicitly ANDed. A term is optionally negated with a leading `-`. Terms take
// several shapes, resolved here into a typed AST that the compiler (compile.go)
// turns into SQL against the index:
//
//		type:model theme:sci-fi -style:realistic width:>=64 "laser turret" added:>2026-01
//
//	  - bare word            free-text / alias / fuzzy filename (resolved late)
//	  - "quoted phrase"      free-text phrase
//	  - namespace:name       tag filter, hierarchy- and alias-aware
//	  - kind: / type:        the asset kind column
//	  - has:alpha|animation  a boolean analysis column
//	  - style:pixel-art      the pixel-art column (other style:* are tags)
//	  - field:<op><value>    numeric/date comparison on an indexed column
//	  - color: / palette-near: parsed but a no-op until M11.5 (see decisions.md)
//
// Parsing never touches the database. Tag and alias resolution, which does,
// happens in the compiler.
package search

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Query is a parsed search expression: OR of groups, each an AND of terms.
type Query struct {
	Groups []Group
	// Warnings records syntactically valid but unactionable parts — a field whose
	// column does not exist yet, a colour filter before M11.5 — so the UI can tell
	// the user their filter was ignored rather than silently dropping it.
	Warnings []string
}

// Empty reports whether the query selects everything (no actionable terms).
func (q Query) Empty() bool {
	for _, g := range q.Groups {
		if len(g.Terms) > 0 {
			return false
		}
	}
	return true
}

// Group is a set of AND-ed terms.
type Group struct {
	Terms []Term
}

// Term is one parsed condition. It is a marker interface; the concrete types
// below are what the compiler switches on.
type Term interface{ negated() bool }

type base struct{ Neg bool }

func (b base) negated() bool { return b.Neg }

// WordTerm is an unquoted bare word: resolved by the compiler as a tag alias if
// one matches, otherwise as a free-text / fuzzy filename match.
type WordTerm struct {
	base
	Word string
}

// PhraseTerm is a quoted phrase, always free-text.
type PhraseTerm struct {
	base
	Phrase string
}

// TagTerm is a namespace:name tag filter, expanded to descendants and matched
// against direct and inherited tags by the compiler.
type TagTerm struct {
	base
	Canonical string
}

// KindTerm filters on the asset kind column (kind: or type:).
type KindTerm struct {
	base
	Kind string
}

// HasTerm filters on a boolean analysis column: alpha, animation, semitransparent.
type HasTerm struct {
	base
	Flag string
}

// StyleTerm is style:pixel-art, filtering the is_pixel_art column.
type StyleTerm struct {
	base
	Style string
}

// FieldTerm is a numeric or date comparison on an indexed column.
type FieldTerm struct {
	base
	Field  string // the query field name, e.g. "width", "added"
	Op     string // one of < <= > >= =
	Num    float64
	IsDate bool
	Date   int64 // unix seconds, when IsDate
}

// ColorTerm is color: or palette-near:, parsed but not matched until M11.5.
type ColorTerm struct {
	base
	Raw string
}

// numericFields maps a query field to its asset column. Only columns that exist
// after M2 are here; the rest are futureFields.
var numericFields = map[string]string{
	"width":  "width",
	"height": "height",
	"size":   "size",
	"colors": "color_count",
	"frames": "frame_count",
	"fps":    "fps",
}

// dateFields maps a date query field to its column.
var dateFields = map[string]string{
	"added": "first_seen_at",
}

// futureFields are §7 fields whose columns arrive in later milestones. They parse
// so a query written today does not error, but they contribute nothing until the
// milestone that adds the column (tris/verts → M6, duration → M5, acquired → M4).
var futureFields = map[string]bool{
	"tris": true, "verts": true, "duration": true, "acquired": true,
}

var knownKinds = map[string]bool{
	"image": true, "spritesheet": true, "texture": true, "model": true,
	"audio": true, "video": true, "font": true, "script": true,
	"material": true, "hdri": true, "tilemap": true, "rig": true, "other": true,
}

var knownHasFlags = map[string]bool{
	"alpha": true, "animation": true, "semitransparent": true, "semitransparency": true,
}

// Parse turns a raw query string into a Query. It never returns an error for
// ordinary user input — malformed fragments become free-text or warnings — so
// the search box cannot 500. It returns an error only for input that cannot be a
// query at all, which in practice does not occur.
func Parse(input string) (Query, error) {
	var q Query
	groups := splitGroups(lex(input))
	for _, toks := range groups {
		var g Group
		for _, tk := range toks {
			term, warn := classify(tk, &q)
			if warn != "" {
				q.Warnings = append(q.Warnings, warn)
			}
			if term != nil {
				g.Terms = append(g.Terms, term)
			}
		}
		if len(g.Terms) > 0 {
			q.Groups = append(q.Groups, g)
		}
	}
	return q, nil
}

// token is one lexed unit and whether it came from inside quotes.
type token struct {
	text   string
	quoted bool
}

// lex splits input into tokens, keeping quoted spans together. An unbalanced
// quote runs to the end of the string rather than erroring.
func lex(input string) []token {
	var (
		out     []token
		b       strings.Builder
		inQuote bool
	)
	flush := func(quoted bool) {
		if b.Len() > 0 || quoted {
			out = append(out, token{text: b.String(), quoted: quoted})
			b.Reset()
		}
	}
	for _, r := range input {
		switch {
		case r == '"':
			if inQuote {
				flush(true)
				inQuote = false
			} else {
				flush(false)
				inQuote = true
			}
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if inQuote {
				b.WriteRune(r)
			} else {
				flush(false)
			}
		default:
			b.WriteRune(r)
		}
	}
	flush(inQuote) // an unbalanced quote still yields its phrase
	return out
}

// splitGroups partitions tokens on the OR operator (`OR`, `or`, or `|`) into
// AND-groups. OR only acts as an operator when it is a bare, unquoted token.
func splitGroups(tokens []token) [][]token {
	groups := [][]token{{}}
	for _, tk := range tokens {
		if !tk.quoted && isOr(tk.text) {
			groups = append(groups, []token{})
			continue
		}
		groups[len(groups)-1] = append(groups[len(groups)-1], tk)
	}
	return groups
}

func isOr(s string) bool {
	return s == "|" || strings.EqualFold(s, "or")
}

// classify turns one token into a term, or returns a warning for a recognised
// but unactionable filter. A nil term with no warning means the token was empty.
func classify(tk token, q *Query) (Term, string) {
	if tk.quoted {
		if tk.text == "" {
			return nil, ""
		}
		return PhraseTerm{Phrase: tk.text}, ""
	}

	text := tk.text
	neg := false
	if strings.HasPrefix(text, "-") && len(text) > 1 {
		neg = true
		text = text[1:]
	}
	if text == "" {
		return nil, ""
	}

	key, value, hasColon := strings.Cut(text, ":")
	if !hasColon {
		return WordTerm{base{neg}, strings.ToLower(text)}, ""
	}
	key = strings.ToLower(key)

	switch key {
	case "kind", "type":
		v := strings.ToLower(value)
		if v == "" {
			return WordTerm{base{neg}, strings.ToLower(text)}, ""
		}
		if key == "type" && !knownKinds[v] {
			// type: with a non-kind value is a real namespaced tag (type:sfx:impact).
			return TagTerm{base{neg}, strings.ToLower(text)}, ""
		}
		return KindTerm{base{neg}, v}, ""
	case "has":
		v := strings.ToLower(value)
		if !knownHasFlags[v] {
			return nil, fmt.Sprintf("ignored has:%s (unknown flag)", value)
		}
		if v == "semitransparency" {
			v = "semitransparent"
		}
		return HasTerm{base{neg}, v}, ""
	case "style":
		if strings.EqualFold(value, "pixel-art") {
			return StyleTerm{base{neg}, "pixel-art"}, ""
		}
		return TagTerm{base{neg}, strings.ToLower(text)}, ""
	case "color", "palette-near":
		return nil, fmt.Sprintf("ignored %s (colour search arrives in a later milestone)", key)
	}

	if _, ok := numericFields[key]; ok {
		return parseNumeric(neg, key, value)
	}
	if _, ok := dateFields[key]; ok {
		return parseDateField(neg, key, value)
	}
	if futureFields[key] {
		return nil, fmt.Sprintf("ignored %s (not available yet)", key)
	}

	// Anything else namespaced is a tag filter: theme:sci-fi, author:kenney,
	// biome:desert:dunes. Value may itself contain ':' for hierarchy.
	return TagTerm{base{neg}, strings.ToLower(text)}, ""
}

// splitOp separates a leading comparator from its operand. No operator means
// equality.
func splitOp(value string) (op, operand string) {
	for _, o := range []string{"<=", ">=", "<", ">", "="} {
		if strings.HasPrefix(value, o) {
			return o, strings.TrimPrefix(value, o)
		}
	}
	return "=", value
}

func parseNumeric(neg bool, field, value string) (Term, string) {
	op, operand := splitOp(value)
	n, err := strconv.ParseFloat(operand, 64)
	if err != nil {
		return nil, fmt.Sprintf("ignored %s:%s (not a number)", field, value)
	}
	return FieldTerm{base: base{neg}, Field: field, Op: op, Num: n}, ""
}

func parseDateField(neg bool, field, value string) (Term, string) {
	op, operand := splitOp(value)
	unix, err := parseDate(operand)
	if err != nil {
		return nil, fmt.Sprintf("ignored %s:%s (not a date)", field, value)
	}
	return FieldTerm{base: base{neg}, Field: field, Op: op, IsDate: true, Date: unix}, ""
}

// parseDate accepts YYYY, YYYY-MM or YYYY-MM-DD and returns the start of that
// period in UTC. A comparator then reads naturally: added:>2026-01 means "after
// the start of January 2026".
func parseDate(s string) (int64, error) {
	for _, layout := range []string{"2006-01-02", "2006-01", "2006"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("unrecognised date %q", s)
}
