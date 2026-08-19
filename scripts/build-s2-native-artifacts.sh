#!/usr/bin/env bash
set -euo pipefail

# Build the de-BAML serving-cutover S2 NATIVE-CAPABLE deployable artifacts the way
# cmd/build/build.sh builds and stamps them, and print the env assignments the
# non-skippable artifact proof (`go test -tags=nativeartifactproof
# ./internal/workerboot`) consumes.
#
# WHY THIS EXISTS AS A SCRIPT. The proof lane must boot the EXACT artifacts S2
# promotes, not a lookalike: same isolated module, same GOWORK=off + CGO build,
# same build tags, and above all the same `-ldflags` attestation stamp — profile,
# release artifact ID, and the inputs the ID is verified against at startup. All
# of that comes from cmd/build/artifactattest here, exactly as build.sh gets it,
# so the proof cannot drift into asserting something about a differently-stamped
# binary. Having one script also means the lane and a developer run the same
# thing.
#
# It builds EVERY entrypoint build.sh can ship as a native_capable artifact. A
# cold review found a flag-off kill-switch failure in cmd/worker-shadow that had
# shipped precisely because only the sibling entrypoint was ever booted.
#
# Usage:
#   scripts/build-s2-native-artifacts.sh <output-dir> [env-file]
#
# Writes the binaries into <output-dir> and appends `KEY=value` lines to
# <env-file> when given (GitHub Actions' $GITHUB_ENV), otherwise prints them.

OUT_DIR="${1:?usage: build-s2-native-artifacts.sh <output-dir> [env-file]}"
ENV_FILE="${2:-}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODULE_DIR="${REPO_ROOT}/internal/nativebody/nanollmprepare"

# The BAML/adapter version axes recorded in the attestation. They only have to be
# CONSISTENT between the stamp and the expected ID the proof asserts — the binary
# under proof is the real artifact either way — so a default keeps this runnable
# outside a full product build, and the env override keeps it honest inside one.
BAML_VERSION="${BAML_VERSION:-0.223.0}"
ADAPTER_VERSION="${ADAPTER_VERSION:-v0.219.0}"
ARTIFACT_SOURCE_REVISION="${ARTIFACT_SOURCE_REVISION:-unset}"
ARTIFACT_SOURCE_BUNDLE_DIGEST="${ARTIFACT_SOURCE_BUNDLE_DIGEST:-unset}"

ARTIFACT_PROFILE_PKG="github.com/invakid404/baml-rest/internal/artifactprofile"

# Canonicalize OUT_DIR to an ABSOLUTE path. build_artifact below runs `go build
# -o` from inside the isolated module (it must `cd` there for GOWORK=off to make
# that module's own go.mod authoritative), so a RELATIVE output path would be
# resolved against the module directory while mkdir -p above resolved it against
# the caller's. The binaries would land somewhere the exported
# BAML_REST_S2_NATIVE_*_BIN paths do not name, and the proof lane would fail on a
# missing file rather than on anything it is meant to prove.
mkdir -p "${OUT_DIR}"
OUT_DIR="$(cd "${OUT_DIR}" && pwd)"

emit() {
    if [ -n "${ENV_FILE}" ]; then
        printf '%s\n' "$1" >> "${ENV_FILE}"
    else
        printf '%s\n' "$1"
    fi
}

# build_artifact <cmd-package-dir> <build-tags> <bin-env-name> <id-env-name>
build_artifact() {
    local pkg_dir="$1" tags="$2" bin_env="$3" id_env="$4"
    local out="${OUT_DIR}/${pkg_dir}"

    # Same computation build.sh performs, from the repo root so the packaged-module
    # tar digest is read out of the real build context.
    local attest_out artifact_id artifact_inputs
    attest_out="$(cd "${REPO_ROOT}" && go run ./cmd/build/artifactattest \
        --profile native_capable \
        --worker-package "nanollmprepare:./cmd/${pkg_dir}/" \
        --build-tags "${tags}" \
        --subprocess true \
        --baml-version "${BAML_VERSION}" \
        --adapter-version "${ADAPTER_VERSION}" \
        --source-revision "${ARTIFACT_SOURCE_REVISION}" \
        --source-bundle-digest "${ARTIFACT_SOURCE_BUNDLE_DIGEST}")"
    artifact_id="$(printf '%s\n' "${attest_out}" | sed -n 's/^artifact_id=//p')"
    artifact_inputs="$(printf '%s\n' "${attest_out}" | sed -n 's/^artifact_inputs=//p')"
    if [ -z "${artifact_id}" ] || [ -z "${artifact_inputs}" ]; then
        echo "ERROR: artifactattest did not emit both artifact_id and artifact_inputs for cmd/${pkg_dir}" >&2
        printf '%s\n' "${attest_out}" >&2
        exit 1
    fi

    echo "Building native-capable artifact cmd/${pkg_dir} (tags=${tags}, artifact_id=${artifact_id})..."
    (
        cd "${MODULE_DIR}"
        GOWORK=off GOFLAGS=-mod=mod CGO_ENABLED=1 go build \
            -tags="${tags}" \
            -ldflags "-s -w -X '${ARTIFACT_PROFILE_PKG}.stampedProfile=native_capable' -X '${ARTIFACT_PROFILE_PKG}.stampedArtifactID=${artifact_id}' -X '${ARTIFACT_PROFILE_PKG}.stampedArtifactInputs=${artifact_inputs}'" \
            -o "${out}" "./cmd/${pkg_dir}"
    )

    # RETAINED SMOKE CHECK. The path emitted below is the one the proof lane
    # execs, so it must name a real, executable file. This is what catches the
    # relative-OUT_DIR class of defect at its source: if `go build -o` ever
    # resolves the output path against a different directory than the one this
    # script computed, the lane fails HERE with a clear message instead of
    # failing later on a missing binary that looks like a proof failure.
    if [ ! -x "${out}" ]; then
        echo "ERROR: built cmd/${pkg_dir} but no executable at ${out}" >&2
        echo "       (the emitted ${bin_env} would name a file that does not exist)" >&2
        exit 1
    fi

    emit "${bin_env}=${out}"
    emit "${id_env}=${artifact_id}"
}

# cmd/worker is the SERVE profile: build.sh gives it the nativestreamserve tag as
# well, because it is the only profile that installs the native stream serve
# factory. cmd/worker-shadow is the SHADOW profile: native-capable, but no stream
# serve factory, so no nativestreamserve tag. Both get nativeworkerartifact, which
# is what marks the artifact profile itself.
build_artifact worker        "subprocess,nativestreamserve,nativeworkerartifact" \
    BAML_REST_S2_NATIVE_WORKER_BIN BAML_REST_S2_NATIVE_WORKER_ARTIFACT_ID
build_artifact worker-shadow "subprocess,nativeworkerartifact" \
    BAML_REST_S2_NATIVE_SHADOW_WORKER_BIN BAML_REST_S2_NATIVE_SHADOW_WORKER_ARTIFACT_ID

echo "Built S2 native-capable artifacts into ${OUT_DIR}"
