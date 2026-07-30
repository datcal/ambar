// Package server wires the HTTP routes, middleware and templates together.
package server

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/httpx"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/junk"
	"github.com/datcal/ambar/internal/projects"
	"github.com/datcal/ambar/internal/provenance"
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
		jobs:        queue,
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
	mux.Handle("GET /{$}", auth.RequireUser(http.HandlerFunc(s.handleIndex)))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLoginSubmit)
	mux.HandleFunc("POST /logout", s.handleLogout)

	mux.Handle("GET /assets", auth.RequireUser(http.HandlerFunc(s.handleAssets)))
	mux.Handle("GET /assets/{id}", auth.RequireUser(http.HandlerFunc(s.handleAsset)))
	mux.Handle("GET /assets/{id}/download", auth.RequireUser(http.HandlerFunc(s.handleAssetDownload)))

	// Tagging (§7). State-changing posts are CSRF-protected by the middleware.
	mux.Handle("POST /assets/{id}/tags", auth.RequireUser(http.HandlerFunc(s.handleAssetTagAdd)))
	mux.Handle("POST /assets/{id}/tags/remove", auth.RequireUser(http.HandlerFunc(s.handleAssetTagRemove)))
	mux.Handle("GET /api/v1/tags/suggest", auth.RequireUser(http.HandlerFunc(s.handleTagSuggest)))
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

	// Palette export (§8): one route per interchange format (.gpl, .png, .txt, …).
	mux.Handle("GET /assets/{id}/palette/{format}", auth.RequireUser(http.HandlerFunc(s.handlePaletteExport)))

	// Spritesheets (§6): the animated preview and the confirm/correct action.
	mux.Handle("GET /assets/{id}/sheet.gif", auth.RequireUser(http.HandlerFunc(s.handleSheet)))
	mux.Handle("POST /assets/{id}/frames", auth.RequireUser(http.HandlerFunc(s.handleFrames)))

	// Background work (§12). POST /scan enqueues and returns immediately, which is
	// what invariant 8 requires of every scan trigger.
	mux.Handle("GET /jobs", auth.RequireUser(http.HandlerFunc(s.handleJobs)))
	mux.Handle("POST /scan", auth.RequireUser(http.HandlerFunc(s.handleScan)))
	mux.Handle("POST /jobs/retry-failed", auth.RequireUser(http.HandlerFunc(s.handleRetryFailed)))

	// Junk view (§9.1, M12): reporting only. GET shows the cached report; POST
	// enqueues a fresh background sweep. No removal path — that is M13.
	mux.Handle("GET /junk", auth.RequireUser(http.HandlerFunc(s.handleJunk)))
	mux.Handle("POST /junk/scan", auth.RequireUser(http.HandlerFunc(s.handleJunkScan)))

	// Ingest (§5): the web-upload path. _inbox polling runs in `serve`, off the web.
	mux.Handle("GET /ingest", auth.RequireUser(http.HandlerFunc(s.handleIngestForm)))
	mux.Handle("POST /ingest/upload", auth.RequireUser(http.HandlerFunc(s.handleUpload)))

	// Provenance and licensing (§9).
	mux.Handle("GET /provenance", auth.RequireUser(http.HandlerFunc(s.handleProvenanceList)))
	mux.Handle("POST /provenance/bulk", auth.RequireUser(http.HandlerFunc(s.handleProvenanceBulk)))
	mux.Handle("GET /packs/{id}/provenance", auth.RequireUser(http.HandlerFunc(s.handlePackProvenanceForm)))
	mux.Handle("POST /packs/{id}/provenance", auth.RequireUser(http.HandlerFunc(s.handlePackProvenanceSave)))

	mux.Handle("GET /api/v1/healthz", auth.RequireUser(http.HandlerFunc(s.handleHealth)))

	// The JSON API (§10), authenticated by a bearer token rather than a session.
	// All reads require the `read` scope; write endpoints (projects) arrive in M9.
	api := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.tokens.RequireToken(auth.ScopeRead, h))
	}
	api("GET /api/v1/search", s.handleAPISearch)
	api("GET /api/v1/assets/{id}", s.handleAPIAsset)
	api("GET /api/v1/assets/{id}/file", s.handleAssetDownload)
	api("GET /api/v1/assets/{id}/thumb", s.handleThumb)
	api("GET /api/v1/assets/{id}/preview.glb", s.handleModelPreview)
	api("GET /api/v1/assets/{id}/peaks.json", s.handlePeaks)
	api("GET /api/v1/packs/{id}", s.handleAPIPack)
	api("GET /api/v1/tags", s.handleAPITags)
	api("GET /api/v1/projects/{project}/credits.md", s.handleAPICredits)

	// Write endpoints (§10): the Godot plugin records what it imports.
	apiWrite := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, s.tokens.RequireToken(auth.ScopeWrite, h))
	}
	apiWrite("POST /api/v1/projects/{project}/uses", s.handleAPIRecordUse)
	apiWrite("DELETE /api/v1/projects/{project}/uses/{id}", s.handleAPIRemoveUse)

	// API token management (§11), session-authed like the rest of the UI.
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
	return http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The assets change only when the binary does, so tie caching to the
		// build rather than guessing at a TTL.
		w.Header().Set("Cache-Control", "public, max-age=3600")
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
	Page           *index.GroupPage
	Group          *index.Group
	Variants       []index.Asset
	Asset          *index.Asset
	Stats          *index.Stats
	Search         string
	Kind           string
	IncludeMissing bool
	NextURL        string

	// Tagging (M3).
	AssetTags     []tags.AssetTag
	Facets        []index.Facet
	SavedSearches []savedsearch.SavedSearch
	TagError      string
	Suggest       []string
	Flash         string

	// Ingest (M4).
	Readonly      bool
	MaxUploadSize int64

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
	}

	pages := []string{"login.html", "index.html", "assets.html", "asset.html", "jobs.html",
		"ingest.html", "provenance.html", "pack_provenance.html", "tokens.html", "junk.html"}
	out := make(map[string]*template.Template, len(pages))
	for _, page := range pages {
		t, err := template.New("base.html").Funcs(funcs).
			ParseFS(web.FS, "templates/base.html", "templates/"+page)
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

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(r)

	// A failing stats query should not blank the home page; the rest of it is
	// still useful, and the health endpoint is the place that reports trouble.
	if stats, err := s.index.Stats(r.Context()); err != nil {
		s.log.ErrorContext(r.Context(), "index stats failed", "error", err)
	} else {
		data.Stats = &stats
	}

	s.render(w, r, "index.html", http.StatusOK, data)
}
