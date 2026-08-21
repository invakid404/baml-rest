#!/usr/bin/env bash
set -euo pipefail

# Build the de-BAML serving-cutover S3a BOOTED-ARTIFACT FIXTURE: the SHIPPED
# native-capable serve-profile worker, plus one build tag that gives it a real
# `Baml_Rest_Dynamic` method table.
#
# WHY THIS EXISTS. The cutover's central claim is that a native-capable artifact,
# with the umbrella flag ON and the shipped EMPTY cohort policy, makes ZERO native
# claims on the DEPLOYED `/call` route. Proving that means sending a real request to
# a booted artifact — and an artifact built from a checkout cannot receive one:
# baml-rest's root `adapter.go` is the "overwritten during build" stub, so `Methods`
# is empty until the CONTAINER build generates a client from the deployment's own
# BAML project. That is why the S2 artifact proof boots the binaries but sends
# nothing.
#
# So this builds `internal/nativebody/nanollmprepare/cmd/worker` — the SAME
# entrypoint scripts/build-s2-native-artifacts.sh builds, with the SAME tags, the
# SAME -ldflags attestation stamp and the SAME GOWORK=off + CGO isolated-module
# build — and adds `debamlworkerfixture`, which swaps in dynclient's COMMITTED
# generated dynamic client (dynclient/workerruntime). One tag changes exactly one
# thing: which BAML methods exist. The serve-profile options, the native factories,
# the flag-first branch and the admission predicate are the shipped ones, by
# construction rather than by resemblance.
#
# WHAT IT DOES NOT CHANGE. The fixture tag confers no authority: it selects the
# method table, not what may be claimed natively, which remains the immutable cohort
# enrollment's answer — and that is empty. A fixture binary runs the same admission
# predicate as the shipped one, which is the whole reason the proof is worth
# anything.
#
# Usage:
#   scripts/build-s3a-fixture-artifact.sh <output-dir> [env-file]
#
# Writes the binary into <output-dir> and appends `KEY=value` lines to <env-file>
# when given (GitHub Actions' $GITHUB_ENV), otherwise prints them.

OUT_DIR="${1:?usage: build-s3a-fixture-artifact.sh <output-dir> [env-file]}"
ENV_FILE="${2:-}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODULE_DIR="${REPO_ROOT}/internal/nativebody/nanollmprepare"

BAML_VERSION="${BAML_VERSION:-0.223.0}"
ADAPTER_VERSION="${ADAPTER_VERSION:-v0.219.0}"
ARTIFACT_SOURCE_REVISION="${ARTIFACT_SOURCE_REVISION:-unset}"
ARTIFACT_SOURCE_BUNDLE_DIGEST="${ARTIFACT_SOURCE_BUNDLE_DIGEST:-unset}"

ARTIFACT_PROFILE_PKG="github.com/invakid404/baml-rest/internal/artifactprofile"

# The SHIPPED serve-profile tag set, stated here exactly as
# scripts/build-s2-native-artifacts.sh states it, plus the fixture tag. Keeping the
# base list literal (rather than deriving it) is deliberate: if the shipped set ever
# changes without this changing with it, the proof's own guard notices, because the
# fixture's stamped artifact inputs stop matching the shipped artifact's.
SHIPPED_TAGS="subprocess,nativestreamserve,nativeworkerartifact"
FIXTURE_TAGS="${SHIPPED_TAGS},debamlworkerfixture"

mkdir -p "${OUT_DIR}"
OUT_DIR="$(cd "${OUT_DIR}" && pwd)"
OUT="${OUT_DIR}/worker-s3a-fixture"

emit() {
    if [ -n "${ENV_FILE}" ]; then
        printf '%s\n' "$1" >> "${ENV_FILE}"
    else
        printf '%s\n' "$1"
    fi
}

# The attestation is computed for the SHIPPED tag set, not the fixture one: the
# fixture must present itself as the artifact it is a fixture OF, so the booted
# binary's profile/ID signal is the shipped artifact's and the proof is about that
# artifact rather than about a differently-stamped lookalike.
attest_out="$(cd "${REPO_ROOT}" && go run ./cmd/build/artifactattest \
    --profile native_capable \
    --worker-package "nanollmprepare:./cmd/worker/" \
    --build-tags "${SHIPPED_TAGS}" \
    --subprocess true \
    --baml-version "${BAML_VERSION}" \
    --adapter-version "${ADAPTER_VERSION}" \
    --source-revision "${ARTIFACT_SOURCE_REVISION}" \
    --source-bundle-digest "${ARTIFACT_SOURCE_BUNDLE_DIGEST}")"
artifact_id="$(printf '%s\n' "${attest_out}" | sed -n 's/^artifact_id=//p')"
artifact_inputs="$(printf '%s\n' "${attest_out}" | sed -n 's/^artifact_inputs=//p')"
if [ -z "${artifact_id}" ] || [ -z "${artifact_inputs}" ]; then
    echo "ERROR: artifactattest did not emit both artifact_id and artifact_inputs" >&2
    printf '%s\n' "${attest_out}" >&2
    exit 1
fi

echo "Building S3a booted-artifact fixture cmd/worker (tags=${FIXTURE_TAGS}, artifact_id=${artifact_id})..."
(
    cd "${MODULE_DIR}"
    GOWORK=off GOFLAGS=-mod=mod CGO_ENABLED=1 go build \
        -tags="${FIXTURE_TAGS}" \
        -ldflags "-s -w -X '${ARTIFACT_PROFILE_PKG}.stampedProfile=native_capable' -X '${ARTIFACT_PROFILE_PKG}.stampedArtifactID=${artifact_id}' -X '${ARTIFACT_PROFILE_PKG}.stampedArtifactInputs=${artifact_inputs}'" \
        -o "${OUT}" "./cmd/worker"
)

# The path emitted below is the one the proof lane execs, so it must name a real,
# executable file — the same retained smoke check the S2 script carries.
if [ ! -x "${OUT}" ]; then
    echo "ERROR: built the fixture but no executable at ${OUT}" >&2
    exit 1
fi

emit "BAML_REST_S3A_FIXTURE_WORKER_BIN=${OUT}"
emit "BAML_REST_S3A_FIXTURE_WORKER_ARTIFACT_ID=${artifact_id}"
