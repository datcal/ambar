package library

import (
	"strings"
	"unicode"
)

// NormalizeSegment turns one raw folder segment into a tag-safe token, or ""
// when the segment carries no meaning worth tagging (§7).
//
// Real vendor folders are messy, so the normalisation is deliberate:
//
//   - a format-folder segment (PNG, ASEPRITE, Tiled_files) is dropped, or the
//     library fills up with a `png` tag on everything;
//   - a leading ordering prefix is stripped, so `2 Objects` -> `objects` and
//     `4 Stone` -> `stone` rather than the useless `2-objects` / `4-stone`;
//   - the rest is lowercased, and every run of non-alphanumeric characters
//     (spaces, `&`, punctuation) collapses to a single `-`;
//   - a pure number (`128`, a size folder) is dropped, since it names nothing.
//
// `2d` and `3d` survive: the digit run is only an ordering prefix when a
// separator follows it, so `2 objects` strips but `2d` does not.
func NormalizeSegment(segment string) string {
	s := strings.TrimSpace(segment)
	if s == "" || IsFormatFolder(s) {
		return ""
	}
	s = stripOrderingPrefix(s)
	s = strings.ToLower(s)

	var b strings.Builder
	dashPending := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dashPending && b.Len() > 0 {
				b.WriteByte('-')
			}
			dashPending = false
			b.WriteRune(r)
		} else {
			dashPending = true
		}
	}

	token := b.String()
	if token == "" || isAllDigits(token) {
		return ""
	}
	return token
}

// stripOrderingPrefix removes a leading run of digits that is followed by a
// separator, which is how vendors number ordered folders (`2 Objects`,
// `01_intro`, `3.Weapons`). Digits not followed by a separator are kept, so a
// genuine `2d` or `3ds` segment is untouched. A prefix that would leave nothing
// behind is also kept, so a folder literally named `2` still normalises to "".
func stripOrderingPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == 0 {
		return s
	}
	j := i
	for j < len(s) {
		switch s[j] {
		case ' ', '_', '-', '.':
			j++
			continue
		}
		break
	}
	if j == i || strings.TrimSpace(s[j:]) == "" {
		return s
	}
	return s[j:]
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// PathTagSegments returns the meaningful, normalised folder segments of a
// slash-separated relative path, with the filename excluded and duplicates
// removed in first-seen order.
//
// This is the raw material for §7 auto-path tags: `Environment/Rocks/idle.png`
// yields `environment`, `rocks`.
func PathTagSegments(relPath string) []string {
	parts := strings.Split(relPath, "/")
	if len(parts) > 0 {
		parts = parts[:len(parts)-1] // drop the filename
	}
	var (
		out  []string
		seen = map[string]bool{}
	)
	for _, p := range parts {
		tok := NormalizeSegment(p)
		if tok == "" || seen[tok] {
			continue
		}
		seen[tok] = true
		out = append(out, tok)
	}
	return out
}
