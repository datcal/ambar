package db

import "testing"

// The palette-search backfill (M11.5 follow-up) wants json_each to turn the stored
// palette_json into indexed swatch rows in a migration. modernc.org/sqlite is a
// translation of the C source, so JSON1 should be compiled in — but "should" is not
// a thing to build a migration on.
func TestJSON1IsAvailable(t *testing.T) {
	d := openTestDB(t)

	var rows, maxR int
	err := d.Reader.QueryRow(`
		SELECT count(*), coalesce(max(json_extract(value, '$.r')), 0)
		FROM json_each('[{"r":10,"g":2,"b":3},{"r":200,"g":2,"b":3}]')`).Scan(&rows, &maxR)
	if err != nil {
		t.Fatalf("json_each is not available: %v", err)
	}
	if rows != 2 || maxR != 200 {
		t.Errorf("rows=%d maxR=%d, want 2 and 200", rows, maxR)
	}
}
