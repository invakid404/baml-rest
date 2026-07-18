#!/usr/bin/env bash
#
# ci-use-warm-bases.sh — bind the warm-loaded mock-LLM base images to local
# aliases so testcontainers builds the mock Dockerfile with zero Docker Hub
# calls.
#
# Why this exists
# ---------------
# The warm-images job resolves each pinned base by its immutable digest,
# pulls it ONCE (authenticated), and `docker save`s it under its ordinary
# `repo:tag`. Consuming cells `docker load` that tarball. But `docker
# save`/`load` restores the image, its layers, and the `repo:tag` — it does
# NOT restore the repo-digest association (`RepoDigests` comes back empty).
#
# The mock Dockerfile pins its bases as digest-qualified `FROM repo:tag@sha256:…`
# references. With no local repo-digest mapping, the classic builder re-resolves
# that manifest against Docker Hub on every `FROM`, which is exactly the
# anonymous registry call that flakes with "registry-1.docker.io … context
# deadline exceeded" (#447 / #541).
#
# `github.com/moby/moby/client` rejects a digest-TARGET tag, so we cannot
# `docker tag <id> repo@sha256:…` to re-create the mapping, and a
# `docker pull repo@digest` would only restore it by making the very registry
# request that flakes. The Rust base already dodges this with a local-alias
# pattern (see the warm-images job's rust step); this helper mirrors it for the
# golang/alpine mock bases.
#
# What it does (fail-closed) — for each of `golang` and `alpine`:
#   1. require exactly one `^FROM <repo>:<tag>@sha256:<64hex>` in the mock
#      Dockerfile (same guard shape the warm job uses);
#   2. split it into `source_tag` (repo:tag), the 64-hex `digest`, and a
#      deterministic local alias `baml-rest-warm/mockllm-<repo>:<64hex-digest>`;
#   3. require `docker image inspect "$source_tag"` to succeed post-load and
#      capture its image ID (the loaded pinned artifact);
#   4. `docker tag <loaded-image-ID> "$alias"` — the source is the image ID, not
#      a digest-target ref (the docker client would reject the latter);
#   5. rewrite the ephemeral CI checkout of the Dockerfile so that FROM consumes
#      `$alias` instead of the `source_tag@sha256:<digest>` ref (line 2 keeps its
#      `AS builder` stage name);
#   6. assert alias ID == source ID.
# Finally it asserts the runtime Dockerfile contains exactly the two expected
# `baml-rest-warm/mockllm-*` FROMs and NO mock-base `sha256:` reference remains.
#
# The COMMITTED Dockerfile stays digest-pinned (Renovate + local builds are
# unaffected); this helper only rewrites the throwaway CI checkout in place.
#
# Usage:
#   integration/mockllm/ci-use-warm-bases.sh            # real CI path (needs docker)
#   integration/mockllm/ci-use-warm-bases.sh --dry-run  # parse+rewrite only, no docker
#
set -euo pipefail

DRY_RUN=false
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    *)
      echo "::error::unknown argument: $arg (supported: --dry-run)" >&2
      exit 2
      ;;
  esac
done

# The Dockerfile lives next to this script; resolve relative to the script so
# the helper works regardless of the caller's working directory (and so a local
# smoke test can run it against a throwaway copy).
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
DOCKERFILE="${SCRIPT_DIR}/Dockerfile"

if [ ! -f "$DOCKERFILE" ]; then
  echo "::error::mock Dockerfile not found at ${DOCKERFILE}" >&2
  exit 1
fi

# Escape a literal string for use as a BRE (sed search). `#` is the sed
# delimiter below, so it is escaped here too; the pinned refs contain none of
# these beyond the `.` in a tag, but keep it robust against future pins.
bre_escape() {
  printf '%s' "$1" | sed -e 's/[][\\.*^$#]/\\&/g'
}

bind_base() {
  local repo="$1"

  # Escape regex metacharacters in the repo so the grep -E patterns match it
  # literally (the repos here have none, but keep it robust — mirrors the warm
  # job's resolve step).
  local repo_re
  repo_re=$(printf '%s' "$repo" | sed -E 's/[][(){}.^$*+?|\\]/\\&/g')

  # Same guard as the warm job: exactly one pinned `FROM <repo>:<tag>@sha256:`.
  local matches
  matches=$(grep -cE "^FROM ${repo_re}:[^[:space:]]+@sha256:[0-9a-f]{64}" "$DOCKERFILE" || true)
  if [ "$matches" -ne 1 ]; then
    echo "::error file=${DOCKERFILE}::expected exactly one pinned 'FROM ${repo}:<tag>@sha256:' line, found ${matches}" >&2
    exit 1
  fi

  # ref = repo:tag@sha256:<64hex>; split into source_tag, digest, alias.
  # Anchor extraction to `^FROM ` too so it can only come from a validated FROM
  # line; `-o` prints the literal `FROM ` prefix, so strip it before splitting.
  local ref source_tag digest alias
  ref=$(grep -oE "^FROM ${repo_re}:[^[:space:]]+@sha256:[0-9a-f]{64}" "$DOCKERFILE" | head -n1)
  ref="${ref#FROM }"
  source_tag="${ref%@*}"
  digest="${ref##*@sha256:}"
  alias="baml-rest-warm/mockllm-${repo}:${digest}"

  # Sanity on the literal ref before touching docker: it must appear exactly
  # once in the file (fail closed on zero/multiple).
  local ref_count
  ref_count=$(grep -Fc -e "$ref" "$DOCKERFILE" || true)
  if [ "$ref_count" -ne 1 ]; then
    echo "::error file=${DOCKERFILE}::expected exactly one literal '${ref}' occurrence, found ${ref_count}" >&2
    exit 1
  fi

  echo "mockllm base ${repo}: source_tag=${source_tag} alias=${alias}"

  local source_id="<dry-run>"
  if [ "$DRY_RUN" = false ]; then
    # The loaded pinned artifact must be present as its ordinary tag.
    if ! source_id=$(docker image inspect "$source_tag" --format '{{.Id}}' 2>/dev/null); then
      echo "::error::loaded source image '${source_tag}' not found — was the warm tarball loaded before this step?" >&2
      exit 1
    fi

    # Diagnostic (NOT a gate): save/load does not restore repo-digest mappings,
    # so RepoDigests is expected to be empty even though the tag resolves — this
    # is the association the alias below stands in for. A future engine that DOES
    # restore it must not fail CI, hence diagnostic-only.
    local repo_digests
    repo_digests=$(docker image inspect "$source_tag" --format '{{json .RepoDigests}}' 2>/dev/null || echo '<inspect failed>')
    echo "[diagnostic] ${source_tag} id=${source_id} repoDigests=${repo_digests} (expected empty after save/load; not a gate)"

    # Tag the LOADED IMAGE ID to the alias. Source is the image ID, never a
    # digest-target ref (docker client rejects `docker tag … repo@sha256:…`).
    docker tag "$source_id" "$alias"

    local alias_id
    alias_id=$(docker image inspect "$alias" --format '{{.Id}}' 2>/dev/null || echo '')
    if [ "$alias_id" != "$source_id" ]; then
      echo "::error::alias '${alias}' id (${alias_id}) != loaded source '${source_tag}' id (${source_id})" >&2
      exit 1
    fi
    echo "tagged ${source_tag} (${source_id}) -> ${alias}"
  fi

  # Rewrite ONLY the `source_tag@sha256:<digest>` ref to the alias in the
  # ephemeral CI checkout; any trailing stage name (`AS builder`) is preserved.
  # Write via a temp then `cat` back so the Dockerfile keeps its original mode
  # and inode (a bare `mv` of a mktemp file would leave it mode 600).
  local search tmp
  search=$(bre_escape "$ref")
  tmp=$(mktemp)
  sed "s#${search}#${alias}#" "$DOCKERFILE" > "$tmp"
  cat "$tmp" > "$DOCKERFILE"
  rm -f "$tmp"

  # Post-rewrite, the alias must be present and the pinned ref gone.
  if ! grep -qF -e "$alias" "$DOCKERFILE"; then
    echo "::error file=${DOCKERFILE}::alias '${alias}' missing after rewrite" >&2
    exit 1
  fi
  if grep -qF -e "$ref" "$DOCKERFILE"; then
    echo "::error file=${DOCKERFILE}::pinned ref '${ref}' still present after rewrite" >&2
    exit 1
  fi
}

bind_base golang
bind_base alpine

# Structural gates on the rewritten runtime Dockerfile.
alias_count=$(grep -cE '^FROM baml-rest-warm/mockllm-' "$DOCKERFILE" || true)
if [ "$alias_count" -ne 2 ]; then
  echo "::error file=${DOCKERFILE}::expected exactly two 'FROM baml-rest-warm/mockllm-*' lines, found ${alias_count}" >&2
  exit 1
fi
if ! grep -qE '^FROM baml-rest-warm/mockllm-golang:[0-9a-f]{64} AS builder$' "$DOCKERFILE"; then
  echo "::error file=${DOCKERFILE}::missing expected 'FROM baml-rest-warm/mockllm-golang:<digest> AS builder'" >&2
  exit 1
fi
if ! grep -qE '^FROM baml-rest-warm/mockllm-alpine:[0-9a-f]{64}$' "$DOCKERFILE"; then
  echo "::error file=${DOCKERFILE}::missing expected 'FROM baml-rest-warm/mockllm-alpine:<digest>'" >&2
  exit 1
fi
# No digest-qualified base reference may remain — the aliases carry the digest
# as a plain tag, so the file must be free of any `@sha256:` reference (the
# exact form that would re-trigger a Docker Hub manifest pull).
sha_left=$(grep -cE '@sha256:' "$DOCKERFILE" || true)
if [ "$sha_left" -ne 0 ]; then
  echo "::error file=${DOCKERFILE}::found ${sha_left} leftover '@sha256:' reference(s) after rewrite" >&2
  exit 1
fi

echo "mock Dockerfile now consumes local warm aliases (no Docker Hub base pull):"
grep -nE '^FROM ' "$DOCKERFILE"
