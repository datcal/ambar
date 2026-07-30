package tags

import (
	"context"
	"fmt"
	"strings"
)

// Suggest returns canonical tag strings whose canonical form or one of whose
// aliases starts with prefix, for the §7 autocomplete. Matching is
// case-insensitive and bounded; an empty prefix lists the first tags
// alphabetically, which is a reasonable "what tags exist" affordance.
func (s *Store) Suggest(ctx context.Context, prefix string, limit int) ([]string, error) {
	p := likeEscape(strings.ToLower(strings.TrimSpace(prefix))) + "%"
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.Reader.QueryContext(ctx, `
		SELECT canon FROM (
			SELECT namespace || ':' || name AS canon FROM tags
			WHERE (namespace || ':' || name) LIKE ? ESCAPE '\'
			UNION
			SELECT t.namespace || ':' || t.name FROM tag_aliases al
			JOIN tags t ON t.id = al.tag_id
			WHERE al.alias LIKE ? ESCAPE '\'
		)
		ORDER BY canon
		LIMIT ?`, p, p, limit)
	if err != nil {
		return nil, fmt.Errorf("suggest tags: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var canon string
		if err := rows.Scan(&canon); err != nil {
			return nil, err
		}
		out = append(out, canon)
	}
	return out, rows.Err()
}

// likeEscape neutralises the LIKE wildcards so a user typing `%` or `_` searches
// for those characters literally. Backslash is the escape character declared in
// the queries above.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
