# Multi-stage build. §2: one container, target under 250 MB. This lands around
# 20 MB, because the binary is static and nothing else is needed.
#
# Nothing for Blender, Aseprite or ffmpeg is baked in (§6): Blender is
# downloaded at runtime into $DATA_ROOT/tools if wanted, and Aseprite's licence
# does not permit redistribution, so it must be bind-mounted.

# --- build ------------------------------------------------------------------

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies as their own layer, so editing source does not re-download the
# module cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

# CGO_ENABLED=0 is the whole reason modernc.org/sqlite is used
# (CLAUDE.md invariant 6). FTS5 availability under this configuration is
# asserted by internal/db/fts5_test.go.
RUN CGO_ENABLED=0 go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /ambar ./cmd/ambar

# Fail the build rather than shipping a dynamically linked binary, which would
# not run on a scratch or musl base and would mean CGO crept back in.
RUN ! ldd /ambar 2>/dev/null | grep -q "=>" || (echo "binary is dynamically linked; CGO_ENABLED=0 was not honoured" && exit 1)

# --- runtime ----------------------------------------------------------------

# alpine rather than scratch: §17 documents `docker exec ambar /ambar backup`,
# and having a shell for that and for diagnosing a mount problem is worth ~8 MB.
FROM alpine:3.22

# ca-certificates for the optional URL-fetch ingest path (§5) and the Blender
# download (§6); tzdata so log timestamps and acquired_at dates are readable in
# the operator's zone.
RUN apk add --no-cache ca-certificates tzdata

COPY --from=build /ambar /ambar

# A non-root default. §17 requires overriding this in compose with the real NAS
# uid/gid (`user: "1026:100"`), because files extracted from _inbox by a root
# container cannot be edited or deleted over SMB afterwards.
USER 65532:65532

# Both are bind mounts in the real deployment; declaring them documents intent
# and makes a `docker run` without mounts fail visibly rather than writing into
# the container's own filesystem.
ENV AMBAR_LIBRARY_ROOT=/library \
    AMBAR_DATA_ROOT=/data

EXPOSE 8080

# busybox wget, already present, so no curl layer. Hits the public liveness
# endpoint, which is deliberately unauthenticated for exactly this.
#
# Note: podman warns that HEALTHCHECK is unsupported for the OCI image format and
# ignores it. Docker, which is the deployment target (§17), honours it.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --quiet --spider --tries=1 http://127.0.0.1:8080/healthz || exit 1

# Explicit, so `docker run <image> user add alice` also works.
ENTRYPOINT ["/ambar"]
CMD ["serve"]
