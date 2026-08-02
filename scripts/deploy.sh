#!/usr/bin/env bash
# Deploy Ambar to the NAS.
#
# Build here, ship the image over SSH, copy the two files the server needs, and
# restart the container. No registry is involved: there is one NAS, the image is
# about 25 MB, and `docker save | ssh docker load` needs no credentials and no
# account anywhere.
#
#   scripts/deploy.sh                  build, ship, restart
#   scripts/deploy.sh --no-build       ship the image already built here
#   scripts/deploy.sh --config-only    copy .env and docker-compose.yml, restart
#   scripts/deploy.sh --push           build, push to the registry, NAS pulls
#
# Two ways for the image to reach the NAS, chosen by what AMBAR_IMAGE names:
#
#   a local tag (ambar:local)          `save | ssh docker load` — no registry, no
#                                      credentials, and over a LAN it is the fast one
#   a registry ref (ghcr.io/…/ambar)   pushed here, pulled there. Needs a login on
#                                      this machine, and on the NAS too unless the
#                                      package is public. Use it when you want a
#                                      versioned artifact rather than a copy.
#
# Neither is more "correct": the first is for iterating, the second for releases. A
# tag pushed to GitHub builds and publishes the same image without either login —
# see .github/workflows/release.yml.
#
# Configuration comes from .env, which stays local — the repository carries no real
# hostnames, usernames or paths (.gitignore, §17). The variables this script needs
# there, or in the environment:
#
#   AMBAR_SSH_HOST      user@host of the NAS
#   AMBAR_SSH_KEY       private key to use (optional; ssh defaults otherwise)
#   AMBAR_REMOTE_DIR    where docker-compose.yml and .env live on the NAS
#   AMBAR_BUILDER       docker or podman; auto-detected otherwise
#
# What this script deliberately does not do: create a user. §11 has no
# self-registration and no default account, so the first account is created by
# hand and the script prints the command.

set -euo pipefail

cd "$(dirname "$0")/.."

# --- configuration -----------------------------------------------------------

if [ ! -f .env ]; then
    echo "no .env — copy .env.example and fill in the real paths first" >&2
    exit 1
fi

# Read what the script itself needs out of .env, without sourcing the whole file
# (it is a compose env file, not a shell script: no quoting guarantees).
envval() {
    sed -n "s/^$1=//p" .env | tail -1
}

# The environment wins over .env, so a one-off deploy elsewhere needs no edit.
SSH_HOST="${AMBAR_SSH_HOST:-$(envval AMBAR_SSH_HOST)}"
SSH_KEY="${AMBAR_SSH_KEY:-$(envval AMBAR_SSH_KEY)}"
REMOTE_DIR="${AMBAR_REMOTE_DIR:-$(envval AMBAR_REMOTE_DIR)}"

if [ -z "$SSH_HOST" ]; then
    echo "set AMBAR_SSH_HOST (in .env or the environment) to user@host of the NAS" >&2
    exit 1
fi
REMOTE_DIR="${REMOTE_DIR:-/volume2/game/ambar-stack}"

IMAGE="$(envval AMBAR_IMAGE)"
IMAGE="${IMAGE:-ambar:local}"
HOST_PORT="$(envval AMBAR_HOST_PORT)"
HOST_PORT="${HOST_PORT:-8973}"
HOST_DATA="$(envval AMBAR_HOST_DATA)"
HOST_LIBRARY="$(envval AMBAR_HOST_LIBRARY)"

# Where --push publishes to, without a tag: the script adds :<version> and :latest.
# Kept separate from AMBAR_IMAGE, because "what to publish" and "what the NAS runs"
# are different decisions — the NAS can keep running a locally shipped image while a
# release is published, and switch when you choose to.
PUSH_IMAGE="${AMBAR_PUSH_IMAGE:-$(envval AMBAR_PUSH_IMAGE)}"

# The builder is whatever is installed. podman writes a docker-archive by default,
# which `docker load` on the far end reads without complaint — so the two are
# interchangeable here.
BUILDER="${AMBAR_BUILDER:-}"
if [ -z "$BUILDER" ]; then
    if command -v docker >/dev/null 2>&1; then
        BUILDER=docker
    elif command -v podman >/dev/null 2>&1; then
        BUILDER=podman
    else
        echo "no docker and no podman on this machine; set AMBAR_BUILDER" >&2
        exit 1
    fi
fi

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"

DO_BUILD=1
DO_IMAGE=1
FORCE_PUSH=0
case "${1:-}" in
    --no-build)    DO_BUILD=0 ;;
    --config-only) DO_BUILD=0; DO_IMAGE=0 ;;
    --push)        FORCE_PUSH=1 ;;
    "")            ;;
    *)             echo "unknown option: $1" >&2; exit 2 ;;
esac

# Does AMBAR_IMAGE name a registry? Only a reference *with a slash* can, and then only
# if its first segment looks like a host — a dot, a port, or literally "localhost".
# This is the rule the container tools themselves use, and getting it wrong the other
# way is easy: `ambar:local` has no slash, so the whole string would look like a host
# with a port if the slash were not checked first.
case "$IMAGE" in
    */*)
        case "${IMAGE%%/*}" in
            *.*|*:*|localhost) IMAGE_IS_REMOTE=1 ;;
            *)                 IMAGE_IS_REMOTE=0 ;;
        esac
        ;;
    *) IMAGE_IS_REMOTE=0 ;;
esac
if [ "$FORCE_PUSH" = 1 ] && [ -z "$PUSH_IMAGE" ]; then
    echo "--push needs AMBAR_PUSH_IMAGE, e.g. ghcr.io/<owner>/ambar" >&2
    exit 2
fi

# -i only when a key is configured; otherwise ssh's own config decides.
ssh_opts=(-o BatchMode=yes)
[ -n "$SSH_KEY" ] && ssh_opts+=(-i "$SSH_KEY")

ssh_nas() { ssh "${ssh_opts[@]}" "$SSH_HOST" "$@"; }

step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

# --- checks ------------------------------------------------------------------

step "checking the NAS"
ssh_nas "docker version --format 'docker {{.Server.Version}}' && docker compose version" \
    | sed 's/^/   /'

# The container runs as this uid:gid and must be able to write to both mounts. A
# wrong value here is the classic Synology deployment failure, and it is much
# cheaper to catch now than after the first ingest leaves root-owned files.
ssh_nas "test -d '$HOST_LIBRARY'" ||
    { echo "the library directory $HOST_LIBRARY does not exist on the NAS" >&2; exit 1; }
ssh_nas "mkdir -p '$HOST_DATA' && test -w '$HOST_DATA'" ||
    { echo "$HOST_DATA is not writable by $SSH_HOST" >&2; exit 1; }

# --- build -------------------------------------------------------------------

if [ "$DO_BUILD" = 1 ]; then
    step "building $IMAGE with $BUILDER ($VERSION, $COMMIT)"
    # --platform pins the runtime image to what the NAS actually runs (§17,
    # amd64). The Dockerfile cross-compiles the Go binary from $BUILDPLATFORM,
    # so this stays fast on an arm64 developer machine.
    "$BUILDER" build \
        --platform=linux/amd64 \
        --build-arg "VERSION=$VERSION" \
        --build-arg "COMMIT=$COMMIT" \
        -t "$IMAGE" .
    "$BUILDER" images --format '   {{.Repository}}:{{.Tag}}  {{.Size}}' 2>/dev/null \
        | grep -F "${IMAGE%%:*}" | head -3 || true
fi

# --- ship the image ----------------------------------------------------------

if [ "$FORCE_PUSH" = 1 ]; then
    step "publishing $PUSH_IMAGE"

    # A token in the environment or in .env logs in for this run; otherwise an
    # existing login is used, and a missing one fails with the registry's own message
    # rather than something invented here.
    token="${GHCR_TOKEN:-${CR_PAT:-${GITHUB_TOKEN:-$(envval GITHUB_TOKEN)}}}"
    registry="${PUSH_IMAGE%%/*}"
    owner="$(echo "$PUSH_IMAGE" | cut -d/ -f2)"
    if [ -n "$token" ]; then
        printf '%s' "$token" |
            "$BUILDER" login "$registry" -u "${GHCR_USER:-$owner}" --password-stdin >/dev/null
        echo "   logged in to $registry as ${GHCR_USER:-$owner}"
    fi

    # Two tags: the version, so a running container can be traced back to a commit,
    # and :latest, so a deployment that does not care always gets the newest.
    for tag in "$VERSION" latest; do
        "$BUILDER" tag "$IMAGE" "$PUSH_IMAGE:$tag"
        "$BUILDER" push "$PUSH_IMAGE:$tag" 2>&1 | tail -1 | sed 's/^/   /'
        echo "   pushed $PUSH_IMAGE:$tag"
    done

elif [ "$DO_IMAGE" = 1 ] && [ "$IMAGE_IS_REMOTE" = 1 ]; then
    step "pulling $IMAGE on the NAS (already published)"

elif [ "$DO_IMAGE" = 1 ]; then
    step "shipping the image to $SSH_HOST"
    # gzip because the wire is slower than the CPU on both ends, and a static Go
    # binary compresses well.
    #
    # podman tags images `localhost/name:tag`, docker tags them `name:tag`, and
    # whichever name the archive carries is the name `docker load` registers. The
    # compose file asks for $IMAGE, so the loaded name is normalised to it rather
    # than leaving the two to disagree.
    loaded=$("$BUILDER" save "$IMAGE" | gzip -1 |
        ssh_nas 'gunzip | docker load' | sed -n 's/^Loaded image: //p' | tail -1)
    echo "   loaded $loaded"
    if [ -n "$loaded" ] && [ "$loaded" != "$IMAGE" ]; then
        ssh_nas "docker tag '$loaded' '$IMAGE'"
        echo "   tagged as $IMAGE"
    fi
fi

# --- ship the configuration --------------------------------------------------

step "copying docker-compose.yml and .env to $REMOTE_DIR"
ssh_nas "mkdir -p '$REMOTE_DIR'"
# The local .env holds deploy-time credentials and SSH details that the server has
# no use for — and compose passes every line of the file it finds into the container
# as an environment variable. So the copy is filtered rather than verbatim: a registry
# token has no business sitting on the NAS or inside the running process.
env_for_server=$(mktemp)
trap 'rm -f "$env_for_server"' EXIT
grep -vE '^(GITHUB_TOKEN|GHCR_TOKEN|GHCR_USER|CR_PAT|AMBAR_SSH_[A-Z_]*|AMBAR_REMOTE_DIR|AMBAR_PUSH_IMAGE|AMBAR_BUILDER)=' \
    .env > "$env_for_server"
stripped=$(( $(grep -cE '^[A-Z]' .env) - $(grep -cE '^[A-Z]' "$env_for_server") ))
[ "$stripped" -gt 0 ] && echo "   withheld $stripped local-only setting(s) from the copy"

# scp rather than rsync: DSM does not always ship rsync, and two files do not
# need it. The session secret is not in here either — the app generates one into
# /data on first run.
#
# -O forces the legacy SCP protocol. Modern OpenSSH prefers SFTP, and DSM's SFTP
# subsystem does not resolve a path like /volume2/game/... — it answers "No such
# file or directory" for a directory that plainly exists over the shell.
scp -O -q "${ssh_opts[@]}" docker-compose.yml "$SSH_HOST:$REMOTE_DIR/"
scp -O -q "${ssh_opts[@]}" "$env_for_server" "$SSH_HOST:$REMOTE_DIR/.env"
ssh_nas "chmod 600 '$REMOTE_DIR/.env'"
echo "   done"

# --- restart -----------------------------------------------------------------

step "starting the container"
if [ "$IMAGE_IS_REMOTE" = 1 ]; then
    # A private package needs `docker login ghcr.io` on the NAS once; the pull's own
    # error says so clearly enough that repeating it here would only add noise.
    ssh_nas "cd '$REMOTE_DIR' && docker compose pull" 2>&1 | sed 's/^/   /'
fi
ssh_nas "cd '$REMOTE_DIR' && docker compose up -d" 2>&1 | sed 's/^/   /'

step "status"
ssh_nas "docker ps --filter name=ambar --format '   {{.Names}} | {{.Image}} | {{.Status}} | {{.Ports}}'"

# A container that starts and then dies is the usual symptom of a uid/gid or mount
# problem, so the health endpoint is checked rather than assumed.
step "health"
sleep 3
if ssh_nas "curl -fsS http://127.0.0.1:$HOST_PORT/healthz" 2>/dev/null | sed 's/^/   /'; then
    :
else
    echo "   /healthz did not answer — last log lines:" >&2
    ssh_nas "docker logs --tail 30 ambar" 2>&1 | sed 's/^/   /' >&2
    exit 1
fi

cat <<EOF

Deployed. http://$(echo "$SSH_HOST" | cut -d@ -f2):$HOST_PORT/

First run only — there is no self-registration and no default account (§11):

    ssh $SSH_HOST "docker exec -it ambar /ambar user add <username>"

Then index the library (or press "Re-scan library" in the UI):

    ssh $SSH_HOST "docker exec ambar /ambar scan"
    ssh $SSH_HOST "docker exec ambar /ambar derive"
EOF
