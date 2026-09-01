#!/usr/bin/env bash
set -euo pipefail

# Unified Build Script for BAML REST API
# This script performs the complete build process: Node.js client generation + Go compilation

# Required environment variables:
# - BAML_VERSION: Version of BAML to use (e.g., "0.204.0")
# - ADAPTER_VERSION: Adapter version to use (e.g., "v0.204.0")
# - USER_CONTEXT_PATH: Path to the user's context directory containing baml_src
#
# Optional environment variables:
# - OUTPUT_PATH: Where to place the final binary (default: /output/baml-rest)
# - CACHE_DIR: Root cache directory (default: /cache)
# - BAML_CACHE_DIR: Final BAML cache directory (default: /baml-cache)

# Validate required environment variables
if [ -z "${BAML_VERSION:-}" ]; then
    echo "ERROR: BAML_VERSION environment variable is required"
    exit 1
fi

if [ -z "${ADAPTER_VERSION:-}" ]; then
    echo "ERROR: ADAPTER_VERSION environment variable is required"
    exit 1
fi

if [ -z "${USER_CONTEXT_PATH:-}" ]; then
    echo "ERROR: USER_CONTEXT_PATH environment variable is required"
    exit 1
fi

# Set defaults for optional variables
OUTPUT_PATH="${OUTPUT_PATH:-/output/baml-rest}"
CACHE_DIR="${CACHE_DIR:-/cache}"
BAML_CACHE_DIR="${BAML_CACHE_DIR:-/baml-cache}"

# Save the final BAML cache destination
BAML_CACHE_FINAL="${BAML_CACHE_DIR}"

# Get target architecture for platform-specific caching
# TARGETARCH is set by Docker during multi-arch builds (e.g., amd64, arm64)
TARGETARCH="${TARGETARCH:-$(uname -m)}"
# Normalize architecture names
case "${TARGETARCH}" in
    x86_64) TARGETARCH="amd64" ;;
    aarch64) TARGETARCH="arm64" ;;
esac

# Use a version and architecture-specific location in the cache mount for BAML downloads during build
# This ensures the shared library is cached across builds and avoids version/architecture conflicts
BAML_CACHE_BUILD="${CACHE_DIR}/baml-shared-lib/${BAML_VERSION}/${TARGETARCH}"

# Configure unified caching (npm cache is architecture-specific due to native bindings)
export NPM_CONFIG_CACHE="${CACHE_DIR}/npm/${TARGETARCH}"
export GOMODCACHE="${CACHE_DIR}/go/mod"
export GOCACHE="${CACHE_DIR}/go/build"
export BAML_CACHE_DIR="${BAML_CACHE_BUILD}"

# Create cache directories if they don't exist
mkdir -p "${NPM_CONFIG_CACHE}"
mkdir -p "${GOMODCACHE}"
mkdir -p "${GOCACHE}"
mkdir -p "${BAML_CACHE_DIR}"

# Handle custom BAML lib if provided
CUSTOM_BAML_LIB_PATH="${USER_CONTEXT_PATH}/custom_baml_lib.so"
if [ -f "${CUSTOM_BAML_LIB_PATH}" ]; then
    echo ""
    echo "=== Custom BAML Library Detected ==="

    # Determine the correct filename based on architecture
    case "${TARGETARCH}" in
        amd64)
            BAML_LIB_FILENAME="libbaml_cffi-x86_64-unknown-linux-gnu.so"
            ;;
        arm64)
            BAML_LIB_FILENAME="libbaml_cffi-aarch64-unknown-linux-gnu.so"
            ;;
        *)
            echo "ERROR: Custom BAML lib not supported for architecture: ${TARGETARCH}"
            exit 1
            ;;
    esac

    echo "Installing custom BAML lib as: ${BAML_LIB_FILENAME}"
    cp "${CUSTOM_BAML_LIB_PATH}" "${BAML_CACHE_DIR}/${BAML_LIB_FILENAME}"
    echo "Custom BAML lib installed to: ${BAML_CACHE_DIR}/${BAML_LIB_FILENAME}"
    echo ""
fi

# Handle custom BAML Go library if provided (check for .provided marker)
CUSTOM_BAML_GO_LIB_PATH="${USER_CONTEXT_PATH}/custom_baml_go_lib"
if [ -f "${CUSTOM_BAML_GO_LIB_PATH}/.provided" ]; then
    echo ""
    echo "=== Custom BAML Go Library Detected ==="
    echo "Path: ${CUSTOM_BAML_GO_LIB_PATH}"
    export CUSTOM_BAML_GO_LIB="${CUSTOM_BAML_GO_LIB_PATH}"
    echo ""
fi

# Handle BAML library path (from --baml-source builds)
if [ -n "${BAML_LIBRARY_PATH:-}" ]; then
    echo ""
    echo "=== Local BAML Library Detected ==="
    echo "Path: ${BAML_LIBRARY_PATH}"

    # Copy to BAML cache with platform-specific filename for runtime discovery
    case "$(uname -s)" in
        Linux)
            case "${TARGETARCH}" in
                amd64) BAML_LIB_FILENAME="libbaml_cffi-x86_64-unknown-linux-gnu.so" ;;
                arm64) BAML_LIB_FILENAME="libbaml_cffi-aarch64-unknown-linux-gnu.so" ;;
            esac
            ;;
        Darwin)
            case "${TARGETARCH}" in
                amd64) BAML_LIB_FILENAME="libbaml_cffi-x86_64-apple-darwin.dylib" ;;
                arm64) BAML_LIB_FILENAME="libbaml_cffi-aarch64-apple-darwin.dylib" ;;
            esac
            ;;
    esac

    if [ -n "${BAML_LIB_FILENAME:-}" ]; then
        echo "Installing to cache as: ${BAML_LIB_FILENAME}"
        cp "${BAML_LIBRARY_PATH}" "${BAML_CACHE_DIR}/${BAML_LIB_FILENAME}"
    fi
    echo ""
fi

echo "============================================"
echo "BAML REST API Build Script"
echo "============================================"
echo "Target Architecture: ${TARGETARCH}"
echo "BAML Version: ${BAML_VERSION}"
echo "Adapter Version: ${ADAPTER_VERSION}"
echo "User Context: ${USER_CONTEXT_PATH}"
echo "Output Path: ${OUTPUT_PATH}"
echo "Cache Directory: ${CACHE_DIR}"
echo "NPM Cache: ${NPM_CONFIG_CACHE}"
echo "BAML Cache (build): ${BAML_CACHE_BUILD}"
echo "BAML Cache (final): ${BAML_CACHE_FINAL}"
if [ -n "${BAML_LIB_FILENAME:-}" ]; then
    echo "Custom BAML Lib: ${BAML_LIB_FILENAME}"
fi
if [ -n "${CUSTOM_BAML_GO_LIB:-}" ]; then
    echo "Custom BAML Go Lib: ${CUSTOM_BAML_GO_LIB}"
fi
if [ -n "${BAML_LIBRARY_PATH:-}" ]; then
    echo "BAML Library Path: ${BAML_LIBRARY_PATH}"
fi
if [ -n "${BAML_CLI_PATH:-}" ]; then
    echo "BAML CLI Path: ${BAML_CLI_PATH}"
fi
if [ "${DEBUG_BUILD:-false}" = "true" ]; then
    echo "Debug Build: enabled"
fi
if [ "${UNARY_SERVER:-false}" = "true" ]; then
    echo "Unary Server: enabled"
fi
if [ "${SUBPROCESS:-true}" = "true" ]; then
    echo "Subprocess Build: enabled"
else
    echo "In-process Build: enabled"
fi
# ============================================================================
# de-BAML serving cutover S2 — ARTIFACT PROFILE RESOLUTION
# ============================================================================
#
# S2 makes the native-capable worker (built from the isolated nanollmprepare
# module) the STANDARD deployable artifact: for a SUBPROCESS build, NATIVE_WORKER
# now defaults to TRUE. The zero-options BAML-only root cmd/worker stays fully
# buildable as the explicit ROLLBACK artifact, selected with NATIVE_WORKER=false.
#
# This changes which artifact SHIPS, not what it serves. The S1 cohort policy is
# empty, so the standard artifact declines every request pre-socket and BAML
# serves 100% of traffic on it; BAML_REST_USE_DEBAML=false additionally makes it
# do no native work at all.
#
# The request is read from NATIVE_WORKER before the default is applied, because
# "unset" and "explicitly false" must stay distinguishable: an in-process build
# that never asked for a native worker must not be rejected merely because the
# default flipped (that would hard-break every existing SUBPROCESS=false
# invocation), while one that DID ask is still a contradiction and is rejected.
NATIVE_WORKER_REQUEST="${NATIVE_WORKER:-}"
SHADOW_WORKER="${SHADOW_WORKER:-false}"

# Strict decoding: these select what ships, so a typo must fail the build rather
# than fall through to a falsy default and silently produce the other artifact.
case "${NATIVE_WORKER_REQUEST}" in
    ""|true|false) ;;
    *)
        echo "ERROR: NATIVE_WORKER must be exactly \"true\" or \"false\" (got \"${NATIVE_WORKER_REQUEST}\")" >&2
        exit 1
        ;;
esac
case "${SHADOW_WORKER}" in
    true|false) ;;
    *)
        echo "ERROR: SHADOW_WORKER must be exactly \"true\" or \"false\" (got \"${SHADOW_WORKER}\")" >&2
        exit 1
        ;;
esac

# ExecBridge-U1b: the NATIVE-ONLY worker selection. It is a single all-on/all-off
# gate: on, it builds cmd/worker-nativeonly (a subprocess worker with ZERO
# BAML/CFFI in its runtime graph) instead of the standard cmd/worker; off, nothing
# below changes. It is SUBPROCESS-only and native_capable, so it is mutually
# exclusive with the SHADOW profile and with an in-process build, and it cannot be
# combined with an explicit NATIVE_WORKER=false.
NATIVE_ONLY_WORKER="${NATIVE_ONLY_WORKER:-false}"
case "${NATIVE_ONLY_WORKER}" in
    true|false) ;;
    *)
        echo "ERROR: NATIVE_ONLY_WORKER must be exactly \"true\" or \"false\" (got \"${NATIVE_ONLY_WORKER}\")" >&2
        exit 1
        ;;
esac
if [ "${NATIVE_ONLY_WORKER}" = "true" ]; then
    if [ "${SHADOW_WORKER}" = "true" ]; then
        echo "ERROR: NATIVE_ONLY_WORKER=true is mutually exclusive with SHADOW_WORKER=true" >&2
        exit 1
    fi
    if [ "${SUBPROCESS:-true}" != "true" ]; then
        echo "ERROR: NATIVE_ONLY_WORKER=true requires SUBPROCESS=true — the native-only worker is a subprocess artifact (the host stays zero-nanollm/CGO-free)" >&2
        exit 1
    fi
    if [ "${NATIVE_WORKER_REQUEST}" = "false" ]; then
        echo "ERROR: NATIVE_ONLY_WORKER=true conflicts with NATIVE_WORKER=false — the native-only worker IS native_capable" >&2
        exit 1
    fi
fi

# The native worker is a SUBPROCESS-only capability by construction: nanollm
# links only into the worker subprocess, never the host. An in-process build has
# no separate worker process, so honouring NATIVE_WORKER there would mean linking
# nanollm into the host — exactly the invariant this arc preserves. The
# SHADOW_WORKER profile (de-BAML cutover Slice 4) is a native worker too, so it
# carries the same subprocess-only requirement.
if [ "${SUBPROCESS:-true}" != "true" ]; then
    if [ "${NATIVE_WORKER_REQUEST}" = "true" ] || [ "${SHADOW_WORKER}" = "true" ]; then
        echo "ERROR: NATIVE_WORKER/SHADOW_WORKER=true requires SUBPROCESS=true — the native" >&2
        echo "       (nanollm) runtime links only into the worker subprocess and must never" >&2
        echo "       enter the in-process host link graph." >&2
        exit 1
    fi
    # An in-process build has no worker subprocess to make native-capable, so it
    # is the BAML-only profile by construction, not by choice.
    NATIVE_WORKER=false
else
    NATIVE_WORKER="${NATIVE_WORKER_REQUEST:-true}"
fi

# ARTIFACT_PROFILE is the single source of truth for the rest of this script and
# for the -ldflags stamp: native_capable means the embedded worker is built from
# the isolated module and links the native engine (the S2 standard artifact, and
# also the shadow profile, whose worker links the engine too); baml_only means
# the zero-options root cmd/worker, which links no native engine at all.
if [ "${NATIVE_WORKER}" = "true" ] || [ "${SHADOW_WORKER}" = "true" ]; then
    ARTIFACT_PROFILE="native_capable"
else
    ARTIFACT_PROFILE="baml_only"
fi

if [ "${SHADOW_WORKER}" = "true" ]; then
    echo "Shadow Worker (BAML+nanollm + one-send shadow comparator, from isolated module): enabled"
elif [ "${NATIVE_WORKER}" = "true" ]; then
    echo "Native Worker (BAML+nanollm, serve-capable behind BAML_REST_USE_DEBAML, from isolated module): enabled"
else
    echo "Native Worker: disabled (BAML-only ROLLBACK artifact)"
fi
echo "Artifact Profile (de-BAML cutover S2): ${ARTIFACT_PROFILE}"
echo "============================================"

# Set up Go build tags
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
# De-BAML native stream cohort (Phase 7D): the SERVE-CAPABLE native worker
# (NATIVE_WORKER=true, subprocess-only) is the only profile that installs the
# native stream serve factory. Tag the HOST build so cmd/serve knows — at compile
# time — that its embedded worker can serve native streams, which arms the pool's
# stream-retry suppression (only alongside a truthy umbrella flag). The SHADOW
# worker is NOT a stream-serve profile, so it does NOT get the tag. This is the
# explicit compile/deployment capability the pool threads through
# configureWorkerMode; it never infers native-stream capability from the umbrella
# flag alone (scope §7D pool rule).
#
# The "NOT the shadow profile" half used to hold IMPLICITLY, because a shadow
# build left NATIVE_WORKER unset and unset meant false. S2 made unset mean the
# standard artifact, so the exclusion is now written out: a shadow worker still
# installs no stream serve factory, and tagging its host would arm the pool's
# stream-retry suppression against a worker that cannot serve a stream.
# The NATIVE-ONLY worker (ExecBridge-U1b) is ALSO excluded: its admitted cohort is
# unary final-call only (stream mode declines pre-socket), so its embedded worker
# cannot serve a native stream and the host must not arm stream-retry suppression
# for it.
if [ "${NATIVE_WORKER}" = "true" ] && [ "${SHADOW_WORKER}" != "true" ] && [ "${NATIVE_ONLY_WORKER:-false}" != "true" ]; then
    BUILD_TAGS="${BUILD_TAGS:+${BUILD_TAGS},}nativestreamserve"
fi
# De-BAML serving cutover S2: tag the HOST build with the ARTIFACT PROFILE, so
# cmd/serve knows at compile time whether the worker bytes it embeds link a
# native engine. Distinct from nativestreamserve above because the profile also
# covers the shadow build (natively linked, no stream serve factory) — deriving
# the profile from the stream tag would mislabel a shadow artifact as BAML-only.
# cmd/serve cross-checks this constant against the -ldflags profile stamp below
# at startup, so the tag and the stamp cannot drift apart silently.
if [ "${ARTIFACT_PROFILE}" = "native_capable" ]; then
    BUILD_TAGS="${BUILD_TAGS:+${BUILD_TAGS},}nativeworkerartifact"
fi
# ExecBridge-U1b: the native-only worker compiles the GENERATED registry (emitted
# below by cmd/gen-native-spine-worker) under this tag; without it the committed
# fail-loud stub (nativegenerated/generated_off.go) is selected instead. The host
# serve build carries the tag too (it is inert there — no root file is gated on it),
# so the same GO_BUILD_TAGS drives both builds and the axis is visible in the
# attested artifact ID.
if [ "${NATIVE_ONLY_WORKER:-false}" = "true" ]; then
    BUILD_TAGS="${BUILD_TAGS:+${BUILD_TAGS},}debamlnativeonlygenerated"
fi
GO_BUILD_TAGS=""
if [ -n "${BUILD_TAGS}" ]; then
    GO_BUILD_TAGS="-tags=${BUILD_TAGS}"
fi

# ============================================================================
# de-BAML serving cutover S2 — ARTIFACT-ID PROVENANCE
# ============================================================================
#
# cmd/build supplies both of these when it drives this script; the integration
# harness supplies them when it renders the same Dockerfile template; a hand-run
# build.sh has neither and records that HONESTLY as the explicit "unset" sentinel
# rather than silently producing an ID that cannot tell two releases apart. The
# native-worker tar digest is not passed at all — artifactattest computes it from
# this very build context, so it describes the tar that is about to be extracted
# and compiled.
#
# Resolved and VALIDATED HERE, before any build work: a bad provenance input is a
# caller/renderer bug, and finding out after the Node client generation and the
# BAML codegen wastes the entire build to report it.
# UNSET-ONLY expansion — `${VAR-unset}`, deliberately NOT `${VAR:-unset}`.
#
# `:-` substitutes for an unset variable AND for one set to the empty string, so
# the colon form cannot tell the deliberate `unset` SENTINEL from a renderer that
# supplied nothing. `ENV ARTIFACT_SOURCE_BUNDLE_DIGEST=""` would then be defaulted
# to `unset`, artifactattest would accept it (it accepts an explicit sentinel by
# design), and the build would mint a perfectly valid artifact ID whose provenance
# is absent by accident rather than by declaration. That is the same
# silently-accept-a-bad-input shape the `<no value>` regression had, one character
# away.
#
# So an explicitly EMPTY value is NOT substituted: it falls through to the
# validators below and is rejected loudly. `unset` stays the ONE accepted absence,
# and it is a token a caller writes on purpose.
ARTIFACT_SOURCE_REVISION="${ARTIFACT_SOURCE_REVISION-unset}"
ARTIFACT_SOURCE_BUNDLE_DIGEST="${ARTIFACT_SOURCE_BUNDLE_DIGEST-unset}"

# Validate the provenance inputs HERE, where the cause is still visible.
#
# Both values arrive from an ENV line the Dockerfile template interpolates, and
# Go's text/template renders a MISSING map key as the literal string `<no value>`.
# That is not a value this build can use, and it is not a value artifactattest can
# diagnose: from there it is just a malformed digest, and the message names the
# digest rather than the renderer that failed to supply one. The whole integration
# matrix went red exactly that way.
#
# So a bad value fails RIGHT HERE with a message that names the template key and
# what to do about it. Note what this does NOT do: it does not default, coerce or
# tolerate the bad value.
artifact_provenance_reject() {
    local name="$1" value="$2" template_key="$3" shape="$4"
    echo "ERROR: ${name}=\"${value}\" is not usable artifact provenance (want ${shape}, or the explicit \"unset\" sentinel)." >&2
    if [ -z "${value}" ]; then
        echo "       The variable is SET but EMPTY. An empty value is not the \"unset\" sentinel: the sentinel is" >&2
        echo "       a token a caller writes on purpose, whereas an empty value is what a renderer that supplied" >&2
        echo "       nothing produces. Defaulting it would mint a valid artifact ID with absent provenance." >&2
    fi
    if [ "${value}" = "<no value>" ]; then
        echo "       \"<no value>\" is Go text/template rendering a MISSING map key: whoever rendered" >&2
        echo "       cmd/build/Dockerfile.tmpl did not supply \"${template_key}\". Every renderer of that" >&2
        echo "       template must provide it — see cmd/build/main.go and integration/testutil/container.go." >&2
    fi
    exit 1
}

# The digest must be EXACTLY 16 lowercase hex characters — the same shape
# artifactprofile.ValidateArtifactID enforces at startup. Sixteen explicit classes
# anchor the whole string, because `case` globs match the entire word.
validate_artifact_digest() {
    local name="$1" value="$2" template_key="$3"
    if [ "${value}" = "unset" ]; then
        return 0
    fi
    case "${value}" in
        [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f])
            return 0
            ;;
    esac
    artifact_provenance_reject "${name}" "${value}" "${template_key}" "16 lowercase hex characters"
}

# The revision must be non-empty and contain ONLY the bounded character class
# artifactprofile.isBoundedRevision accepts.
#
# Checked with a NEGATED class — "does any forbidden character appear?" — rather
# than with an allowed-class glob. An allowed-class glob of the form
# `[A-Za-z0-9._/+-]*[A-Za-z0-9._/+-]` reads as if it constrains the whole string,
# but the `*` in the middle matches ARBITRARY characters, so `abc<def` sailed
# through it. The negated form has no such hole: one forbidden character anywhere
# matches, and the check cannot be satisfied by a compliant prefix and suffix.
validate_artifact_revision() {
    local name="$1" value="$2" template_key="$3"
    if [ "${value}" = "unset" ]; then
        return 0
    fi
    case "${value}" in
        "" | *[!A-Za-z0-9._/+-]*)
            artifact_provenance_reject "${name}" "${value}" "${template_key}" \
                "a bounded revision token (letters, digits, . _ / + -)"
            ;;
    esac
    return 0
}

validate_artifact_digest ARTIFACT_SOURCE_BUNDLE_DIGEST "${ARTIFACT_SOURCE_BUNDLE_DIGEST}" artifactSourceBundleDigest
validate_artifact_revision ARTIFACT_SOURCE_REVISION "${ARTIFACT_SOURCE_REVISION}" artifactSourceRevision

# de-BAML serving cutover S2 proof hook: print the resolved artifact selection
# and exit before the build itself starts — no toolchain, no network, no
# generated sources, nothing written outside the cache directories the env
# already points at. It exists so the artifact-selection contract (standard
# default, named rollback, in-process downgrade, shadow profile, tag coupling) is
# tested against THIS script rather than against a second copy of the rules in a
# test file — the copy that would be the one to drift.
if [ "${ARTIFACT_PROFILE_DRY_RUN:-false}" = "true" ]; then
    echo "artifact_profile=${ARTIFACT_PROFILE}"
    echo "artifact_source_revision=${ARTIFACT_SOURCE_REVISION}"
    echo "artifact_source_bundle_digest=${ARTIFACT_SOURCE_BUNDLE_DIGEST}"
    echo "native_worker=${NATIVE_WORKER}"
    echo "shadow_worker=${SHADOW_WORKER}"
    echo "native_only_worker=${NATIVE_ONLY_WORKER}"
    echo "subprocess=${SUBPROCESS:-true}"
    echo "build_tags=${BUILD_TAGS}"
    exit 0
fi

# Create working directory structure
WORK_DIR="$(mktemp -d)"
if [ "${KEEP_SOURCE:-false}" != "true" ]; then
    trap "rm -rf ${WORK_DIR}" EXIT
fi

BAML_WORK="${WORK_DIR}/baml"
BUILD_WORK="${WORK_DIR}/build"

mkdir -p "${BAML_WORK}"
mkdir -p "${BUILD_WORK}"

echo ""
echo "=== Stage 1: Node.js Client Generation ==="
echo ""

# Check for required tools (Node.js stage - only needed when not using custom CLI)
if [ -z "${BAML_CLI_PATH:-}" ]; then
    if ! command -v node &> /dev/null; then
        echo "ERROR: node is not installed or not in PATH"
        exit 1
    fi

    if ! command -v npx &> /dev/null; then
        echo "ERROR: npx is not installed or not in PATH"
        exit 1
    fi
fi

# Copy user's baml_src to working directory
echo "Copying baml_src from ${USER_CONTEXT_PATH}..."
cp -r "${USER_CONTEXT_PATH}/baml_src" "${BAML_WORK}/baml_src"

# Inject dynamic prompt (file provided by baml_rest sources)
# Requires BAML >= 0.215.0
DYNAMIC_TEMPLATE="${USER_CONTEXT_PATH}/baml_rest/cmd/build/dynamic.baml"
MIN_DYNAMIC_VERSION="0.215.0"
if [ -f "${DYNAMIC_TEMPLATE}" ]; then
    # Compare versions using sort -V
    if printf '%s\n%s\n' "$MIN_DYNAMIC_VERSION" "$BAML_VERSION" | sort -V -C; then
        echo "Injecting dynamic prompt (BAML ${BAML_VERSION} >= ${MIN_DYNAMIC_VERSION})..."
        cp "${DYNAMIC_TEMPLATE}" "${BAML_WORK}/baml_src/__baml_rest_internal_dynamic__.baml"
    else
        echo "Skipping dynamic prompt injection (BAML ${BAML_VERSION} < ${MIN_DYNAMIC_VERSION})"
    fi
fi

# Change to baml working directory
cd "${BAML_WORK}"

# Run remove_unneeded_blocks.sh script
echo "Removing unneeded blocks from .baml files..."
KEYWORDS='generator|test'

find baml_src -type f -name '*.baml' -print0 | while IFS= read -r -d '' file; do
  gawk -v kw="$KEYWORDS" '
    BEGIN { pat = "^[ \t]*(" kw ")[ \t][^{]*\\{" }
    $0 ~ pat {
      in_block = 1
      tmp = $0
      opens  = gsub(/\{/, "", tmp)
      closes = gsub(/\}/, "", tmp)
      level = opens - closes
      if (level <= 0) in_block = 0
      next
    }
    in_block {
      tmp = $0
      opens  = gsub(/\{/, "", tmp)
      closes = gsub(/\}/, "", tmp)
      level += opens - closes
      if (level <= 0) in_block = 0
      next
    }
    { print }
  ' "$file" > "${file}.tmp" && mv "${file}.tmp" "$file"
done

# Render clients.baml template using envsubst
echo "Rendering clients.baml template..."
export OUTPUT_DIR="../baml_rest_generated"
cat > clients.baml.template <<'EOF'
generator baml_rest_target {
  output_type "go"
  output_dir "${OUTPUT_DIR}"
  version "${BAML_VERSION}"
  client_package_name "github.com/invakid404/baml-rest"
}
EOF

cat clients.baml.template | envsubst | tee baml_src/baml_rest_client.baml

# Generate BAML client
if [ -n "${BAML_CLI_PATH:-}" ]; then
    echo "Running BAML client generation (${BAML_CLI_PATH} generate --no-version-check)..."
    "${BAML_CLI_PATH}" generate --no-version-check
else
    echo "Running BAML client generation (npx @boundaryml/baml@${BAML_VERSION} generate)..."
    NPX_MAX_RETRIES=3
    NPX_RETRY_DELAY=5
    for NPX_ATTEMPT in $(seq 1 "${NPX_MAX_RETRIES}"); do
        if npx "@boundaryml/baml@${BAML_VERSION}" generate; then
            break
        fi
        if [ "${NPX_ATTEMPT}" -eq "${NPX_MAX_RETRIES}" ]; then
            echo "ERROR: npx failed after ${NPX_MAX_RETRIES} attempts"
            exit 1
        fi
        echo "npx failed (attempt ${NPX_ATTEMPT}/${NPX_MAX_RETRIES}), retrying in ${NPX_RETRY_DELAY}s..."
        sleep "${NPX_RETRY_DELAY}"
        NPX_RETRY_DELAY=$((NPX_RETRY_DELAY * 2))
    done
fi

echo ""
echo "=== Stage 2: Go Build ==="
echo ""

# Check for required tools (Go stage)
if ! command -v go &> /dev/null; then
    echo "ERROR: go is not installed or not in PATH"
    exit 1
fi

# Install goimports if not available
if ! command -v goimports &> /dev/null; then
    echo "Installing goimports..."
    go install golang.org/x/tools/cmd/goimports@latest
fi

# Copy baml_rest sources to build directory
echo "Copying baml_rest sources to build directory..."
cp -r "${USER_CONTEXT_PATH}/baml_rest" "${BUILD_WORK}/baml_rest"

# Change to build working directory
cd "${BUILD_WORK}/baml_rest"

# Copy generated BAML client
echo "Copying generated BAML client..."
cp -r "${BAML_WORK}/baml_rest_generated/baml_client" ./baml_client

# Initialize Go module for generated client
echo "Initializing Go module for BAML client..."
pushd baml_client
go mod init github.com/invakid404/baml-rest/baml_client
popd

# The embedded customer-build source bundle intentionally excludes the
# public dynclient module (added in #289 PR B). Strip dev-workspace
# references before the first `go work` command validates all use
# entries — Go errors on missing use targets before applying the new
# `go work use ./baml_client` below. Guarded on the absence of
# `dynclient/` so a dev checkout (where the module is present) is
# untouched.
if [ ! -d dynclient ]; then
    echo "Removing dynclient workspace/module entries from server-only build tree..."
    go work edit \
        -dropuse=./dynclient \
        -dropuse=./dynclient/baml-patched
    go mod edit \
        -droprequire github.com/invakid404/baml-rest/dynclient \
        -droprequire github.com/invakid404/baml-rest/dynclient/baml-patched \
        -dropreplace github.com/invakid404/baml-rest/dynclient \
        -dropreplace github.com/invakid404/baml-rest/dynclient/baml-patched
fi

# Add generated client to Go workspace
echo "Adding BAML client to Go workspace..."
go work use ./baml_client

# Clean up unused adapters to avoid version conflicts
echo ""
echo "=== Cleaning up unused adapters ==="
SELECTED_ADAPTER=$(basename "${ADAPTER_VERSION}")
echo "Selected adapter: ${SELECTED_ADAPTER}"

# Delete all adapter_v* directories except the selected one
echo "Removing unused adapter directories from adapters/..."
for adapter_dir in adapters/adapter_v*/; do
    if [ -d "${adapter_dir}" ]; then
        adapter_name=$(basename "${adapter_dir}")
        if [ "${adapter_name}" != "${SELECTED_ADAPTER}" ]; then
            echo "  Deleting ${adapter_dir}..."
            rm -rf "${adapter_dir}"
        else
            echo "  Keeping ${adapter_dir}"
        fi
    fi
done

# Remove replace statements from go.mod for deleted adapters
echo "Cleaning up go.mod replace statements..."
tmp_file=$(mktemp)
in_replace_block=0

while IFS= read -r line; do
    # Detect start of replace block
    if [[ "$line" =~ ^replace[[:space:]]*\( ]]; then
        in_replace_block=1
        echo "$line" >> "$tmp_file"
    # Detect end of replace block
    elif [[ "$in_replace_block" -eq 1 ]] && [[ "$line" =~ ^\) ]]; then
        in_replace_block=0
        echo "$line" >> "$tmp_file"
    # If we're in the replace block
    elif [[ "$in_replace_block" -eq 1 ]]; then
        # Check if this line references an adapter_v* directory
        if [[ "$line" =~ adapters/adapter_v[0-9_]+ ]]; then
            # Extract the adapter name from the line
            adapter_in_line=$(echo "$line" | grep -oE 'adapter_v[0-9_]+' | head -1)
            # Only keep the line if it matches the selected adapter
            if [ "${adapter_in_line}" == "${SELECTED_ADAPTER}" ]; then
                echo "$line" >> "$tmp_file"
            else
                echo "  Removing replace for: ${adapter_in_line}"
            fi
        else
            # Not an adapter line, keep it (common, bamlutils, introspected, etc.)
            echo "$line" >> "$tmp_file"
        fi
    else
        # Not in replace block, keep the line
        echo "$line" >> "$tmp_file"
    fi
done < go.mod

mv "$tmp_file" go.mod

# Remove require statements from go.mod for deleted adapters
echo "Cleaning up go.mod require statements..."
tmp_file=$(mktemp)
in_require_block=0

while IFS= read -r line; do
    # Detect start of require block
    if [[ "$line" =~ ^require[[:space:]]*\( ]]; then
        in_require_block=1
        echo "$line" >> "$tmp_file"
    # Detect end of require block
    elif [[ "$in_require_block" -eq 1 ]] && [[ "$line" =~ ^\) ]]; then
        in_require_block=0
        echo "$line" >> "$tmp_file"
    # If we're in the require block
    elif [[ "$in_require_block" -eq 1 ]]; then
        # Check if this line references an adapter_v* module
        if [[ "$line" =~ github\.com/invakid404/baml-rest/adapters/adapter_v[0-9_]+ ]]; then
            # Extract the adapter name from the line
            adapter_in_line=$(echo "$line" | grep -oE 'adapter_v[0-9_]+' | head -1)
            # Only keep the line if it matches the selected adapter
            if [ "${adapter_in_line}" == "${SELECTED_ADAPTER}" ]; then
                echo "$line" >> "$tmp_file"
            else
                echo "  Removing require for: ${adapter_in_line}"
            fi
        else
            # Not an adapter_v* line, keep it (common, bamlutils, introspected, other deps, etc.)
            echo "$line" >> "$tmp_file"
        fi
    else
        # Not in require block, keep the line
        echo "$line" >> "$tmp_file"
    fi
done < go.mod

mv "$tmp_file" go.mod

# Remove use statements from go.work for deleted adapters
echo "Cleaning up go.work use statements..."
tmp_file=$(mktemp)
in_use_block=0

while IFS= read -r line; do
    # Detect start of use block
    if [[ "$line" =~ ^use[[:space:]]*\( ]]; then
        in_use_block=1
        echo "$line" >> "$tmp_file"
    # Detect end of use block
    elif [[ "$in_use_block" -eq 1 ]] && [[ "$line" =~ ^\) ]]; then
        in_use_block=0
        echo "$line" >> "$tmp_file"
    # If we're in the use block
    elif [[ "$in_use_block" -eq 1 ]]; then
        # Check if this line references an adapter_v* directory
        if [[ "$line" =~ adapters/adapter_v[0-9_]+ ]]; then
            # Extract the adapter name from the line
            adapter_in_line=$(echo "$line" | grep -oE 'adapter_v[0-9_]+' | head -1)
            # Only keep the line if it matches the selected adapter
            if [ "${adapter_in_line}" == "${SELECTED_ADAPTER}" ]; then
                echo "$line" >> "$tmp_file"
            else
                echo "  Removing use for: ${adapter_in_line}"
            fi
        else
            # Not an adapter line, keep it (., common, bamlutils, introspected, etc.)
            echo "$line" >> "$tmp_file"
        fi
    else
        # Not in use block, keep the line
        echo "$line" >> "$tmp_file"
    fi
done < go.work

mv "$tmp_file" go.work

# Regenerate embed.go to reflect the deleted adapters
echo "Regenerating embed.go..."
go run cmd/embed/main.go

echo "Adapter cleanup complete!"
echo ""

# Handle BAML Go dependency
if [ -n "${CUSTOM_BAML_GO_LIB:-}" ]; then
    echo "Using custom BAML Go library: ${CUSTOM_BAML_GO_LIB}"
    # Add replace directive FIRST so Go resolves locally (required for unreleased versions)
    echo "Adding replace directive for custom BAML Go library to go.work..."
    go work edit -replace "github.com/boundaryml/baml=${CUSTOM_BAML_GO_LIB}"

    # Add placeholder require entry for baml_client (version resolved via replace)
    pushd baml_client
    echo "Adding placeholder BAML dependency to baml_client..."
    go mod edit -require "github.com/boundaryml/baml@v0.0.0"
    popd
else
    # Get BAML dependency from module proxy
    echo "Getting BAML Go dependency (github.com/boundaryml/baml@${BAML_VERSION})..."
    go get "github.com/boundaryml/baml@${BAML_VERSION}"
fi

# Sync Go workspace
echo "Syncing Go workspace..."
go work sync

if [ -z "${CUSTOM_BAML_GO_LIB:-}" ]; then
    # Ensure baml_client uses the correct BAML version
    pushd baml_client
    echo "Setting BAML version in baml_client to ${BAML_VERSION}..."
    go get "github.com/boundaryml/baml@${BAML_VERSION}"
    popd
fi

# Run hacks to patch generated BAML client
echo "Running hacks..."
go run cmd/hacks/main.go --baml-client-dir ./baml_client --baml-version "${BAML_VERSION}"

# Copy baml_src into the build directory so the introspect command can
# parse .baml files for fallback chains, client providers, retry policies, etc.
# (baml_src lives in the BAML working directory; the build runs here.)
echo "Copying baml_src for introspection..."
rm -rf ./baml_src
cp -r "${BAML_WORK}/baml_src" ./baml_src

# Run introspection.
#
# Use the PACKAGE form (`./cmd/introspect`), NOT a single file
# (`cmd/introspect/main.go`). The introspect command is a multi-file package —
# main.go plus schemabuild.go (de-BAML P3 slice 2, #586) and any future
# siblings — and `go run <file>.go` compiles ONLY the named file, so a symbol
# defined in a sibling (e.g. buildStaticSchemas) is "undefined" at container
# build time even though the file ships in the embed.FS. The package form
# compiles every .go file in the directory, so new introspect files need no
# change here. (This masked a red CI on PR #590: `go build ./...` compiles the
# whole package locally and hid the single-file break.)
echo "Running introspection..."
go run ./cmd/introspect

# Format and organize imports
echo "Formatting code and organizing imports..."
gofmt -w .
goimports -w .

# Run adapter
echo "Running adapter (${ADAPTER_VERSION})..."
go run "${ADAPTER_VERSION}/cmd/main.go"

# URL-rewrite ldflag goes onto whichever binary actually links the
# BAML call path. Subprocess builds bake it into cmd/worker;
# in-process builds bake it into cmd/serve because cmd/worker is not
# built at all.
WORKER_LDFLAGS="-s -w"
SERVE_LDFLAGS="-s -w"

# ============================================================================
# de-BAML serving cutover S2 — ARTIFACT ATTESTATION STAMP
# ============================================================================
#
# Stamp the resolved artifact profile and a reproducible release artifact ID
# into BOTH binaries. At startup each one cross-checks the stamp against what it
# demonstrably IS — the worker against its linked native capability, the host
# against its `nativeworkerartifact` build tag — and refuses to serve on a
# contradiction. So this stamp is not a decoration: it is the claim that makes a
# mislabelled artifact a startup failure instead of a wrong dashboard label.
#
# The worker package is resolved HERE (and reused by the build below) so the
# thing that is stamped and the thing that is built cannot drift.
NATIVE_WORKER_PKG=""
ARTIFACT_WORKER_PKG=""
if [ "${SUBPROCESS:-true}" = "true" ]; then
    if [ "${ARTIFACT_PROFILE}" = "native_capable" ]; then
        # Isolated-module worker profiles. NATIVE_ONLY (ExecBridge-U1b) takes highest
        # precedence, then SHADOW (no-send comparator), then the standard serve worker.
        # The distinct package makes the attested artifact ID distinct via
        # Inputs.WorkerPackage — no third artifact profile is needed (native_capable
        # stays a true derived fact for the native-only worker).
        NATIVE_WORKER_PKG="./cmd/worker/"
        if [ "${NATIVE_ONLY_WORKER:-false}" = "true" ]; then
            NATIVE_WORKER_PKG="./cmd/worker-nativeonly/"
        elif [ "${SHADOW_WORKER}" = "true" ]; then
            NATIVE_WORKER_PKG="./cmd/worker-shadow/"
        fi
        ARTIFACT_WORKER_PKG="nanollmprepare:${NATIVE_WORKER_PKG}"
    else
        ARTIFACT_WORKER_PKG="root:./cmd/worker/"
    fi
fi

ARTIFACT_PROFILE_PKG="github.com/invakid404/baml-rest/internal/artifactprofile"
echo "Computing de-BAML S2 release artifact ID..."
# artifactattest prints `artifact_id=` and `artifact_inputs=`. BOTH are stamped:
# the inputs are the evidence the startup attestation re-derives the ID from, so a
# well-formed but wrong ID cannot serve.
ARTIFACT_ATTEST_OUT="$(go run ./cmd/build/artifactattest \
    --profile "${ARTIFACT_PROFILE}" \
    --worker-package "${ARTIFACT_WORKER_PKG}" \
    --build-tags "${BUILD_TAGS}" \
    --subprocess "${SUBPROCESS:-true}" \
    --baml-version "${BAML_VERSION}" \
    --adapter-version "${ADAPTER_VERSION}" \
    --source-revision "${ARTIFACT_SOURCE_REVISION}" \
    --source-bundle-digest "${ARTIFACT_SOURCE_BUNDLE_DIGEST}")"
ARTIFACT_ID="$(printf '%s\n' "${ARTIFACT_ATTEST_OUT}" | sed -n 's/^artifact_id=//p')"
ARTIFACT_INPUTS="$(printf '%s\n' "${ARTIFACT_ATTEST_OUT}" | sed -n 's/^artifact_inputs=//p')"
if [ -z "${ARTIFACT_ID}" ] || [ -z "${ARTIFACT_INPUTS}" ]; then
    echo "ERROR: artifactattest did not emit both artifact_id and artifact_inputs" >&2
    printf '%s\n' "${ARTIFACT_ATTEST_OUT}" >&2
    exit 1
fi
echo "Artifact Profile: ${ARTIFACT_PROFILE}  Artifact ID: ${ARTIFACT_ID}"
echo "Artifact Provenance: source_revision=${ARTIFACT_SOURCE_REVISION} source_bundle_digest=${ARTIFACT_SOURCE_BUNDLE_DIGEST}"
ARTIFACT_STAMP_LDFLAGS="-X '${ARTIFACT_PROFILE_PKG}.stampedProfile=${ARTIFACT_PROFILE}' -X '${ARTIFACT_PROFILE_PKG}.stampedArtifactID=${ARTIFACT_ID}' -X '${ARTIFACT_PROFILE_PKG}.stampedArtifactInputs=${ARTIFACT_INPUTS}'"
WORKER_LDFLAGS="${WORKER_LDFLAGS} ${ARTIFACT_STAMP_LDFLAGS}"
SERVE_LDFLAGS="${SERVE_LDFLAGS} ${ARTIFACT_STAMP_LDFLAGS}"

if [ -n "${BAML_REST_BASE_URL_REWRITES:-}" ]; then
    echo "Baking in base URL rewrites: ${BAML_REST_BASE_URL_REWRITES}"
    if [ "${SUBPROCESS:-true}" = "true" ]; then
        WORKER_LDFLAGS="${WORKER_LDFLAGS} -X 'github.com/invakid404/baml-rest/bamlutils/urlrewrite.builtinRules=${BAML_REST_BASE_URL_REWRITES}'"
    else
        SERVE_LDFLAGS="${SERVE_LDFLAGS} -X 'github.com/invakid404/baml-rest/bamlutils/urlrewrite.builtinRules=${BAML_REST_BASE_URL_REWRITES}'"
    fi
fi

if [ "${SUBPROCESS:-true}" = "true" ]; then
    # Build worker binary first (this imports baml and loads the shared library).
    # The host embeds these bytes at cmd/serve/worker below; whichever variant
    # we build here becomes the embedded worker payload. The host link graph is
    # identical either way — it only embeds an opaque byte slice.
    if [ "${ARTIFACT_PROFILE}" = "native_capable" ]; then
        # de-BAML cutover Slice 2 (option b): build the BAML+nanollm worker FROM
        # the isolated, out-of-go.work internal/nativebody/nanollmprepare module
        # with GOWORK=off + CGO so the nanollm static archive links ONLY into the
        # worker subprocess. The host/root module graph is never consulted for
        # this build (GOWORK=off makes the module's own go.mod authoritative), so
        # it stays zero-nanollm and CGO-free. Output goes to the SAME embed
        # location so the host build below is unchanged.
        #
        # Two isolated-module worker profiles select which package is built:
        #   - NATIVE_WORKER: cmd/worker — the SERVE-CAPABLE profile (de-BAML cutover
        #     Slice 6). While BAML_REST_USE_DEBAML is enabled it installs the native
        #     serve callback and actually serves an admitted unary `_dynamic` /call
        #     natively (one exact provider RoundTrip); unsupported traffic declines
        #     pre-socket to BAML. Flag-off is ZERO native (no FFI/callback/socket),
        #     byte-identical to the BAML-only worker — the immediate kill switch.
        #   - SHADOW_WORKER: cmd/worker-shadow — the one-send SHADOW profile (S4).
        #     It additionally installs the native shadow comparator, which (only
        #     while the umbrella flag is enabled) compares native vs BAML request
        #     plans with NO socket and then declines, so BAML still serves every
        #     request byte-identically. SHADOW_WORKER takes precedence.
        # NATIVE_WORKER_PKG was resolved next to the S2 artifact stamp above, so
        # the package named in the attested artifact ID is exactly the package
        # built here.
        # The isolated module is EXCLUDED from the embed source bundle
        # (.embedignore) so cmd/embed never imports it into the root link graph;
        # it rides along as the opaque tar cmd/build/nativeworker_module.tar
        # (which ships in the bundle under the already-embedded cmd/build dir).
        # As of de-BAML #624 that SAME tar also carries the sibling public
        # nativeserve module (the nanollm-linked serve core cmd/worker imports); it
        # is likewise .embedignore'd, so extraction restores BOTH modules into their
        # canonical paths. Restore here, AFTER the embed regen above (so cmd/embed
        # never discovers a nanollm module). Full-checkout builds already have the
        # directories; extracting over them is a no-op on content because the tar is
        # the authoritative committed snapshot.
        if [ ! -f cmd/build/nativeworker_module.tar ]; then
            echo "ERROR: artifact profile is native_capable but cmd/build/nativeworker_module.tar is missing from the build context" >&2
            echo "       (build the BAML-only ROLLBACK artifact with NATIVE_WORKER=false if that is what you want)" >&2
            exit 1
        fi
        echo "Restoring isolated nanollm worker + nativeserve modules from opaque asset (cmd/build/nativeworker_module.tar)..."
        tar -xf cmd/build/nativeworker_module.tar

        # Overlay the extracted WORKER module's OWN go.mod so it can resolve this
        # build's generated ./baml_client and use the selected/custom BAML under
        # GOWORK=off (the isolated module never sees the builder's go.work, where
        # `go work use ./baml_client` and any custom-BAML replace live). The
        # overlay edits ONLY the throwaway extracted go.mod — root go.mod/go.work
        # are untouched, so the host stays zero-nanollm/CGO-free. It also drops
        # replaces whose targets this server bundle trimmed (dynclient, unselected
        # adapters). Root's generated InitBamlRuntime imports baml_client, so the
        # worker (via root) transitively needs it. Only the WORKER module (the build's
        # main module) needs the overlay: nativeserve is a DEPENDENCY here, and Go
        # ignores a dependency module's replace directives, so its own trimmed-target
        # replaces never dangle and it needs no baml_client (it imports none).
        if [ "${NATIVE_ONLY_WORKER:-false}" = "true" ]; then
            # ExecBridge-U1b: the NATIVE-ONLY worker. Overlay in native-only mode —
            # ONLY the generic missing-replace cleanup, NO baml_client / BAML wiring —
            # then generate the deployment-specific native registry and build the
            # BAML-free command. The overlay's mode is validated mutually exclusive
            # with the BAML mode, so this path can never silently gain baml_client.
            echo "Overlaying isolated worker module go.mod (native-only: missing-replace cleanup only, NO BAML wiring)..."
            go run ./cmd/build/nativeworker-overlay \
                --module-dir internal/nativebody/nanollmprepare \
                --native-only

            # Emit the project's own codegen-spine descriptor and generate the native
            # registry into the extracted tree. Generation cleans its output dir first,
            # so a method removed since the last build cannot survive as a stale
            # registration. The committed generated_off.go stub is left in place and is
            # excluded by the debamlnativeonlygenerated tag below.
            echo "Emitting native-spine project descriptor..."
            go run ./cmd/introspect \
                --native-spine-descriptors ./nativeworker_descriptors.json \
                --baml-src-dir ./baml_src
            echo "Generating native-only worker registry (cmd/gen-native-spine-worker)..."
            go run ./cmd/gen-native-spine-worker \
                --descriptors ./nativeworker_descriptors.json \
                --out-dir internal/nativebody/nanollmprepare/nativegenerated

            # Whole-command DEPENDENCY GATE against the exact package/tags built into
            # cmd/serve/worker: the packaged native-only command must link NO BAML,
            # CFFI, dynclient, rootruntime, introspected, workerboot, or the root
            # generated baml_rest package. This is defense-in-depth for the standalone
            # go-list-deps test; a red here fails the build before it embeds anything.
            echo "Running native-only worker dependency gate (go list -deps -tags=${BUILD_TAGS})..."
            NATIVE_ONLY_DEPS="$(mktemp)"
            (
                cd internal/nativebody/nanollmprepare
                GOWORK=off go list -deps -tags="${BUILD_TAGS}" ./cmd/worker-nativeonly
            ) > "${NATIVE_ONLY_DEPS}"
            if [ ! -s "${NATIVE_ONLY_DEPS}" ]; then
                echo "ERROR: native-only dependency gate produced no output (wrong package/tags?)" >&2
                exit 1
            fi
            if grep -Eq 'baml_client|github\.com/boundaryml/baml|github\.com/invakid404/baml-rest/dynclient|dynclient/baml-patched|language_client_go|github\.com/invakid404/baml-rest/internal/rootruntime|github\.com/invakid404/baml-rest/introspected|github\.com/invakid404/baml-rest/internal/workerboot|^github\.com/invakid404/baml-rest$' "${NATIVE_ONLY_DEPS}"; then
                echo "ERROR: the native-only worker's compiled dependency graph contains a forbidden BAML/CFFI/dynclient/rootruntime/introspected/workerboot/root-baml_rest dependency:" >&2
                grep -E 'baml_client|github\.com/boundaryml/baml|github\.com/invakid404/baml-rest/dynclient|dynclient/baml-patched|language_client_go|github\.com/invakid404/baml-rest/internal/rootruntime|github\.com/invakid404/baml-rest/introspected|github\.com/invakid404/baml-rest/internal/workerboot|^github\.com/invakid404/baml-rest$' "${NATIVE_ONLY_DEPS}" >&2
                rm -f "${NATIVE_ONLY_DEPS}"
                exit 1
            fi
            rm -f "${NATIVE_ONLY_DEPS}"

            echo "Building native-only worker binary (${NATIVE_WORKER_PKG}, BAML-free, from isolated nanollmprepare module)..."
            WORKER_OUT="$(pwd)/cmd/serve/worker"
            (
                cd internal/nativebody/nanollmprepare
                # -mod=mod so the build may populate this module's go.sum for any
                # transitive dep the generated registry pulls; the root module's go.sum
                # is never consulted (GOWORK=off). CGO for the nanollm archive.
                GOWORK=off GOFLAGS=-mod=mod CGO_ENABLED=1 go build ${GO_BUILD_TAGS} ${WORKER_LDFLAGS:+-ldflags "${WORKER_LDFLAGS}"} -o "${WORKER_OUT}" "${NATIVE_WORKER_PKG}"
            )
        else
            echo "Overlaying isolated worker module go.mod (baml_client + BAML selection)..."
            NATIVE_WORKER_OVERLAY_ARGS=(
                --module-dir internal/nativebody/nanollmprepare
                --baml-client ../../../baml_client
                --baml-version "${BAML_VERSION}"
            )
            if [ -n "${CUSTOM_BAML_GO_LIB:-}" ]; then
                NATIVE_WORKER_OVERLAY_ARGS+=(--custom-baml-lib "${CUSTOM_BAML_GO_LIB}")
            fi
            go run ./cmd/build/nativeworker-overlay "${NATIVE_WORKER_OVERLAY_ARGS[@]}"

            echo "Building BAML+nanollm worker binary (${NATIVE_WORKER_PKG}, from isolated nanollmprepare module)..."
            WORKER_OUT="$(pwd)/cmd/serve/worker"
            (
                cd internal/nativebody/nanollmprepare
                # -mod=mod so the overlay's baml_client (local) transitive deps and any
                # aligned BAML version can populate this module's go.sum during the
                # build; the root module's go.sum is never consulted (GOWORK=off).
                GOWORK=off GOFLAGS=-mod=mod CGO_ENABLED=1 go build ${GO_BUILD_TAGS} ${WORKER_LDFLAGS:+-ldflags "${WORKER_LDFLAGS}"} -o "${WORKER_OUT}" "${NATIVE_WORKER_PKG}"
            )
        fi
    else
        # The explicit ROLLBACK artifact: the BAML-only worker from the root
        # module (imports baml, loads its shared library; no nanollm). This is
        # what keeps "BAML-only worker = 100% BAML" a build-level kill switch.
        # Since S2 it is no longer the default — it is selected on purpose with
        # NATIVE_WORKER=false (cmd/build: --baml-only-rollback-worker) — but it
        # remains a first-class, fully buildable artifact, because
        # BAML_REST_USE_DEBAML=false only promises a total BAML revert for as long
        # as a BAML-capable artifact still exists to revert to.
        echo "Building worker binary (BAML-only ROLLBACK artifact)..."
        go build ${GO_BUILD_TAGS} ${WORKER_LDFLAGS:+-ldflags "${WORKER_LDFLAGS}"} -o cmd/serve/worker ./cmd/worker/
    fi
else
    echo "Skipping worker binary build (in-process mode)..."
fi

# Generate OpenAPI schema (this also imports baml)
echo "Generating OpenAPI schema..."
go run cmd/schema/main.go cmd/serve/openapi.json

# Build final binary (embeds worker and schema, doesn't import baml directly
# in subprocess builds; in in-process builds the worker handler is linked in)
echo "Building final binary${GO_BUILD_TAGS:+ with tags: ${GO_BUILD_TAGS#-tags=}}..."
go build ${GO_BUILD_TAGS} ${SERVE_LDFLAGS:+-ldflags "${SERVE_LDFLAGS}"} -o baml-rest ./cmd/serve/

# Clean up intermediate files from cmd/serve (they're embedded now)
rm -f cmd/serve/worker cmd/serve/openapi.json

# Create output directory and copy binary
OUTPUT_DIR="$(dirname "${OUTPUT_PATH}")"
mkdir -p "${OUTPUT_DIR}"

echo "Copying binary to ${OUTPUT_PATH}..."
cp baml-rest "${OUTPUT_PATH}"

# Handle source preservation if KEEP_SOURCE is enabled
if [ "${KEEP_SOURCE:-false}" = "true" ]; then
    echo ""
    echo "KEEP_SOURCE enabled - preserving generated source files..."

    KEEP_SOURCE_DIR="${KEEP_SOURCE_DIR:-/baml-rest-generated-src}"

    # Attempt to create the directory and all parent directories
    if mkdir -p "${KEEP_SOURCE_DIR}" 2>/dev/null && [ -d "${KEEP_SOURCE_DIR}" ] && [ -w "${KEEP_SOURCE_DIR}" ]; then
        # Successfully created/verified directory with write permissions
        echo "Copying generated source to ${KEEP_SOURCE_DIR}..."
        if cp -r . "${KEEP_SOURCE_DIR}/" 2>/dev/null; then
            echo "Generated source files saved to ${KEEP_SOURCE_DIR}"
        else
            echo "WARNING: Failed to copy source files to ${KEEP_SOURCE_DIR}"
            echo "Generated source files preserved at: ${BUILD_WORK}/baml_rest"
            KEEP_SOURCE_DIR="${BUILD_WORK}/baml_rest"
        fi
    else
        # Cannot create directory or insufficient permissions - fall back to temp directory
        echo "Cannot write to ${KEEP_SOURCE_DIR} (permissions or invalid path)"
        echo "Generated source files preserved at: ${BUILD_WORK}/baml_rest"
        KEEP_SOURCE_DIR="${BUILD_WORK}/baml_rest"
    fi
fi

# Copy BAML cache from build location to final destination
echo ""
echo "Copying BAML cache from build location to final destination..."
mkdir -p "${BAML_CACHE_FINAL}"
if [ -d "${BAML_CACHE_BUILD}" ]; then
    cp -r "${BAML_CACHE_BUILD}/." "${BAML_CACHE_FINAL}/" || echo "WARNING: Failed to copy BAML cache to final destination"
    echo "BAML cache copied to: ${BAML_CACHE_FINAL}"
else
    echo "WARNING: BAML cache build directory does not exist: ${BAML_CACHE_BUILD}"
fi

echo ""
echo "============================================"
echo "Build completed successfully!"
echo "Binary location: ${OUTPUT_PATH}"
if [ "${KEEP_SOURCE:-false}" = "true" ]; then
    echo "Generated source: ${KEEP_SOURCE_DIR}"
fi
echo "BAML cache location: ${BAML_CACHE_FINAL}"
echo "============================================"
