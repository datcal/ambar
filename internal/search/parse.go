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
//	  - color:#8b3a3a[~t]    assets containing that colour, within a tolerance
//	  - palette-near:<id>[~t] assets whose palette is close to that asset's
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
	// column does not exist yet, a malformed colour — so the UI can tell the user
	// their filter was ignored rather than silently dropping it.
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

// DimensionsTerm is a pixel size: `32x32`, typed bare, or `dim:32x32`.
//
// Exact rather than a range. "32x32" in a sprite library is a category — the tile grid you
// are working in — not an approximation, and `width:>=32 width:<=48` already covers the
// range case.
type DimensionsTerm struct {
	base
	W, H int
}

// parseDimensions reads "32x32", "32X32" or "32×32". Zero is refused: a 0-pixel asset is not
// a thing anyone searches for, and accepting it would turn a stray "0x0" into a filter that
// silently matches nothing.
func parseDimensions(text string) (w, h int, ok bool) {
	lower := strings.ToLower(strings.ReplaceAll(text, "×", "x"))
	left, right, found := strings.Cut(lower, "x")
	if !found {
		return 0, 0, false
	}
	w, errW := strconv.Atoi(left)
	h, errH := strconv.Atoi(right)
	if errW != nil || errH != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// ColorTerm is `color:#8b3a3a` — assets containing that colour (§7).
//
// The match is a box in RGB rather than a perceptual distance: the query is "this
// exact colour, give or take", which is what a person picking a hex out of the
// palette panel means, and a box is what an index can answer. Tolerance is
// per-channel and comes from an optional `~N` suffix.
type ColorTerm struct {
	base
	R, G, B int
	// Tolerance is the permitted per-channel difference, 0–255.
	Tolerance int
	// Raw is the token as typed, for error messages and for round-tripping a query
	// back into the search box.
	Raw string
}

// DefaultColorTolerance is the per-channel slack when a query does not say.
//
// 12 of 255 is deliberately tight: pixel artists reuse exact palette entries, so
// `color:` is normally an exact-match question, and a wide box would return every
// brownish asset in the library. `color:#8b3a3a~40` is there for the other case.
const DefaultColorTolerance = 12

// PaletteNearTerm is `palette-near:<asset_id>` — assets whose palette is close to
// the given asset's (§7). Resolved by the compiler, which needs the database to read
// the reference asset's swatches.
type PaletteNearTerm struct {
	base
	AssetID int64
	// Tolerance is the per-channel slack when deciding whether two swatches are the
	// same colour. Looser than ColorTerm's by default: the question is "does this sit
	// next to that", not "does it contain exactly this".
	Tolerance int
	Raw       string
}

// DefaultPaletteNearTolerance is the per-channel slack for palette-near.
const DefaultPaletteNearTolerance = 24

// numericFields maps a query field to its asset column. Only columns that exist
// after M2 are here; the rest are futureFields.
var numericFields = map[string]string{
	"width":  "width",
	"height": "height",
	"size":   "size",
	"colors": "color_count",
	"frames": "frame_count",
	"fps":    "fps",

	// M16: these were in futureFields, parsing so a query would not error and then
	// contributing nothing — so `tris:<5000` quietly returned everything, which is worse
	// than an error. Their columns landed with M5 (audio) and M6 (models); the filters
	// only ever needed connecting.
	"tris":      "tri_count",
	"verts":     "vert_count",
	"materials": "material_count",
	"duration":  "duration_ms",
}

// dateFields maps a date query field to its column.
var dateFields = map[string]string{
	"added": "first_seen_at",
}

// futureFields are §7 fields whose columns do not exist yet. They parse so a query written
// today does not error, but they contribute nothing.
//
// `acquired` is the last one: provenance records an acquisition date on the *pack*, and
// filtering assets by it needs a join this compiler does not do yet.
var futureFields = map[string]bool{
	"acquired": true,
}

var knownKinds = map[string]bool{
	"image": true, "spritesheet": true, "texture": true, "model": true,
	"audio": true, "video": true, "font": true, "script": true,
	"material": true, "hdri": true, "tilemap": true, "rig": true, "other": true,
}

var knownHasFlags = map[string]bool{
	"alpha": true, "animation": true, "semitransparent": true, "semitransparency": true,
	// M16: has:provenance means the pack's licence and source are both recorded, so
	// -has:provenance is the capture backlog §9 asks for.
	"provenance": true,
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
		// A bare "32x32" is a size, not a word. Nobody types that hoping to match a
		// filename, and "which of these are 32 by 32" is the most common question in a
		// library of sprites — asking for width:32 height:32 to answer it is a syntax
		// tax on the thing people search for most.
		if w, h, ok := parseDimensions(text); ok {
			return DimensionsTerm{base{neg}, w, h}, ""
		}
		return WordTerm{base{neg}, strings.ToLower(text)}, ""
	}
	key = strings.ToLower(key)

	switch key {
	case "dim", "dims", "px":
		// The explicit form of the same thing, for a query built by a link rather than by
		// hand. `size:` cannot be it: that has meant file bytes since M1.
		if w, h, ok := parseDimensions(value); ok {
			return DimensionsTerm{base{neg}, w, h}, ""
		}
		return nil, fmt.Sprintf("%q is not a pixel size — try dim:32x32", text)

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
	case "color", "colour":
		return parseColor(neg, text, value)
	case "palette-near":
		return parsePaletteNear(neg, text, value)
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

// parseColor reads `color:#8b3a3a`, `color:8b3a3a`, the three-digit short form, and
// an optional `~N` tolerance suffix.
func parseColor(neg bool, text, value string) (Term, string) {
	body, tolerance, warn := splitTolerance(text, value, DefaultColorTolerance)
	if warn != "" {
		return nil, warn
	}
	r, g, b, ok := parseHexColour(body)
	if !ok {
		return nil, fmt.Sprintf("ignored %s (not a hex colour like #8b3a3a)", text)
	}
	return ColorTerm{
		base: base{neg}, R: r, G: g, B: b, Tolerance: tolerance, Raw: text,
	}, ""
}

// parsePaletteNear reads `palette-near:1234` with an optional `~N` tolerance.
func parsePaletteNear(neg bool, text, value string) (Term, string) {
	body, tolerance, warn := splitTolerance(text, value, DefaultPaletteNearTolerance)
	if warn != "" {
		return nil, warn
	}
	id, err := strconv.ParseInt(strings.TrimSpace(body), 10, 64)
	if err != nil || id <= 0 {
		return nil, fmt.Sprintf("ignored %s (palette-near takes an asset id)", text)
	}
	return PaletteNearTerm{base: base{neg}, AssetID: id, Tolerance: tolerance, Raw: text}, ""
}

// splitTolerance peels an optional `~N` suffix off a value, clamping it to a
// per-channel 0–255. Returns the remaining body and the tolerance to use.
func splitTolerance(text, value string, fallback int) (body string, tolerance int, warn string) {
	body, suffix, found := strings.Cut(value, "~")
	if !found {
		return body, fallback, ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(suffix))
	if err != nil {
		return body, 0, fmt.Sprintf("ignored %s (tolerance after ~ must be a number)", text)
	}
	switch {
	case n < 0:
		return body, 0, fmt.Sprintf("ignored %s (tolerance cannot be negative)", text)
	case n > 255:
		// Not an error: a tolerance of 255 already matches everything, so clamping is
		// the same answer the user asked for.
		n = 255
	}
	return body, n, ""
}

// parseHexColour accepts #rgb, #rrggbb and the same without the hash.
func parseHexColour(s string) (r, g, b int, ok bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	switch len(s) {
	case 3:
		// #abc means #aabbcc, as everywhere else on the web.
		v, err := strconv.ParseUint(s, 16, 32)
		if err != nil {
			return 0, 0, 0, false
		}
		r = int((v>>8)&0xf) * 0x11
		g = int((v>>4)&0xf) * 0x11
		b = int(v&0xf) * 0x11
		return r, g, b, true
	case 6:
		v, err := strconv.ParseUint(s, 16, 32)
		if err != nil {
			return 0, 0, 0, false
		}
		return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), true
	default:
		return 0, 0, 0, false
	}
}
