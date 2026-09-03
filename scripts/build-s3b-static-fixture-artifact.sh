#!/usr/bin/env bash
set -euo pipefail

# Build the de-BAML serving-cutover S3b STATIC BOOTED-ARTIFACT FIXTURE: the SHIPPED
# native-capable serve-profile worker, plus the two build tags that give it a real
# STATIC (schema-defined) method table.
#
# WHY THIS EXISTS, and why the S3a fixture is not enough. The S3a fixture links
# dynclient's committed generated client, which supplies exactly ONE method —
# `Baml_Rest_Dynamic`. That is the surface the cutover ENROLLS, so it is what the
# positive proof needs. It leaves the cutover's STATIC-surface claim ("flag on, the
# fe-v1 enrollment present, a real static `/call` still declines pre-socket and BAML
# serves it byte-identically") with nothing on a booted artifact to send a request
# to: an artifact that knows no static method rejects a static call by NAME, before
# any route, adapter, factory or admission gate is reached, so a test written against
# it passes with nothing exercised. That is a false green, and a cold review caught
# it as one.
#
# internal/nativeprompt/testdata/staticserve_fixture is a real, compilable BAML
# project — its own baml_src/baml_client/introspected, and a generated adapter that
# carries the de-BAML STATIC serve seam. This script builds the SAME entrypoint
# scripts/build-s3a-fixture-artifact.sh builds, with the SAME shipped tag set, the
# SAME -ldflags attestation stamp and the SAME GOWORK=off + CGO isolated-module
# build, plus `debamlworkerfixture,debamlworkerstaticfixture`, which selects that
# project's method table instead of dynclient's.
#
# WHAT IT DOES NOT CHANGE. The fixture tags confer no authority: they select the
# method table, not what may be claimed natively, which remains the immutable cohort
# enrollment's answer — and that enrollment names the DYNAMIC unary call surface
# only. A fixture binary runs the same admission predicate as the shipped one, which
# is the whole reason driving a real static route through it proves anything.
#
# The fixture project's client bakes a FIXED loopback base_url (127.0.0.1:17654), so
# the proof binds a capture server on that exact port and the booted subprocess
# reaches it over loopback.
#
# Usage:
#   scripts/build-s3b-static-fixture-artifact.sh <output-dir> [env-file]
#
# Writes the binary into <output-dir> and appends `KEY=value` lines to <env-file>
# when given (GitHub Actions' $GITHUB_ENV), otherwise prints them.

OUT_DIR="${1:?usage: build-s3b-static-fixture-artifact.sh <output-dir> [env-file]}"
ENV_FILE="${2:-}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODULE_DIR="${REPO_ROOT}/internal/nativebody/nanollmprepare"
SIBLING="${REPO_ROOT}/scripts/build-s3a-fixture-artifact.sh"

BAML_VERSION="${BAML_VERSION:-0.223.0}"
ADAPTER_VERSION="${ADAPTER_VERSION:-v0.219.0}"
ARTIFACT_SOURCE_REVISION="${ARTIFACT_SOURCE_REVISION:-unset}"
ARTIFACT_SOURCE_BUNDLE_DIGEST="${ARTIFACT_SOURCE_BUNDLE_DIGEST:-unset}"

ARTIFACT_PROFILE_PKG="github.com/invakid404/baml-rest/internal/artifactprofile"

# The SHIPPED serve-profile tag set, stated literally for the same reason the S3a
# script states it literally, plus the two fixture tags.
SHIPPED_TAGS="subprocess,nativestreamserve,nativeworkerartifact,debamlnativespinegenerated"
FIXTURE_TAGS="${SHIPPED_TAGS},debamlworkerfixture,debamlworkerstaticfixture"

# DRIFT GUARD. Two scripts now build "the shipped artifact, with a different method
# table". If one script's idea of the shipped tag set moves and the other's does not,
# the two fixtures stop being fixtures of the SAME artifact and the static proof
# quietly becomes a proof about a lookalike. Read the sibling's literal and require
# it to agree, so that divergence is a build failure instead of a silent one.
sibling_tags="$(sed -n 's/^SHIPPED_TAGS="\(.*\)"$/\1/p' "${SIBLING}" | head -1)"
if [ -z "${sibling_tags}" ]; then
    echo "ERROR: could not read SHIPPED_TAGS from ${SIBLING}" >&2
    exit 1
fi
if [ "${sibling_tags}" != "${SHIPPED_TAGS}" ]; then
    echo "ERROR: shipped tag set drift — $(basename "${SIBLING}") says '${sibling_tags}', this script says '${SHIPPED_TAGS}'." >&2
    echo "       The dynamic and static booted fixtures must be fixtures of the SAME artifact." >&2
    exit 1
fi

mkdir -p "${OUT_DIR}"
OUT_DIR="$(cd "${OUT_DIR}" && pwd)"
OUT="${OUT_DIR}/worker-s3b-static-fixture"

emit() {
    if [ -n "${ENV_FILE}" ]; then
        printf '%s\n' "$1" >> "${ENV_FILE}"
    else
        printf '%s\n' "$1"
    fi
}

# The attestation is computed for the SHIPPED tag set, not the fixture one — same
# rule as the S3a script: the fixture must present itself as the artifact it is a
# fixture OF, so the proof is about that artifact rather than a differently-stamped
# lookalike. Both fixtures therefore stamp the SAME artifact id, which the proof
# asserts before claiming anything on the artifact's behalf.
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

# ExecBridge-U1c: the standard serve worker compiles the generated native spine registry.
# Generate it from the FIXTURE's own baml_src — the SAME project the
# debamlworkerstaticfixture tag selects — so the spine admits the exact-U1
# StaticRecursiveAliasJSON with the SAME baked plan (StaticOracleClient, loopback 17654)
# the fixture's BAML uses. That is what lets the live plan compare MATCH and native serve
# on the default-on acceptance leg. --allow-empty is harmless here (the project has a
# candidate) but keeps the standard-mode contract explicit.
FIXTURE_DESC="$(mktemp)"
(cd "${REPO_ROOT}" && go run ./cmd/introspect \
    --native-spine-descriptors "${FIXTURE_DESC}" \
    --baml-src-dir internal/nativeprompt/testdata/staticserve_fixture/baml_src)
echo "Generating native spine registry (static fixture: exact-U1 population from the fixture baml_src)..."
(cd "${REPO_ROOT}" && go run ./cmd/gen-native-spine-worker \
    --descriptors "${FIXTURE_DESC}" \
    --out-dir internal/nativebody/nanollmprepare/nativegenerated \
    --allow-empty)
rm -f "${FIXTURE_DESC}"

echo "Building S3b STATIC booted-artifact fixture cmd/worker (tags=${FIXTURE_TAGS}, artifact_id=${artifact_id})..."
(
    cd "${MODULE_DIR}"
    GOWORK=off GOFLAGS=-mod=mod CGO_ENABLED=1 go build \
        -tags="${FIXTURE_TAGS}" \
        -ldflags "-s -w -X '${ARTIFACT_PROFILE_PKG}.stampedProfile=native_capable' -X '${ARTIFACT_PROFILE_PKG}.stampedArtifactID=${artifact_id}' -X '${ARTIFACT_PROFILE_PKG}.stampedArtifactInputs=${artifact_inputs}'" \
        -o "${OUT}" "./cmd/worker"
)

# The path emitted below is the one the proof lane execs, so it must name a real,
# executable file.
if [ ! -x "${OUT}" ]; then
    echo "ERROR: built the static fixture but no executable at ${OUT}" >&2
    exit 1
fi

emit "BAML_REST_S3B_STATIC_FIXTURE_WORKER_BIN=${OUT}"
emit "BAML_REST_S3B_STATIC_FIXTURE_WORKER_ARTIFACT_ID=${artifact_id}"
