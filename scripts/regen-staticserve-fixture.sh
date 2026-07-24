#!/usr/bin/env bash
#
# regen-staticserve-fixture.sh
#
# Reproducible regeneration of the de-BAML STATIC differential/serve fixtures
# from their checked-in BAML source projects:
#
#   internal/nativeprompt/testdata/static_oracle       (stock oracle; has baml_src)
#   internal/nativeprompt/testdata/staticserve_fixture  (ctx-first serve fixture)
#
# Both are stock BAML v0.223.0 Go clients. static_oracle is UNTOUCHED stock;
# staticserve_fixture is the SAME source generated under a different
# client_package_name and then run through the ctx-first + lazy-runtime client
# hacks (cmd/hacks) so its Request/Parse methods are ctx-first — matching the
# generated static-serve adapter emission. Neither enters the production build;
# both are excluded from the customer/container embed via .embedignore.
#
# The de-BAML Phase 2 (recursive classes) slice added Node / A / B classes and
# StaticRecursiveNode/A/B methods to BOTH baml_src projects. This script is the
# documented, IDEMPOTENT transform that turns that source into the checked-in
# generated artifacts. A second run over the converged tree must leave `jj diff`
# empty. The staticserve_fixture drift guard
# (internal/nativeprompt/staticoracle-style fixture_drift_test analogues) re-runs
# the CGO-free portions and byte-compares.
#
# Requirements: npx (offline npm cache of @boundaryml/baml@0.223.0), goimports,
# gofmt, a Go toolchain, and the cached BAML CFFI (for the CGO adapter regen).
#
# Usage (from repo root):
#   scripts/regen-staticserve-fixture.sh
set -euo pipefail

BAML_VERSION="0.223.0"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Require the Go toolchain, then add GOPATH/bin so goimports lives on PATH. Capturing
# GOPATH in its own step means a failure surfaces here (a bare `$(go env GOPATH)` inside
# an `export PATH=...` assignment does NOT trip `set -e`), and the up-front tool checks
# give a clear error instead of an opaque "command not found" mid-run.
command -v go >/dev/null 2>&1 || { echo "error: 'go' toolchain not found on PATH" >&2; exit 1; }
GOPATH_DIR="$(go env GOPATH)"
export PATH="$PATH:$GOPATH_DIR/bin"
for tool in goimports gofmt npx; do
  command -v "$tool" >/dev/null 2>&1 || { echo "error: '$tool' not found on PATH (need go/goimports/gofmt/npx)" >&2; exit 1; }
done

TESTDATA="internal/nativeprompt/testdata"
SO="$TESTDATA/static_oracle"
SF="$TESTDATA/staticserve_fixture"

STRAY_TB="github.com/boundaryml/baml/engine/generators/languages/go/generated_tests/enums/baml_client/type_builder"

# regen_client <project_dir> — run stock BAML generate then gofmt/goimports.
# The generator's output_dir is "../" so it writes <project_dir>/baml_client.
regen_client() {
  local proj="$1"
  echo "==> [$proj] npx @boundaryml/baml@$BAML_VERSION generate"
  ( cd "$proj" && npx --offline "@boundaryml/baml@$BAML_VERSION" generate >/dev/null )
  goimports -w "$proj/baml_client"
  gofmt -w "$proj/baml_client"
}

# --- static_oracle: UNTOUCHED stock client (no ctx-first hacks) ---------------
regen_client "$SO"

CGO_ENABLED=0 go run ./cmd/introspect \
  --input-dir "$SO/baml_client" \
  --baml-src-dir "$SO/baml_src" \
  --output-dir "$SO/introspected" \
  --module-path "github.com/invakid404/baml-rest/$SO" \
  --interfaces-pkg "github.com/invakid404/baml-rest/bamlutils" \
  --baml-module-path "github.com/boundaryml/baml"

# --- staticserve_fixture: ctx-first + lazy-runtime client ---------------------
regen_client "$SF"

echo "==> [$SF] cmd/hacks (context-fix + lazy-runtime + …)"
go run ./cmd/hacks --skip-baml-module-patch \
  --baml-client-dir "$SF/baml_client" --baml-version "$BAML_VERSION"

# The BAML generator emits a stray absolute type_builder import
# (…/generated_tests/enums/baml_client/type_builder) whose TypeBuilder is a
# DISTINCT type from the fixture's own type_builder package — which makes the
# generated adapter's baml_client<->introspected TypeBuilder bridge fail to
# compile. Rewrite it to the fixture-local type_builder (what stock generation
# means), then re-format.
LOCAL_TB="github.com/invakid404/baml-rest/$SF/baml_client/type_builder"
# Portable in-place edit via a temp file: `sed -i ''` (empty backup suffix) is BSD/macOS
# syntax that GNU sed (Linux CI, contributors) misparses — the `''` becomes the sed
# SCRIPT and the real expression a FILENAME. Writing to a temp then mv works on both.
# `|| true` on grep: no match (a future BAML that stops emitting the stray import) is not
# a failure — there is simply nothing to rewrite (pipefail would otherwise abort).
grep -rl "$STRAY_TB" "$SF/baml_client" 2>/dev/null | while IFS= read -r f; do
  tmp="$(mktemp)"
  sed "s#$STRAY_TB#$LOCAL_TB#g" "$f" >"$tmp" && mv "$tmp" "$f"
done || true
goimports -w "$SF/baml_client"
gofmt -w "$SF/baml_client"

CGO_ENABLED=0 go run ./cmd/introspect \
  --input-dir "$SF/baml_client" \
  --baml-src-dir "$SF/baml_src" \
  --output-dir "$SF/introspected" \
  --module-path "github.com/invakid404/baml-rest/$SF" \
  --interfaces-pkg "github.com/invakid404/baml-rest/bamlutils" \
  --baml-module-path "github.com/boundaryml/baml"

echo "==> [$SF] cmd/gen-staticserve-fixture (generated serve adapter)"
( cd internal/nativebody/nanollmprepare && \
    GOWORK=off CGO_ENABLED=1 go run ./cmd/gen-staticserve-fixture -root ../../.. )

echo "==> done. A clean tree means the fixtures are converged/idempotent."
