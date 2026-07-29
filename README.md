# Ambar

Self-hosted game asset library. Go + SQLite + htmx, single static binary, single
Docker container, built to run on a Synology NAS.

Indexes a large library of downloaded game assets — 2D sprites, 3D models,
audio — makes them searchable by tag, tracks provenance and licensing, and serves
a Godot editor plugin.

The full specification is in [docs/spec.md](docs/spec.md). Work is organised by
the milestones in §14.

## Status

**M0 complete.** What exists today:

- Configuration from the environment, with validation that fails at startup
- SQLite with WAL, a single writer connection and a separate read pool
- Embedded, forward-only migrations
- Authentication: argon2id passwords, server-side sessions, login rate limiting,
  CSRF tokens
- `ambar user add` / `ambar user list`
- `/healthz` (public liveness) and `/api/v1/healthz` (authenticated detail)
- Container image and compose file

Not yet: scanning, indexing, thumbnails, tags, search, viewers, the API, the
Godot plugin. M1 brings `ambar scan` and a grid view.

### FTS5

§15 item 1 is resolved. `modernc.org/sqlite` v1.55.0 (SQLite 3.53.3) ships FTS5
enabled, including the external-content form §4 specifies, `bm25()` ranking and
the `trigram` tokenizer — all with `CGO_ENABLED=0`, so the static binary and the
no-CGO invariant hold. No `mattn/go-sqlite3`, no CGO.

`internal/db/fts5_test.go` keeps those assertions permanently, because a
dependency bump that silently dropped FTS5 would break search with no other
signal.

## Building

Requires Go 1.26 or newer. No CGO, no C toolchain, no bundler.

```sh
make build      # -> dist/ambar
make test       # all packages
make test-race  # the rate limiter and session store are shared mutable state
make check      # gofmt + vet + test, what CI runs
make docker     # container image
```

`GO`, `GOFMT` and `DOCKER` are overridable if the toolchain is not on `PATH`:

```sh
make test GO="/usr/local/go/bin/go"
make docker DOCKER=podman
```

`make test-race` needs cgo and a C compiler, and overrides `CGO_ENABLED=0` for
that target alone. That does not weaken the no-CGO invariant: `make build` and
the Dockerfile both stay at `CGO_ENABLED=0`, and the Docker build fails outright
if the resulting binary is dynamically linked.

## Running locally

```sh
make run                          # against ./testdata, on :8080
make user-add USERNAME=yourname   # first run only; there is no default account
```

Then open <http://localhost:8080/>.

Configuration is entirely by environment variable. See
[.env.example](.env.example), which documents every one.

## Deploying

Copy `.env.example` to `.env`, adjust, and use
[docker-compose.yml](docker-compose.yml).

Three things that are easy to get wrong:

**Set `user:` to the uid/gid that owns the library.** Not optional. Files
extracted from `_inbox` by a root container are owned by root and cannot be
edited or deleted over SMB afterwards. Find the values with `ssh <user>@<nas> id`
— on Synology, human users typically start at uid 1026 and the `users` group is
gid 100. If the container cannot write to `/data` it refuses to start, which is
how a wrong value here surfaces immediately.

**Keep the data root on a real local volume, and outside the library tree.** WAL
corrupts on network filesystems, so never point `/data` at another machine's
SMB/NFS share. A Synology `/volume2/...` path is a local volume and is fine —
SMB is only how a desktop reaches it from outside. Keeping it a sibling of the
library also means `scan` never walks the database and a file-level library
backup never captures a live WAL database mid-write.

**Pick a host port that does not collide.** DSM occupies 5000/5001 and several
Synology packages take 8080. Check with `sudo netstat -tlnp`.

Then create the first user:

```sh
docker exec -it ambar /ambar user add <username>
```

Or non-interactively:

```sh
printf '%s' "$PASSWORD" | docker exec -i ambar /ambar user add <username> --password-stdin
```

### Running the image with rootless podman

Only relevant for local testing; the deployment target is Docker. Under rootless
podman, `--user` alone maps to a subuid that does not own the bind mount, so the
container correctly refuses to start. Add `--userns=keep-id`:

```sh
make docker-run DOCKER=podman ROOTLESS_FLAGS=--userns=keep-id
```

Docker passes host uids straight through and needs neither flag, which is why
`docker-compose.yml` only sets `user:`.

## Security notes

Authentication is required and is not deferrable. The daily access path is a
Tailscale tailnet, but the moment Tailscale Funnel or a Cloudflare Tunnel is
switched on, this application is on the public internet with no edge rate
limiting — see §11.

One deliberate deviation from the spec is worth knowing about. §11 mandates the
`Secure` attribute on the session cookie; a `Secure` cookie is never sent over
plain HTTP, and §8 documents plain-HTTP LAN access (`http://nas:8973`) as a real
path, so an unconditional flag would make LAN login impossible. `Secure` is
therefore derived from `AMBAR_BASE_URL`'s scheme, overridable with
`AMBAR_COOKIE_SECURE=auto|true|false`, and a warning is logged at startup when it
is off. **Set `AMBAR_COOKIE_SECURE=true` once access is HTTPS-only.**

## Licence

Not chosen yet. §17 frames it as MIT for maximum reuse versus AGPL-3.0 if hosted
commercial forks are unwelcome.
