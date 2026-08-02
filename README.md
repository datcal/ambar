# Ambar

A self-hosted library for the game assets you have already downloaded.

If you buy or grab free asset packs, you end up with a folder of forty zips, three
copies of the same tileset, and no idea which pack the sprite you liked came from or
what its licence was. Ambar indexes that folder in place, makes it searchable, keeps
track of where each pack came from, and lets you pull an asset straight into a Godot
project — recording which project uses what, so it can generate the credits file for
you.

One Go binary, one SQLite file, one container. Runs on a NAS.

- **Your files are never touched.** Ambar reads the library and writes derivatives
  elsewhere. It has no scheduled cleanup, no "tidy my library" button, and it never
  deletes anything you did not select yourself.
- **The filesystem stays the source of truth.** The database is an index you can throw
  away and rebuild (`ambar rebuild-index`).
- **No CGO, no bundler, no Node.** Server-rendered HTML with htmx; the image is ~25 MB.

---

## How it fits together

```
   your desktop                        the NAS (or any Linux box)
 ┌─────────────────┐                 ┌──────────────────────────────────────┐
 │  browser        │─── HTTP ───────▶│  ambar (one container, port 8080)    │
 │                 │                 │                                      │
 │  Godot editor   │                 │  ┌────────────────────────────────┐  │
 │   └ addons/     │─── HTTP ───────▶│  │ HTTP: pages, JSON API, files   │  │
 │      ambar      │    bearer token │  │ job queue: scan, thumbnails    │  │
 │                 │                 │  │ index: SQLite + FTS5           │  │
 │  Finder/SMB ────┼──┐              │  └────────────────────────────────┘  │
 └─────────────────┘  │              │        │                  │          │
                      │              │        ▼                  ▼          │
                      │              │  /library (mount)   /data (volume)   │
                      └──── drop ────┼─▶ _inbox/            ambar.db        │
                          a zip      │   2d/ 3d/ audio/     derivatives/    │
                                     │   packs…             trash/ backups/ │
                                     └──────────────────────────────────────┘
```

Three ways in, one library:

| From | Path | What it does |
| --- | --- | --- |
| Browser | `http://nas:8973/` | Search, view, tag, ingest, review duplicates |
| Godot | `addons/ambar` → `/api/v1` | Browse and import into a project; records the use |
| SMB / Finder | drop a zip in `_inbox/` | Ambar extracts, classifies and indexes it |

The application knows nothing about how it is reached. It binds plain HTTP and leaves
remote access to something that does it properly — a Tailscale tailnet, a Cloudflare
tunnel, a reverse proxy. **There is no anonymous access:** the web UI needs a login and
the API needs a bearer token, because the moment one of those tunnels is switched on
the thing is on the public internet.

[ARCHITECTURE.md](ARCHITECTURE.md) has the detailed version — components, data flow,
and the rules the code is not allowed to break.

## How this one is used

The instance this was built for runs on a Synology DSM 7 box:

- the library is a share the desktops already mount over SMB — about 6,500 files of
  CraftPix and Kenney packs, sprites, tilesets and glTF models
- Ambar runs as one container beside it, with the database on a *local* volume
  (never on a network share — WAL corrupts there)
- access is over Tailscale, so the port is never exposed
- a Godot project on a laptop imports from it and commits `.ambar/manifest.json`, so
  everyone's editor knows what is already in the project

None of that is required. It is a single container with two volumes; a Raspberry Pi
with a USB disk works, and so does `make run` on your laptop against a folder of
sprites.

## Quick start

### With Docker

```sh
git clone https://github.com/datcal/ambar.git && cd ambar
cp .env.example .env          # then edit the two paths and the port
docker compose up -d
docker exec -it ambar /ambar user add yourname
```

Open `http://localhost:8973/`, log in, and press **Re-scan library**.

Two things worth getting right before the first run — both are in `.env.example` with
the reasoning:

- **`user:` in `docker-compose.yml` must be the uid/gid that owns the library.** A
  container running as root extracts files you then cannot edit over SMB. `ssh nas id`
  tells you the numbers.
- **Keep `/data` on a local disk and outside the library folder.** SQLite's WAL is not
  safe on SMB or NFS, and keeping it a sibling means a scan never walks its own
  database.

### Without Docker

Go 1.26 or newer, nothing else:

```sh
make build
AMBAR_LIBRARY_ROOT=/path/to/assets AMBAR_DATA_ROOT=/path/to/data ./dist/ambar user add you
AMBAR_LIBRARY_ROOT=/path/to/assets AMBAR_DATA_ROOT=/path/to/data ./dist/ambar serve
```

Or `make run`, which serves `./testdata/library` on `:8973` with a throwaway database.

## The Godot plugin

Download `ambar-godot-plugin-<version>.zip` from
[Releases](https://github.com/datcal/ambar/releases) and unzip it at the root of your
Godot project, so you have `res://addons/ambar/plugin.cfg`. Then **Project → Project
Settings → Plugins → Ambar → enable**.

An **Ambar** tab appears beside *2D*, *3D* and *Script*. It opens on a setup panel:
paste the server address and an API token (**Settings → API tokens** in the web UI —
tick **write**, the plugin needs it to record imports) and press **Save and test**.

What it does:

- **Browse** the whole library as thumbnails, with the same search language as the web
  UI, nine sort orders and numbered pages
- **Inspect before importing** — full-size preview, dimensions, frame count, triangle
  count, tags, licence, and the other formats of the same artwork
- **Render models nobody has looked at yet.** The server has no renderer and Blender is
  optional, so most models have no thumbnail. The plugin is inside a game engine: it
  draws them itself and posts the result back, so the web UI fills in too
- **Import** into `res://assets/<kind>/<pack>/<file>`, verify the bytes against the hash
  the server advertised, and record it in `res://.ambar/manifest.json` (commit that
  file) and on the server
- **"In this project"** — everything imported, with what is stale, what the server was
  never told about, and what is missing from the checkout; plus **Write CREDITS.md**

Requires Godot 4. Verified on 4.7.

## What it does with a library

- **Scans in place.** Depth-agnostic pack detection, junk (`__MACOSX`, `.DS_Store`)
  ignored, format variants collapsed — the PNG, PSD and ASEPRITE of one sprite are one
  tile, not three.
- **Search** by filename, pack, tag, kind, dimensions, colour, triangle count:
  `sword type:model -style:realistic 32x32`, `color:#8b3a3a~20`.
- **Previews for everything it can:** pixel-art-safe thumbnails (nearest-neighbour,
  never smoothed), an `.aseprite` decoder that reads the binary format directly, audio
  waveforms, font specimens, spritesheet playback with frame detection, and a 3D viewer.
- **Provenance and licences** per pack, and a generated `CREDITS.md` per Godot project
  from what that project actually uses.
- **Duplicates, carefully.** Exact-hash duplicates, packs that contain other packs, and
  near-identical images — all reported, never acted on. Removals move to a trash folder
  with a record of where they came from, an asset a project uses cannot be removed, and
  the last copy of any content can never be removed.
- **Ingest**: drop an archive in `_inbox/`, or upload one, and it is extracted (with
  path-traversal defences), classified and indexed.

Measured on 20,000 assets: first scan 2.9 s, rescan 157 ms when nothing changed, and
paging stays flat — page 200 costs what page 1 costs.

## Configuration

Everything is an environment variable, and [`.env.example`](.env.example) documents
every one with its default. The ones you will actually set:

| Variable | What for |
| --- | --- |
| `AMBAR_LIBRARY_ROOT` | the folder to index |
| `AMBAR_DATA_ROOT` | database, derivatives, trash, backups |
| `AMBAR_BASE_URL` | for links, and it decides the session cookie's `Secure` flag |
| `AMBAR_LIBRARY_BUCKETS` | top folders that group packs rather than being packs |
| `AMBAR_IGNORE_GLOBS` | junk to skip |
| `AMBAR_LOCAL_LIBRARY_PATH` | the library as *your desktop* sees it, for "open in…" |

## Command line

```
ambar serve                     run the server
ambar scan [--dry-run]          index the library
ambar derive                    generate any missing thumbnails and previews
ambar ingest <archive>          extract an archive into the library
ambar verify                    re-hash files to detect bit rot
ambar rebuild-index             drop the database and rebuild it from the filesystem
ambar backup                    VACUUM INTO a timestamped copy
ambar dupes / junk              report duplicates and clutter (reporting only)
ambar trash list|restore|purge  inspect the trash, put files back
ambar user add|list             accounts; there is no self-registration
```

## Development

```sh
make check        # gofmt + vet + test — what CI runs
make test-race    # needs cgo and a C compiler
make run          # serve ./testdata on :8973
make docker       # build the image
make plugin-zip   # dist/ambar-godot-plugin-<version>.zip
```

The Godot plugin has its own suite, because the Go tests cannot reach GDScript and a
parse error there leaves an addon that is enabled and silently does nothing:

```sh
make godot-test GODOT=/path/to/godot            # parse check, API, import, project screen
make godot-test GODOT=… ARGS=ui                 # plus model rendering and a screenshot
```

See [godot-test/README.md](godot-test/README.md).

## Releasing

Push a tag. [`.github/workflows/release.yml`](.github/workflows/release.yml) runs the
tests, publishes `ghcr.io/datcal/ambar:<version>`, builds the Linux binaries and the
plugin zip, and creates the GitHub release:

```sh
git tag -a v1.0.0 -m "…" && git push origin v1.0.0
```

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md) — how the pieces fit, and the rules that hold
- [CHANGELOG.md](CHANGELOG.md) — what changed, per version
- [docs/spec.md](docs/spec.md) — the original design specification, kept as the detailed
  reference for behaviour and the reasoning behind it

## Licence

[MIT](LICENSE).
