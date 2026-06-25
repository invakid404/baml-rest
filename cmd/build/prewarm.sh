#!/usr/bin/env bash
# Prewarm script for BAML REST API Docker builds.
#
# Runs the source-INDEPENDENT portion of the build so that the expensive,
# per-batch-invariant work — Go module downloads, the npm BAML package
# download, and Go build-cache population — lands in a Docker layer built
# BEFORE the user's baml_src is copied in. That layer is keyed only on the
# stable baml_rest sources and the build matrix env (BAML_VERSION,
# ADAPTER_VERSION, TARGETARCH, build tags, baml-source mode), so it is reused
# across batches whose only difference is baml_src; build.sh then redoes just
# the source-dependent steps (client generation, introspection, final compile).
#
# This is best-effort cache warming: individual warming steps may fail without
# failing the image build (note the absence of `set -e`). Go's build/module
# caches and npm's cache are content-addressed, so a partial or skipped warm
# only costs build speed, never correctness — build.sh recomputes anything that
# is missing or stale. The cache layout and module/adapter preparation below
# mirror build.sh so the warmed entries are the ones build.sh actually reuses.
set -uo pipefail

# These are guaranteed by the Dockerfile env block; bail out cleanly (without
# failing the build) if a caller invokes prewarm without them.
if [ -z "${BAML_VERSION:-}" ] || [ -z "${ADAPTER_VERSION:-}" ] || [ -z "${USER_CONTEXT_PATH:-}" ]; then
    echo "prewarm: required build env not set, skipping cache warm"
    exit 0
fi

# --- Cache layout (identical to build.sh) ----------------------------------
CACHE_DIR="${CACHE_DIR:-/cache}"

# TARGETARCH is set by Docker during the build; normalize uname fallbacks the
# same way build.sh does so the per-arch cache paths line up.
TARGETARCH="${TARGETARCH:-$(uname -m)}"
case "${TARGETARCH}" in
    x86_64) TARGETARCH="amd64" ;;
    aarch64) TARGETARCH="arm64" ;;
esac

export NPM_CONFIG_CACHE="${CACHE_DIR}/npm/${TARGETARCH}"
export GOMODCACHE="${CACHE_DIR}/go/mod"
export GOCACHE="${CACHE_DIR}/go/build"
export BAML_CACHE_DIR="${CACHE_DIR}/baml-shared-lib/${BAML_VERSION}/${TARGETARCH}"

mkdir -p "${NPM_CONFIG_CACHE}" "${GOMODCACHE}" "${GOCACHE}" "${BAML_CACHE_DIR}"

# --- Go build tags (identical to build.sh) ---------------------------------
# Warmed GOCACHE entries are tag-sensitive, so compile the prewarm tree with the
# same tags the final build uses.
BUILD_TAGS=""
if [ "${DEBUG_BUILD:-false}" = "true" ]; then
    BUILD_TAGS="${BUILD_TAGS:+${BUILD_TAGS},}debug"
fi
if [ "${UNARY_SERVER:-false}" = "true" ]; then
    BUILD_TAGS="${BUILD_TAGS:+${BUILD_TAGS},}unaryserver"
fi
if [ "${SUBPROCESS:-true}" = "true" ]; then
    BUILD_TAGS="${BUILD_TAGS:+${BUILD_TAGS},}subprocess"
fi
GO_BUILD_TAGS=""
if [ -n "${BUILD_TAGS}" ]; then
    GO_BUILD_TAGS="-tags=${BUILD_TAGS}"
fi

echo "============================================"
echo "BAML REST API Prewarm"
echo "============================================"
echo "Target Architecture: ${TARGETARCH}"
echo "BAML Version: ${BAML_VERSION}"
echo "Adapter Version: ${ADAPTER_VERSION}"
echo "Cache Directory: ${CACHE_DIR}"
echo "Build Tags: ${BUILD_TAGS:-<none>}"
echo "============================================"

# --- npm/npx warm ----------------------------------------------------------
# Pre-fetch the BAML npm package so build.sh's `npx ... generate` hits the npm
# cache instead of the network. Skipped for baml-source builds, which use the
# locally built CLI (BAML_CLI_PATH) rather than npx.
if [ -z "${BAML_CLI_PATH:-}" ]; then
    if command -v npx &> /dev/null; then
        echo "Prewarming npm cache for @boundaryml/baml@${BAML_VERSION}..."
        npx --yes "@boundaryml/baml@${BAML_VERSION}" --version || \
            echo "prewarm: npm warm failed (continuing)"
    fi
fi

# --- Go module + build cache warm ------------------------------------------
# Work on a throwaway copy of baml_rest. build.sh re-copies the pristine tree
# into its own working directory, so the module-graph edits below never leak
# into the real build.
WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

if ! cp -r "${USER_CONTEXT_PATH}/baml_rest" "${WORK_DIR}/baml_rest"; then
    echo "prewarm: failed to stage baml_rest, skipping Go warm"
    exit 0
fi
cd "${WORK_DIR}/baml_rest" || exit 0

# Strip dynclient workspace/module references (mirrors build.sh). The embedded
# server-only bundle excludes the dynclient module, and `go work` validates all
# use entries before any command runs, so these refs must go before `go work
# sync` below. Guarded on the absence of dynclient/ so a dev checkout (where the
# module is present) is left untouched.
if [ ! -d dynclient ]; then
    echo "Stripping dynclient workspace/module references..."
    go work edit \
        -dropuse=./dynclient \
        -dropuse=./dynclient/baml-patched || true
    go mod edit \
        -droprequire github.com/invakid404/baml-rest/dynclient \
        -droprequire github.com/invakid404/baml-rest/dynclient/baml-patched \
        -dropreplace github.com/invakid404/baml-rest/dynclient \
        -dropreplace github.com/invakid404/baml-rest/dynclient/baml-patched || true
fi

# Prune non-selected adapters (keyed on ADAPTER_VERSION, not baml_src). Each
# adapter pins a different github.com/boundaryml/baml version; dropping the
# others lets Minimum Version Selection resolve baml to the cell's BAML_VERSION,
# so the warmed build cache matches the version the final build compiles
# against. Only the directories and root replace/require entries are touched —
# the per-adapter go.mod pins are never modified, preserving the pin matrix.
SELECTED_ADAPTER="$(basename "${ADAPTER_VERSION}")"
echo "Selected adapter: ${SELECTED_ADAPTER}"
for adapter_dir in adapters/adapter_v*/; do
    [ -d "${adapter_dir}" ] || continue
    adapter_name="$(basename "${adapter_dir}")"
    if [ "${adapter_name}" != "${SELECTED_ADAPTER}" ]; then
        echo "  Pruning ${adapter_name}..."
        rm -rf "${adapter_dir}"
        go mod edit \
            -droprequire "github.com/invakid404/baml-rest/adapters/${adapter_name}" \
            -dropreplace "github.com/invakid404/baml-rest/adapters/${adapter_name}" || true
    fi
done

# Regenerate embed.go so the root package embeds only the surviving adapter,
# matching build.sh (this also compiles cmd/embed and its deps into GOCACHE).
echo "Regenerating embed.go..."
go run cmd/embed/main.go || echo "prewarm: embed regen failed (continuing)"

# Pin the BAML Go dependency the same way build.sh does so the module graph
# resolves to the cell's version before downloading.
if [ -n "${CUSTOM_BAML_GO_LIB:-}" ]; then
    go work edit -replace "github.com/boundaryml/baml=${CUSTOM_BAML_GO_LIB}" || true
else
    echo "Fetching github.com/boundaryml/baml@${BAML_VERSION}..."
    go get "github.com/boundaryml/baml@${BAML_VERSION}" || \
        echo "prewarm: go get baml failed (continuing)"
fi

# Download the workspace module graph into GOMODCACHE.
echo "Syncing Go workspace (module download)..."
go work sync || echo "prewarm: go work sync failed (continuing)"

# Warm GOCACHE for the dependency tree. The committed introspected stub imports
# only bamlutils (no generated baml_client), so the server commands compile here
# without the per-batch client. When build.sh later regenerates the client and
# introspection, only the client-dependent leaves recompile — the deep
# dependency tree stays cached.
echo "Warming Go build cache..."
# shellcheck disable=SC2086
go build ${GO_BUILD_TAGS} \
    ./cmd/worker/ \
    ./cmd/serve/ \
    ./cmd/schema/ \
    ./cmd/introspect/ \
    ./cmd/hacks/ || echo "prewarm: build-cache warm incomplete (continuing)"

echo "============================================"
echo "Prewarm complete"
echo "============================================"
exit 0
