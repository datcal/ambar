// Package web embeds the server-rendered templates and static assets.
//
// §2: server-rendered HTML plus htmx, no SPA and no bundler. htmx is vendored
// as a single file under static/ rather than fetched from a CDN — the NAS is
// often reached over a tailnet with no general internet egress assumption, and
// a CDN would also break the `default-src 'self'` CSP.
package web

import "embed"

// FS holds templates/ and static/. Parsed once at startup by internal/server,
// so a broken template fails the process rather than one request.
//
//go:embed templates static
var FS embed.FS

// HTMXVersion is the vendored htmx release, recorded here because a minified
// file carries no obvious version and the next person will want to know what to
// diff against when upgrading.
const HTMXVersion = "2.0.4"
