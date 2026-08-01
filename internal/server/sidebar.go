package server

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/savedsearch"
)

// The shared library sidebar's data, and the cache in front of it.
//
// Two things forced this to exist together with M16's shared navigation. The sidebar now
// renders on the asset page as well as on the grid, and it is built from whole-library
// aggregates: counts by kind, the dominant colours, the folder tree, the pack states. On
// the target hardware — a NAS with a weak CPU and pure-Go SQLite — those add up to
// hundreds of milliseconds of *CPU* per page view, and `LibraryColours` is the worst of
// them because it groups asset_swatches on computed expressions, so no index applies.
// Rendering that on every asset you open would have made the navigation improvement a
// performance regression.
//
// The numbers only change when a scan, an ingest or a tag write lands, so a short TTL is
// the honest trade: at worst the sidebar is a minute stale, which for "how many models are
// in the library" is not a lie worth CPU. Writes that visibly change a count call
// invalidate() so the common case — you pressed a button, the number moved — is exact.

// sidebarTTL is how long a snapshot is served before it is rebuilt.
const sidebarTTL = 60 * time.Second

// lastScanInfo is the sidebar's "Scanned 10 minutes ago" line. §12 wanted scan runnable
// from the UI; not being able to see when it last ran was the other half of that.
type lastScanInfo struct {
	At     time.Time
	Assets int
}

// Ago renders the age the way the sidebar shows it: short, and never "0 seconds".
func (l lastScanInfo) Ago() string {
	d := time.Since(l.At)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

// sidebarData is everything the shared sidebar needs that does not depend on the current
// request. The folder tree is kept unflattened on purpose: flattening marks the open and
// selected nodes, which is per-request, and doing it in memory costs nothing.
type sidebarData struct {
	Stats           index.Stats
	Colours         []index.PackColour
	Tree            *index.TreeNode
	Saved           []savedsearch.SavedSearch
	NeedsProvenance int
	ActiveJobs      int
	LastScan        *lastScanInfo

	// Facets for the *unfiltered* library. Facets normally describe the current result set,
	// so they cannot be shared — but the unfiltered set is the same for everyone and is what
	// most page views ask for (browsing, and every page of it). Measured at 55 ms, which made
	// it the most expensive thing left on the request path once the grid query was fixed.
	Facets []index.Facet
}

// sidebarCache serves one snapshot to every request inside the TTL.
type sidebarCache struct {
	mu sync.Mutex
	at time.Time
	// building is the single-flight flag for a background refresh.
	building bool
	snap     *sidebarData
}

// invalidate drops the snapshot so the next request rebuilds it. Called from the handlers
// that change a count the sidebar shows.
func (c *sidebarCache) invalidate() {
	c.mu.Lock()
	c.snap = nil
	c.mu.Unlock()
}

// get returns the cached snapshot, refreshing it in the background when it goes stale.
//
// Stale-while-revalidate, for a measured reason. The aggregates cost about 250 ms of CPU on a
// 6,500-asset library on a developer machine — `LibraryColours` alone is 70 ms of SQL over
// 122,711 swatch rows, grouped on computed expressions so no index applies — and the NAS this
// runs on is several times slower. Making a request *wait* for that rebuild meant the first
// click after every write was the slow one, which is precisely the click a person is watching.
//
// So: a warm snapshot is served immediately, however stale, and one goroutine refreshes it. The
// numbers here are counts in a sidebar; a minute of staleness is not a lie worth a second of
// someone's attention. Only the very first request of a process has to wait, because there is
// nothing to serve yet.
//
// `building` makes the refresh single-flight: a thundering herd of page loads must not each
// start their own rebuild, since these queries are CPU-bound and ten in parallel make all ten
// slower.
func (c *sidebarCache) get(ctx context.Context, build func(context.Context) (*sidebarData, error)) (*sidebarData, error) {
	c.mu.Lock()
	warm := c.snap
	stale := warm == nil || time.Since(c.at) >= sidebarTTL
	shouldBuild := stale && !c.building
	if shouldBuild {
		c.building = true
	}
	c.mu.Unlock()

	if shouldBuild && warm != nil {
		// Refresh behind the request. The context is deliberately *not* the request's: the
		// rebuild must not be cancelled by the browser that happened to trigger it, or a
		// user navigating away would leave the cache permanently stale.
		go c.rebuild(context.WithoutCancel(ctx), build)
		return warm, nil
	}

	if warm != nil {
		return warm, nil
	}

	// Nothing to serve: the first request of the process waits for the real thing.
	snap, err := c.rebuild(ctx, build)
	if err != nil {
		return nil, err
	}
	return snap, nil
}

// rebuild runs the aggregates and stores the result, clearing the in-flight flag either way.
func (c *sidebarCache) rebuild(ctx context.Context, build func(context.Context) (*sidebarData, error)) (*sidebarData, error) {
	snap, err := build(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.building = false

	if err != nil {
		// Keep whatever we had: a failed aggregate should not blank the navigation on a page
		// that is otherwise fine.
		return c.snap, err
	}
	c.snap = snap
	c.at = time.Now()
	return snap, nil
}

// buildSidebar runs the aggregates. Every failure here is logged and tolerated: the grid
// and the asset page are both useful without a folder tree.
func (s *Server) buildSidebar(ctx context.Context) (*sidebarData, error) {
	data := &sidebarData{}

	stats, err := s.index.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("sidebar stats: %w", err)
	}
	data.Stats = stats

	// 40, not 18 (M17). Measured on the real library: 673 colour buckets clear the
	// 2% threshold, and sorted by coverage the first eighteen are all dark browns and
	// greys — the greens, yellows and blues start at about the twentieth. A filter
	// that cannot offer green is not a colour filter. The SQL cost is unchanged: the
	// grouping already runs over everything and LIMIT only decides where to stop.
	if colours, err := s.index.LibraryColours(ctx, 40); err != nil {
		s.log.ErrorContext(ctx, "loading library colours failed", "error", err)
	} else {
		data.Colours = colours
	}

	// Depth-limited: a vendor pack can nest ten levels of format folders, and a sidebar
	// that renders all of them is a scrollbar rather than navigation.
	if tree, err := s.index.Tree(ctx, index.DefaultTreeDepth); err != nil {
		s.log.ErrorContext(ctx, "building the folder tree failed", "error", err)
	} else {
		data.Tree = tree
	}

	if searches, err := s.saved.List(ctx); err != nil {
		s.log.ErrorContext(ctx, "listing saved searches failed", "error", err)
	} else {
		data.Saved = searches
	}

	// One count over `packs`, which is small — this is the number that makes the
	// provenance backlog worth visiting instead of a link you never click.
	if err := s.db.Reader.QueryRowContext(ctx,
		`SELECT count(*) FROM packs WHERE provenance_state = 'needs_provenance'`,
	).Scan(&data.NeedsProvenance); err != nil {
		s.log.ErrorContext(ctx, "counting packs needing provenance failed", "error", err)
	}

	if jobStats, err := s.jobs.Stats(ctx); err != nil {
		s.log.ErrorContext(ctx, "job stats failed", "error", err)
	} else {
		data.ActiveJobs = jobStats.Pending()
	}

	if facets, err := s.index.Facets(ctx, index.ListOptions{}, index.DefaultFacetLimit); err != nil {
		s.log.ErrorContext(ctx, "facets failed", "error", err)
	} else {
		data.Facets = facets
	}

	if last, err := s.lastScan(ctx, stats.Assets); err != nil {
		s.log.ErrorContext(ctx, "reading the last scan time failed", "error", err)
	} else {
		data.LastScan = last
	}

	return data, nil
}

// lastScan finds when a scan last touched the library. Nil means none ever has, which is
// a real state on a fresh install and reads better than a zero time.
//
// Read from assets.last_verified_at rather than from the jobs table, which was the first
// attempt and was wrong: `ambar scan` on the command line does the work directly instead
// of enqueuing it, so a library scanned from a shell — the documented way to do the first
// one — reported "not scanned yet" while showing six thousand assets. Every scan stamps
// last_verified_at on every file it sees, whichever path it came through.
func (s *Server) lastScan(ctx context.Context, assets int) (*lastScanInfo, error) {
	var verified *int64
	if err := s.db.Reader.QueryRowContext(ctx,
		`SELECT max(last_verified_at) FROM assets`,
	).Scan(&verified); err != nil {
		return nil, fmt.Errorf("last scan: %w", err)
	}
	if verified == nil {
		return nil, nil
	}
	return &lastScanInfo{At: time.Unix(*verified, 0), Assets: assets}, nil
}

// applySidebar fills the navigation half of pageData. Callers set data.Nav themselves,
// since only the handler knows which page it is.
func (s *Server) applySidebar(ctx context.Context, data *pageData, currentDir string) {
	snap, err := s.nav.get(ctx, s.buildSidebar)
	if err != nil {
		s.log.ErrorContext(ctx, "building the sidebar failed", "error", err)
		return
	}

	stats := snap.Stats
	data.Stats = &stats
	data.Colours = snap.Colours
	data.SavedSearches = snap.Saved
	data.NeedsProvenance = snap.NeedsProvenance
	data.ActiveJobs = snap.ActiveJobs
	data.LastScan = snap.LastScan

	// The cached facets apply only to an unfiltered browse; a filtered view computes its own.
	data.Facets = snap.Facets

	if snap.Tree != nil {
		data.Tree = index.Flatten(snap.Tree, currentDir)
		data.TreeTotal = snap.Tree.Assets
	}
}
