// Package server wires the HTTP routes, middleware and templates together.
package server

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	netpprof "net/http/pprof"
	"strings"
	"sync"
	"time"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/dupes"
	"github.com/datcal/ambar/internal/httpx"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/junk"
	"github.com/datcal/ambar/internal/projects"
	"github.com/datcal/ambar/internal/provenance"
	"github.com/datcal/ambar/internal/removal"
	"github.com/datcal/ambar/internal/savedsearch"
	"github.com/datcal/ambar/internal/sidecar"
	"github.com/datcal/ambar/internal/tags"
	"github.com/datcal/ambar/internal/web"
)

// BuildInfo is stamped in at link time by the Makefile.
type BuildInfo struct {
	Version string
	Commit  string
}

// Server holds everything the handlers need. One instance per process.
type Server struct {
	cfg   *config.Config
	db    *db.DB
	log   *slog.Logger
	build BuildInfo

	index    *index.Indexer
	tags     *tags.Store
	saved    *savedsearch.Store
	prov     *provenance.Store
	projects *projects.Store
	sidecars *sidecar.Manager
	jobs     *jobs.Queue

	// The shared library sidebar's cached aggregates (M16). See sidebar.go for why the
	// cache is not optional.
	nav *sidebarCache

	// The M13 removal path (§9.1). removals plans and refuses; trash carries plans
	// out and owns the trash directory. Kept as two fields rather than one so the
	// read-only half is obviously read-only at the call site.
	removals *removal.Planner
	trash    *removal.Executor
	users    *auth.UserStore
	tokens   *auth.TokenStore
	sessions *auth.SessionStore
	audit    *audit.Logger
	csrf     *auth.CSRF
	realIP   *httpx.RealIP

	// Separate buckets so one host hammering many accounts and many hosts
	// hammering one account are both caught (§11).
	loginByIP   *auth.Limiter
	loginByUser *auth.Limiter

	// dummyHash equalizes the cost of a login against a username that does not
	// exist. Computed once here because argon2id is deliberately slow.
	dummyHash string

	// The dedupe link probe writes a temporary file, so it runs once on first use
	// rather than on every health check or page render (§9.1).
	linkOnce  sync.Once
	linkProbe removal.LinkSupport

	templates map[string]*template.Template
	handler   http.Handler
	startedAt time.Time
}

// New builds the server. It fails rather than starting degraded: a template
// that does not parse or a missing static asset is a build mistake, and finding
// out at startup beats finding out when a user hits the page.
func New(cfg *config.Config, database *db.DB, indexer *index.Indexer, queue *jobs.Queue,
	log *slog.Logger, build BuildInfo) (*Server, error) {

	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}

	dummy, err := auth.DummyHash()
	if err != nil {
		return nil, fmt.Errorf("precompute dummy password hash: %w", err)
	}

	s := &Server{
		cfg:      cfg,
		db:       database,
		index:    indexer,
		tags:     tags.NewStore(database),
		saved:    savedsearch.NewStore(database),
		prov:     provenance.NewStore(database),
		projects: projects.NewStore(database),
		sidecars: sidecar.New(database, sidecar.Options{
			LibraryRoot: cfg.LibraryRoot, DataRoot: cfg.DataRoot, Readonly: cfg.LibraryReadonly, Log: log,
		}),
		jobs:     queue,
		nav:      &sidebarCache{},
		removals: removal.NewPlanner(database, cfg.LibraryRoot, cfg.DataRoot, cfg.TrashDir),
		trash: removal.NewExecutor(database, cfg.LibraryRoot, cfg.DataRoot, cfg.TrashDir,
			cfg.DedupeLinkMode, audit.New(database, log), log),
		log:         log,
		build:       build,
		users:       auth.NewUserStore(database),
		tokens:      auth.NewTokenStore(database),
		sessions:    auth.NewSessionStore(database),
		audit:       audit.New(database, log),
		csrf:        auth.NewCSRF(cfg.SessionSecret, cfg.CookieSecure),
		realIP:      httpx.New(cfg.TrustedProxies, cfg.RealIPHeader),
		loginByIP:   auth.NewLimiter(auth.LoginAttemptsPerIP, auth.LoginWindow),
		loginByUser: auth.NewLimiter(auth.LoginAttemptsPerUsername, auth.LoginWindow),
		dummyHash:   dummy,
		templates:   templates,
		startedAt:   time.Now(),
	}
	s.handler = s.routes()
	return s, nil
}

// Handler is the fully wrapped http.Handler.
func (s *Server) Handler() http.Handler { return s.handler }

// Sessions exposes the store so `serve` can sweep expired rows at startup.
func (s *Server) Sessions() *auth.SessionStore { return s.sessions }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Liveness. Unauthenticated and deliberately uninformative: this is the
	// container HEALTHCHECK, and §2 warns the app may end up publicly reachable
	// with no edge rate limiting, so it must not become a fingerprinting
	// endpoint. The detailed report lives behind auth at /api/v1/healthz.
	mux.HandleFunc("GET /healthz", s.handleLiveness)

	// {$} anchors the pattern to the exact path, so /nonsense 404s instead of
	// being swallowed by a catch-all.
	// §0: "the whole point is the assets". The library grid is the landing page, and
	// the dashboard it replaced moved to /status.
	mux.Handle("GET /{$}", auth.RequireUser(http.HandlerFunc(s.handleAssets)))
	mux.Handle("GET /status", auth.RequireUser(http.HandlerFunc(s.handleStatus)))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.Handle("GET /assets", auth.RequireUser(http.HandlerFunc(s.handleAssets)))
	// The colour picker beside the sidebar's swatches (M17): pick a colour, get the
	// search. A redirect so the form needs no JavaScript.
	mux.Handle("GET /colour", auth.RequireUser(http.HandlerFunc(s.handleColourSearch)))
	mux.Handle("GET /assets/{id}", auth.RequireUser(http.HandlerFunc(s.handleAsset)))
	mux.Handle("GET /assets/{id}/download", auth.RequireUser(http.HandlerFunc(s.handleAssetDownload)))

	// Tagging (§7). State-changing posts are CSRF-protected by the middleware.
	mux.Handle("POST /assets/{id}/tags", auth.RequireUser(http.HandlerFunc(s.handleAssetTagAdd)))
	mux.Handle("POST /assets/{id}/tags/remove", auth.RequireUser(http.HandlerFunc(s.handleAssetTagRemove)))
	mux.Handle("GET /api/v1/tags/suggest", auth.RequireUser(http.HandlerFunc(s.handleTagSuggest)))
	// M16: completion for the toolbar's search box — the query language, the library's own
	// vocabulary, and filenames.
	mux.Handle("GET /api/v1/suggest", auth.RequireUser(http.HandlerFunc(s.handleSearchSuggest)))
	mux.Handle("POST /assets/tags/bulk", auth.RequireUser(http.HandlerFunc(s.handleBulkTag)))

	// Saved searches (§7).
	mux.Handle("POST /searches", auth.RequireUser(http.HandlerFunc(s.handleSaveSearch)))
	mux.Handle("POST /searches/{id}/delete", auth.RequireUser(http.HandlerFunc(s.handleDeleteSearch)))

	// Generated derivatives. Served inline, which is safe only because these bytes
	// came from our own encoder — see the note on handleThumb.
	mux.Handle("GET /assets/{id}/thumb", auth.RequireUser(http.HandlerFunc(s.handleThumb)))
	mux.Handle("GET /assets/{id}/preview.webp", auth.RequireUser(http.HandlerFunc(s.handlePreview)))
	mux.Handle("GET /assets/{id}/anim.gif", auth.RequireUser(http.HandlerFunc(s.handleAnimation)))

	// Audio (§8): the waveform peaks and the original for in-page playback.
	mux.Handle("GET /assets/{id}/peaks.json", auth.RequireUser(http.HandlerFunc(s.handlePeaks)))
	mux.Handle("GET /assets/{id}/audio", auth.RequireUser(http.HandlerFunc(s.handleAudio)))

	// 3D (§8): the normalised preview.glb the three.js viewer loads.
	mux.Handle("GET /assets/{id}/preview.glb", auth.RequireUser(http.HandlerFunc(s.handleModelPreview)))
	// M14: the original model plus the files it references, so .obj and .fbx can be
	// viewed in the browser instead of waiting for Blender.
	mux.Handle("GET /assets/{id}/file/{name...}", auth.RequireUser(http.HandlerFunc(s.handleAssetFile)))
	// M15: the browser hands back a thumbnail it rendered for a model, because the
	// server has no renderer and Blender is optional (§6).
	mux.Handle("POST /assets/{id}/thumb", auth.RequireUser(http.HandlerFunc(s.handleModelThumbUpload)))
	// M15: a font's bytes, so the detail page can set your own text in it.
	mux.Handle("GET /assets/{id}/font", auth.RequireUser(http.HandlerFunc(s.handleAssetFont)))

	// Palette export (§8): one route per interchange format (.gpl, .png, .txt, …).
	mux.Handle("GET /assets/{id}/palette/{format}", auth.RequireUser(http.HandlerFunc(s.handlePaletteExport)))

	// Spritesheets (§6): the animated preview and the confirm/correct action.
	mux.Handle("GET /assets/{id}/sheet.gif", auth.RequireUser(http.HandlerFunc(s.handleSheet)))
	mux.Handle("POST /assets/{id}/frames", auth.RequireUser(http.HandlerFunc(s.handleFrames)))

	// Background work (§12). POST /scan enqueues and returns immediately, which is
	// what invariant 8 requires of every scan trigger.
	// Runtime profiling behind auth. Only trusted users have accounts (§11), so the
	// same session gate the rest of the app uses is enough — /debug/pprof/profile
	// can pin a core for its sample window, so it must not be reachable anonymously.
	mux.Handle("GET /debug/pprof/", auth.RequireUser(http.HandlerFunc(netpprof.Index)))
	mux.Handle("GET /debug/pprof/cmdline", auth.RequireUser(http.HandlerFunc(netpprof.Cmdline)))
	mux.Handle("GET /debug/pprof/profile", auth.RequireUser(http.HandlerFunc(netpprof.Profile)))
	mux.Handle("GET /debug/pprof/symbol", auth.RequireUser(http.HandlerFunc(netpprof.Symbol)))
	mux.Handle("POST /debug/pprof/symbol", auth.RequireUser(http.HandlerFunc(netpprof.Symbol)))
	mux.Handle("GET /debug/pprof/trace", auth.RequireUser(http.HandlerFunc(netpprof.Trace)))

	mux.Handle("GET /jobs", auth.RequireUser(http.HandlerFunc(s.handleJobs)))
	// M16: answers in place instead of redirecting to /jobs, and /api/v1/jobs/status is what
	// the sidebar and the jobs page poll while work is in flight.
	mux.Handle("POST /scan", auth.RequireUser(http.HandlerFunc(s.handleScanNow)))
	mux.Handle("GET /api/v1/jobs/status", auth.RequireUser(http.HandlerFunc(s.handleJobStatus)))
	mux.Handle("POST /jobs/retry-failed", auth.RequireUser(http.HandlerFunc(s.handleRetryFailed)))

	// Junk view (§9.1, M12): reporting only. GET shows the cached report; POST
	// enqueues a fresh background sweep. No removal path — that is M13.
	mux.Handle("GET /junk", auth.RequireUser(http.HandlerFunc(s.handleJunk)))
	mux.Handle("POST /junk/scan", auth.RequireUser(http.HandlerFunc(s.handleJunkScan)))

	// §7 pack palette consistency: a read-only comparison, so one GET.

	// Duplicates and the removal path (§9.1, M13). Every destructive route is a
	// POST behind CSRF, and /removals/plan is the preview step no apply can skip.
	mux.Handle("GET /dupes", auth.RequireUser(http.HandlerFunc(s.handleDupes)))
	mux.Handle("POST /dupes/scan", auth.RequireUser(http.HandlerFunc(s.handleDupesScan)))
	mux.Handle("POST /removals/plan", auth.RequireUser(http.HandlerFunc(s.handleRemovalPlan)))
	mux.Handle("POST /removals/apply", auth.RequireUser(http.HandlerFunc(s.handleRemovalApply)))
	mux.Handle("POST /removals/script", auth.RequireUser(http.HandlerFunc(s.handleRemovalScript)))
	mux.Handle("GET /trash", auth.RequireUser(http.HandlerFunc(s.handleTrash)))
	mux.Handle("POST /trash/{id}/restore", auth.RequireUser(http.HandlerFunc(s.handleTrashRestore)))
	mux.Handle("POST /trash/purge", auth.RequireUser(http.HandlerFunc(s.handleTrashPurge)))

	// Ingest (§5): the web-upload path. _inbox polling runs in `serve`, off the web.
	// "Upload" is what this is from the user's side; /ingest stays as an alias so old
	// links and bookmarks keep working (§5 calls the pipeline ingest, and the code
	// still does).
	mux.Handle("GET /upload", auth.RequireUser(http.HandlerFunc(s.handleIngestForm)))
	mux.Handle("GET /ingest", auth.RequireUser(http.HandlerFunc(s.handleIngestForm)))
	mux.Handle("POST /ingest/upload", auth.RequireUser(http.HandlerFunc(s.handleUpload)))
	// M16: the upload lands the bytes, this starts the extraction once the destination has
	// been chosen. Two steps, because the destination is a question about the real archive.
	mux.Handle("POST /ingest/start", auth.RequireUser(http.HandlerFunc(s.handleIngestStart)))

	// Provenance and licensing (§9).
	mux.Handle("GET /packs/{id}/provenance", auth.RequireUser(http.HandlerFunc(s.handlePackProvenanceForm)))
	mux.Handle("POST /packs/{id}/provenance", auth.RequireUser(http.HandlerFunc(s.handlePackProvenanceSave)))
	// M16: the two provenance fields that actually get filled in, from the asset page.
	mux.Handle("POST /assets/{id}/provenance", auth.RequireUser(http.HandlerFunc(s.handleAssetProvenanceSave)))

	// Session-authed, because the status page fetches it from the browser.
	mux.Handle("GET /api/v1/healthz", auth.RequireUser(http.HandlerFunc(s.handleHealth)))

	// The JSON API (§10), authenticated by a bearer token rather than a session.
	// All reads require the `read` scope; write endpoints (projects) arrive in M9.
	api := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.tokens.RequireToken(auth.ScopeRead, h))
	}
	// M16: /api/v1/healthz is registered above for the browser, but it is also the natural
	// "can I reach the library" probe for an API client — and it answered 401 to a perfectly
	// good token, which sent anyone testing the Godot plugin off to re-check their token.
	// /api/v1/ping is the same report, token-authed.
	api("GET /api/v1/ping", s.handleHealth)
	api("GET /api/v1/search", s.handleAPISearch)
	// M18: the browse orders `search?sort=` accepts, so a client's dropdown is not a
	// hand-copied list that goes stale the next time one is added.
	api("GET /api/v1/sorts", s.handleAPISorts)
	api("GET /api/v1/assets/{id}", s.handleAPIAsset)
	api("GET /api/v1/assets/{id}/file", s.handleAssetDownload)
	api("GET /api/v1/assets/{id}/thumb", s.handleThumb)
	// M18: the full-size preview, for looking at an asset properly *before* importing
	// it. It existed only behind the session cookie, so the editor plugin could offer a
	// 96-pixel thumbnail and nothing between that and downloading the file.
	api("GET /api/v1/assets/{id}/preview.webp", s.handlePreview)
	api("GET /api/v1/assets/{id}/anim.gif", s.handleAnimation)
	api("GET /api/v1/assets/{id}/sheet.gif", s.handleSheet)
	api("GET /api/v1/assets/{id}/preview.glb", s.handleModelPreview)
	api("GET /api/v1/assets/{id}/peaks.json", s.handlePeaks)
	api("GET /api/v1/packs/{id}", s.handleAPIPack)
	api("GET /api/v1/tags", s.handleAPITags)
	api("GET /api/v1/projects/{project}/credits.md", s.handleAPICredits)
	// M18: what a project holds, for the plugin's "in this project" screen — and the only way
	// to find the imports the server was never told about, which §10 promised were replayable.
	api("GET /api/v1/projects/{project}/uses", s.handleAPIProjectUses)

	// Write endpoints (§10): the Godot plugin records what it imports.
	apiWrite := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.tokens.RequireToken(auth.ScopeWrite, h))
	}
	apiWrite("POST /api/v1/projects/{project}/uses", s.handleAPIRecordUse)
	apiWrite("DELETE /api/v1/projects/{project}/uses/{id}", s.handleAPIRemoveUse)
	// M18: the same "somebody's renderer drew this model, keep it" endpoint the browser
	// has had since M15, for a client with a token instead of a cookie.
	//
	// The Godot plugin runs inside a game engine, which is the one thing this server does
	// not have and deliberately will not add (§6 keeps Blender optional). It can read the
	// preview.glb derive already writes, so it can fill in the thumbnails nobody has
	// browsed to yet — and every viewer, web included, gets them from then on. The
	// handler refuses to overwrite a thumbnail that already exists, so a second client
	// rendering the same model costs one 204 and nothing else.
	apiWrite("POST /api/v1/assets/{id}/thumb", s.handleModelThumbUpload)

	// API token management (§11), session-authed like the rest of the UI.
	// Settings: users (§11 has no self-registration, so creating one is behind auth)
	// and the tokens page.
	mux.Handle("GET /settings", auth.RequireUser(http.HandlerFunc(s.handleSettings)))
	// The ambar:// helper, generated per platform (M15). A GET because it is a
	// download of a script the operator is expected to read before running.
	mux.Handle("GET /settings/open-helper", auth.RequireUser(http.HandlerFunc(s.handleOpenHelper)))
	mux.Handle("POST /settings/users", auth.RequireUser(http.HandlerFunc(s.handleUserCreate)))
	mux.Handle("GET /settings/tokens", auth.RequireUser(http.HandlerFunc(s.handleTokensPage)))
	mux.Handle("POST /settings/tokens", auth.RequireUser(http.HandlerFunc(s.handleTokenCreate)))
	mux.Handle("POST /settings/tokens/{id}/revoke", auth.RequireUser(http.HandlerFunc(s.handleTokenRevoke)))

	mux.Handle("GET /static/", s.staticHandler())

	authn := auth.NewAuthenticator(s.sessions, s.cfg.CookieSecure, s.log)

	// Assigned innermost first, so the LAST line is the outermost middleware.
	// Execution order is therefore:
	//
	//   recoverPanic -> requestID -> realIP -> accessLog -> securityHeaders
	//   -> csrf.Ensure -> authn.Load -> csrf.Protect -> mux
	//
	// Two ordering constraints that are easy to get wrong, because a context
	// value only ever propagates *downward* into inner handlers:
	//
	//   - requestID and realIP must both be OUTSIDE accessLog, or the log line
	//     cannot see what they resolved and silently records an empty value.
	//   - recoverPanic is outermost so it catches a panic in any other
	//     middleware, and accessLog is inside it so a panicking request is still
	//     logged with its 500.
	var h http.Handler = mux
	h = s.csrf.Protect(h)
	h = authn.Load(h)
	h = s.csrf.Ensure(h)
	h = s.securityHeaders(h)
	h = s.accessLog(h)
	h = s.realIP.Middleware(h)
	h = s.requestID(h)
	h = s.recoverPanic(h)
	return h
}

// staticHandler serves the embedded assets.
func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(web.FS, "static")
	if err != nil {
		// Unreachable: the directory is embedded at compile time.
		panic("static assets missing from embed: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	// The assets change only when the binary does, and the templates append ?v=<version>
	// to every reference, so a released build's URLs are unique and can be cached hard.
	//
	// A dev or dirty build is being edited right now, and the old one-hour TTL with no
	// version in the URL meant every CSS change needed a manual hard refresh — the exact
	// trap that makes a UI change look like it did nothing.
	cache := "public, max-age=31536000, immutable"
	if v := s.build.Version; v == "" || v == "dev" || strings.Contains(v, "dirty") {
		cache = "no-cache"
	}

	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", cache)
		fileServer.ServeHTTP(w, r)
	}))
}

// pageData is the single data type passed to every template. One struct with a
// few unused fields per page beats a type per page at this size.
type pageData struct {
	CSRF     string
	User     *auth.User
	Version  string
	Error    string
	Next     string
	Username string

	LibraryRoot string
	DataRoot    string

	// Asset browsing (M1) and grouping (M2). Page holds groups rather than
	// individual assets — see §5.1 and index.ListGroups.
	Page            *index.GroupPage
	Group           *index.Group
	Variants        []index.Asset
	Asset           *index.Asset
	Stats           *index.Stats
	Search          string
	Kind            string
	IncludeMissing  bool
	IncludeDisabled bool

	// The grid's pager and sort control (M16). PageURL builds a link to another page with
	// every filter intact; it is a func so the template can ask for an arbitrary number
	// without the handler precomputing fifty-seven URLs.
	// Per-tile quick actions (M16), keyed by asset id. Empty when AMBAR_LOCAL_LIBRARY_PATH is
	// unset, which is what makes the whole row conditional in the template.
	TileApps  map[int64][]openApp
	TilePaths map[int64]string

	Sort        string
	SortOptions []index.SortOrder
	PageSizes   []int
	PageURL     func(int) string

	// Previous/next in the browse order, and the filters that define it (M16). Nil at
	// either end of the list.
	PrevAsset *index.Neighbour
	NextAsset *index.Neighbour
	// BrowseQuery is those filters as a query string, "?q=…", or empty.
	BrowseQuery string

	// The shared navigation (M16). Nav names the current page so the sidebar and the
	// toolbar can mark it; the counts come from the cached snapshot in sidebar.go.
	Nav             string
	LastScan        *lastScanInfo
	ActiveJobs      int
	NeedsProvenance int

	// Tagging (M3).
	AssetTags     []tags.AssetTag
	Facets        []index.Facet
	SavedSearches []savedsearch.SavedSearch
	TagError      string
	Suggest       []string
	// Suggestions is the search box's grouped completion list (M16).
	Suggestions []index.Suggestion
	Flash       string

	// Ingest (M4). Folders are the top-level library directories a pack can be filed into
	// (M16); MaxUploadSize <= 0 means no cap.
	Readonly      bool
	MaxUploadSize int64
	Folders       []string

	// API tokens (M8).
	Tokens     []auth.Token
	NewToken   string
	TokenError string

	// Provenance (§9, M4).
	PackSummaries []provenance.PackSummary
	ProvView      string
	Prov          *provenance.Provenance
	Licenses      []provenance.License
	ProvPackID    int64
	ProvPackName  string
	ProvPackRel   string
	Sniff         provenance.Sniffed

	// The jobs page (§12).
	Jobs        []jobs.Job
	JobStats    *jobs.Stats
	DeriveStats *derive.Stats
	JobState    string

	// Junk view (§9.1, M12). Nil until a sweep has run. JunkRunning is true while a
	// sweep is queued or in flight, so the page can say so.
	Junk        *junk.StoredReport
	JunkRunning bool

	// Duplicates, the removal preview and the trash (§9.1, M13).
	Dupes        *dupes.StoredReport
	DupesRunning bool
	// Plan is the removal preview. Non-nil only on the confirmation page, and an
	// empty plan there means "nothing in that selection may happen".
	Plan     *removal.Plan
	PlanForm removalForm
	// LinkMode and LinkSupport describe AMBAR_DEDUPE_LINK_MODE and whether the
	// filesystem actually supports it, so the UI can recommend the right action.
	LinkMode    string
	LinkSupport removal.LinkSupport
	// Workspace switches base.html from a centred document to the full-height
	// three-pane library shell.
	Workspace bool

	// M14 "open in…": the asset's path on the operator's machine, the applications
	// worth suggesting for it, and the Godot projects already using it.
	Local    *LocalPath
	OpenWith []string
	// OpenApps are the ambar:// launch links for this asset (M15).
	OpenApps    []openApp
	ProjectUses []projects.AssetUse

	// ViewerSrc is the file the 3D viewer loads for this asset: a derived preview.glb
	// when one exists, otherwise the original through the companion route.
	ViewerSrc string

	// NeedsModelThumbs is true when the grid holds a model with no thumbnail, so the
	// page loads three.js and asks the browser to render one (M15).
	NeedsModelThumbs bool

	// Settings (M15): the user list, the form's error, and the password rule to show.
	Users             []auth.User
	UserError         string
	MinPasswordLength int

	// Colours is the library's own dominant palette, as a clickable filter (M15).
	Colours []index.PackColour

	// The M14 folder tree: the visible nodes, the browsed directory, and the whole
	// library's count for the "everything" row.
	Tree      []index.FlatNode
	TreeTotal int
	Dir       string

	// §7 pack palette consistency.

	Trash          []*removal.Batch
	TrashBytes     int64
	TrashDir       string
	TrashRetention time.Duration
	RemovalRunning bool
}

func (s *Server) newPageData(r *http.Request) pageData {
	d := pageData{
		CSRF:        auth.TokenFromContext(r.Context()),
		Version:     s.build.Version,
		LibraryRoot: s.cfg.LibraryRoot,
		DataRoot:    s.cfg.DataRoot,
	}
	if u, ok := auth.UserFromContext(r.Context()); ok {
		d.User = &u
	}
	return d
}

// parseTemplates builds one template set per page, each combining base.html
// with that page's content block.
func parseTemplates() (map[string]*template.Template, error) {
	// Helpers the templates need. Kept small and side-effect free: anything that
	// can fail belongs in a handler, not in a template.
	funcs := template.FuncMap{
		"bytes":      FormatBytes,
		"libraryDir": libraryDir,
		"jobAge":     formatJobAge,
		// sizes builds a literal list, which templates cannot do on their own. Used by
		// the font specimen's size ladder (M15).
		"sizes": func(v ...int) []int { return v },
	}

	pages := []string{"login.html", "index.html", "assets.html", "asset.html", "jobs.html",
		"ingest.html", "pack_provenance.html", "tokens.html", "junk.html",
		"dupes.html", "removal_confirm.html", "trash.html", "settings.html"}
	out := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		// nav.html carries the library sidebar (M16). It is parsed into every page's set
		// rather than into the two that use it today, because the whole point of the
		// change is that navigation does not move when you open something — a page that
		// wants the sidebar should only have to ask for it.
		t, err := template.New("base.html").Funcs(funcs).
			ParseFS(web.FS, "templates/base.html", "templates/nav.html", "templates/"+page)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		out[page] = t
	}
	return out, nil
}

// render writes a page.
//
// The template is executed into a buffer first: a template error halfway through
// would otherwise leave a half-written body with a 200 already sent, which is
// impossible to debug from the client side.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, status int, data pageData) {
	t, ok := s.templates[page]
	if !ok {
		s.log.ErrorContext(r.Context(), "unknown template requested", "page", page)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		s.log.ErrorContext(r.Context(), "template execution failed", "page", page, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Authenticated HTML is per-user; a shared cache must not keep it.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		s.log.DebugContext(r.Context(), "client went away mid-response", "error", err)
	}
}

// renderPartial executes one named block from a page's template set, for htmx
// responses that replace a fragment rather than the whole page. Same
// buffer-first discipline as render, for the same reason.
func (s *Server) renderPartial(w http.ResponseWriter, r *http.Request, page, block string, status int, data pageData) {
	t, ok := s.templates[page]
	if !ok {
		s.log.ErrorContext(r.Context(), "unknown template requested", "page", page)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, data); err != nil {
		s.log.ErrorContext(r.Context(), "partial execution failed", "page", page, "block", block, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		s.log.DebugContext(r.Context(), "client went away mid-response", "error", err)
	}
}

// handleStatus is the old landing dashboard: counts, derivative progress and the
// links to the operational views. It is not the front door any more — the library
// is — but the numbers are still worth one page.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(r)
	data.Nav = "status"

	// A failing stats query should not blank the page; the rest of it is
	// still useful, and the health endpoint is the place that reports trouble.
	if stats, err := s.index.Stats(r.Context()); err != nil {
		s.log.ErrorContext(r.Context(), "index stats failed", "error", err)
	} else {
		data.Stats = &stats
	}

	s.render(w, r, "index.html", http.StatusOK, data)
}
