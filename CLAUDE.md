# Ambar

Self-hosted game asset library. Go + SQLite + htmx, single static binary, single Docker
container, runs on a Synology NAS. Indexes a large library of downloaded game assets
(2D sprites, 3D models, audio), makes them searchable by tag, tracks provenance and
licensing, and serves a Godot editor plugin.

## The spec

The full specification lives at `docs/spec.md`. It is long and deliberately not imported
here.

**Before implementing anything, read the relevant sections of `docs/spec.md`.** Section
numbers are stable; work is organised by the milestones in §14.

## Non-negotiable invariants

Violating any of these is a bug, regardless of what else the code does correctly.

1. **Originals are never modified, moved, or renamed.** The only exception is a removal the
   user explicitly selected (§9.1). Sidecar `.ambar.json` files may be written beside
   originals; the asset files themselves are read-only.
2. **The filesystem is the source of truth. SQLite is a rebuildable index.** Never store
   metadata only in the database. `ambar rebuild-index` must reconstruct everything from the
   filesystem and sidecars, and it must actually work.
3. **The application never deletes anything on its own.** No scheduled cleanup, no
   "clean up my library" action, no pre-selected rows, no automatic trash purge, no deletion
   as a side effect of scan or ingest. It detects and reports; the human selects.
4. **The last remaining copy of any content hash can never be removed.** Enforce as a code
   invariant, not a UI flow.
5. **Anything referenced by `project_uses` is never a removal candidate.**
6. **No CGO.** The binary must be statically linkable. This is why `modernc.org/sqlite` is
   used. Do not introduce a dependency that breaks this without raising it first.
7. **Format variants are not duplicates.** The PNG, PSD, and ASEPRITE files of the same
   artwork are one logical asset (§5.1). Never surface them as redundant copies.
8. **No long-running HTTP handlers.** All ingest, scan, and derivative work goes through the
   job queue with pollable status.
9. **Path traversal defence on every filesystem operation**, ingest and serving alike. Never
   build a path from user input without resolving it and confirming it stays under the
   configured root.
10. **Project identity is a UUID, never a filesystem path.** The Godot project is checked out
    at different paths on different machines.

## Stack constraints

- Go, stdlib `net/http` with Go 1.22+ pattern routing. Add `chi` only if it earns its place.
- SQLite via `modernc.org/sqlite`. WAL, `busy_timeout=5000`, `foreign_keys=ON`. Single
  writer connection, separate read pool.
- Server-rendered HTML + htmx. No SPA, no bundler for the main UI. JS only as isolated
  islands: 3D viewer, canvas zoom, audio waveform.
- Pure-Go decoders wherever possible. External binaries (Blender, Aseprite, ffmpeg) are
  optional, configured by path, never baked into the image.
- One container. Target under 250 MB.

## Working agreement

- **One milestone at a time.** Do not start the next milestone without being asked.
- **Plan before implementing.** For anything touching more than two files, present the plan
  and wait for approval.
- **Raise disagreements explicitly.** If the spec seems wrong or a constraint is blocking,
  say so rather than quietly deviating. The spec is a decision record, not scripture — but
  deviations must be surfaced, not discovered later.
- **Do not scope-creep.** Features not in the current milestone do not get "while I'm here"
  implementations, even small ones.
- Prefer boring, readable Go over clever Go. This codebase will be read six months from now
  by someone who has forgotten it.
- Tests are required for: the search query parser, path sanitisation, archive extraction
  against malicious input, spritesheet grid detection, scan reconciliation,
  `rebuild-index` fidelity, and every deletion invariant above. Table-driven, with real
  fixture files including deliberately broken ones.

## Commands

<!-- Fill these in as they come into existence; keep them accurate. -->

```
make build          # build the binary
make test           # run all tests
make run            # run locally against ./testdata
make docker         # build the container image
```

## Environment

Target deployment is documented in §17 of the spec. Library at `/volume2/game/assets`,
data at `/volume2/game/ambar`, x86_64, DSM 7. Never hardcode these — they are configuration.
