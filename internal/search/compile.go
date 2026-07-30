package search

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// TagResolver maps a tag token to the asset-tag ids a filter on it should match.
//
// The set is the tag itself plus every descendant (§7: searching a parent
// returns children), with aliases already resolved. found is false when no such
// tag exists, so the compiler can make the filter match nothing rather than
// erroring on a typo.
type TagResolver interface {
	ResolveTag(ctx context.Context, token string) (ids []int64, found bool, err error)
}

// Compiled is a boolean SQL expression over an asset table alias, ready to AND
// into a larger WHERE clause, plus its bound arguments. SQL is empty when the
// query constrains nothing.
type Compiled struct {
	SQL  string
	Args []any
}

// Compile turns a parsed query into SQL against the given asset table alias
// (typically "a"). Groups are ORed, terms within a group ANDed; a resolver
// supplies tag id sets. Free-text and tag-alias resolution is why this needs a
// resolver and a context.
func Compile(ctx context.Context, q Query, alias string, r TagResolver) (Compiled, error) {
	var (
		groupSQL []string
		args     []any
	)
	for _, g := range q.Groups {
		var termSQL []string
		for _, t := range g.Terms {
			sql, a, err := compileTerm(ctx, t, alias, r)
			if err != nil {
				return Compiled{}, err
			}
			if sql == "" {
				continue
			}
			termSQL = append(termSQL, sql)
			args = append(args, a...)
		}
		if len(termSQL) == 0 {
			continue
		}
		groupSQL = append(groupSQL, "("+strings.Join(termSQL, " AND ")+")")
	}
	if len(groupSQL) == 0 {
		return Compiled{}, nil
	}
	return Compiled{SQL: "(" + strings.Join(groupSQL, " OR ") + ")", Args: args}, nil
}

func compileTerm(ctx context.Context, t Term, alias string, r TagResolver) (string, []any, error) {
	switch term := t.(type) {
	case WordTerm:
		// A bare word is a tag when it is a known alias/tag, otherwise free text.
		if r != nil {
			ids, found, err := r.ResolveTag(ctx, term.Word)
			if err != nil {
				return "", nil, err
			}
			if found {
				return tagExpr(alias, ids, term.Neg), tagArgs(ids), nil
			}
		}
		return ftsExpr(alias, ftsPrefix(term.Word), term.Neg)
	case PhraseTerm:
		return ftsExpr(alias, ftsPhrase(term.Phrase), term.Neg)
	case TagTerm:
		if r == nil {
			return negate("0 = 1", term.Neg), nil, nil
		}
		ids, found, err := r.ResolveTag(ctx, term.Canonical)
		if err != nil {
			return "", nil, err
		}
		if !found {
			// A filter on a tag nobody has matches nothing (or everything, negated).
			return negate("0 = 1", term.Neg), nil, nil
		}
		return tagExpr(alias, ids, term.Neg), tagArgs(ids), nil
	case KindTerm:
		return negate(fmt.Sprintf("%s.kind = ?", alias), term.Neg), []any{term.Kind}, nil
	case HasTerm:
		return negate(hasExpr(alias, term.Flag), term.Neg), nil, nil
	case StyleTerm:
		// Only pixel-art reaches here; other style:* parse to a TagTerm.
		return negate(fmt.Sprintf("coalesce(%s.is_pixel_art, 0) = 1", alias), term.Neg), nil, nil
	case FieldTerm:
		col, ok := columnFor(term.Field)
		if !ok {
			return "", nil, nil // unknown/future field: a no-op, already warned at parse
		}
		val := any(term.Num)
		if term.IsDate {
			val = term.Date
		}
		return negate(fmt.Sprintf("%s.%s %s ?", alias, col, term.Op), term.Neg), []any{val}, nil
	case ColorTerm:
		return "", nil, nil // M11.5
	default:
		return "", nil, fmt.Errorf("search: unhandled term %T", t)
	}
}

func columnFor(field string) (string, bool) {
	if c, ok := numericFields[field]; ok {
		return c, true
	}
	if c, ok := dateFields[field]; ok {
		return c, true
	}
	return "", false
}

// tagExpr matches assets carrying the tag directly or inheriting it from their
// pack (§7 inherited tags).
func tagExpr(alias string, ids []int64, neg bool) string {
	ph := placeholders(len(ids))
	expr := fmt.Sprintf(
		"(%s.id IN (SELECT asset_id FROM asset_tags WHERE tag_id IN (%s)) "+
			"OR %s.pack_id IN (SELECT pack_id FROM pack_tags WHERE tag_id IN (%s)))",
		alias, ph, alias, ph)
	return negate(expr, neg)
}

// tagArgs repeats the id list twice: once for asset_tags, once for pack_tags.
func tagArgs(ids []int64) []any {
	args := make([]any, 0, len(ids)*2)
	for range 2 {
		for _, id := range ids {
			args = append(args, id)
		}
	}
	return args
}

func hasExpr(alias, flag string) string {
	switch flag {
	case "alpha":
		return fmt.Sprintf("coalesce(%s.has_alpha, 0) = 1", alias)
	case "animation":
		return fmt.Sprintf("coalesce(%s.frame_count, 0) > 1", alias)
	case "semitransparent":
		return fmt.Sprintf("coalesce(%s.has_semitransparent, 0) = 1", alias)
	}
	return "0 = 1"
}

// ftsExpr wraps an FTS5 MATCH as an asset-id subquery so it composes under AND,
// OR and NOT alongside the structured filters.
func ftsExpr(alias, match string, neg bool) (string, []any, error) {
	if match == "" {
		// A word that tokenises to nothing (all punctuation) constrains nothing.
		return "", nil, nil
	}
	op := "IN"
	if neg {
		op = "NOT IN"
	}
	return fmt.Sprintf("%s.id %s (SELECT rowid FROM assets_fts WHERE assets_fts MATCH ?)", alias, op),
		[]any{match}, nil
}

// ftsPrefix builds a prefix MATCH for a bare word, mirroring the M1 search box:
// the token is quoted as a literal (neutralising FTS operators) with a trailing
// * for as-you-type matching.
func ftsPrefix(word string) string {
	tok := ftsToken(word)
	if tok == "" {
		return ""
	}
	return `"` + tok + `"*`
}

// ftsPhrase builds an exact-phrase MATCH for a quoted phrase.
func ftsPhrase(phrase string) string {
	tok := ftsToken(phrase)
	if tok == "" {
		return ""
	}
	return `"` + tok + `"`
}

// ftsToken keeps only characters the tokenizer indexes, collapsing separators to
// spaces, so "wooden_sword.png" becomes the multi-word literal "wooden sword png"
// rather than a single unmatchable token.
func ftsToken(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		case r > unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func negate(expr string, neg bool) string {
	if neg {
		return "NOT (" + expr + ")"
	}
	return expr
}
