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

# rewrite_stray_type_builder <client_dir> <local_type_builder_import>
#
# The BAML generator emits a stray ABSOLUTE type_builder import
# (…/generated_tests/enums/baml_client/type_builder) whose TypeBuilder is a DISTINCT
# type from the fixture's own type_builder package — which makes the generated
# adapter's baml_client<->introspected TypeBuilder bridge fail to compile. Rewrite it
# to the fixture-local type_builder (what stock generation means).
#
# ERROR HANDLING, deliberately: `grep -rl` exits 1 when it matches NOTHING, which is a
# legitimate outcome (a future BAML that stops emitting the stray import), and exits
# >1 on a real error. Only status 1 is absorbed; a failing `sed`/`mv` propagates, so a
# rewrite that silently did not happen cannot let introspection and adapter generation
# continue over stale imports. (`|| true` on the whole pipeline, which this replaced,
# treated both the same.)
#
# Portable in-place edit via a temp file: `sed -i ''` (empty backup suffix) is
# BSD/macOS syntax that GNU sed (Linux CI, contributors) misparses — the `''` becomes
# the sed SCRIPT and the real expression a FILENAME. Writing to a temp then mv works
# on both.
rewrite_stray_type_builder() {
  local client_dir="$1" local_tb="$2"
  local matches status
  set +e
  matches="$(grep -rl "$STRAY_TB" "$client_dir" 2>/dev/null)"
  status=$?
  set -e
  if [ "$status" -eq 1 ]; then
    return 0   # no match: nothing to rewrite
  fi
  if [ "$status" -ne 0 ]; then
    echo "error: grep failed with status $status while scanning $client_dir" >&2
    return "$status"
  fi
  local f tmp
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    tmp="$(mktemp)"
    sed "s#$STRAY_TB#$local_tb#g" "$f" >"$tmp"
    mv "$tmp" "$f"
  done <<<"$matches"
}


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
# The dynamic-order-client hack ADDS ordered_map_static.go helper files (one per
# schema-type package that carries a recursive structural-alias map arm — the
# de-BAML Phase 3a JSON / JsonValue unions). BAML's codegen REFUSES to run when the
# output directory contains a file it did not itself generate, so a second regen
# would abort on these hack-added helpers. Remove them before regenerating so the
# fixture regeneration stays IDEMPOTENT (a clean second run). rm -f no-ops on a
# fresh tree where the helpers do not yet exist.
find "$SF/baml_client" -name ordered_map_static.go -delete
# Same reason for the bamlutils-checked-carrier bridge the hack ADDS to the types
# package (cmd/hacks/hacks/checked_carrier.go): BAML's generator would abort on a file
# it did not itself produce, so remove it before regenerating.
find "$SF/baml_client" -name checked_carrier_bridge.go -delete
regen_client "$SF"

echo "==> [$SF] cmd/hacks (context-fix + lazy-runtime + …)"
# The staticserve_fixture links the STOCK github.com/boundaryml/baml@v0.223.0
# runtime, which — unlike the patched dynclient fork — does NOT export
# DecodeToOrderedValue. A recursive structural-alias map arm (the de-BAML Phase 3a
# JSON / JsonValue unions) would otherwise be rewritten to a patched-only
# DecodeToOrderedValue call and fail to compile. BAML_HACKS_STOCK_STATIC_MAP_DECODE=1
# selects baml.DecodeToValue (present in stock) for that arm — byte-equivalent since
# the arm materialises a plain map[string]T and loses CFFI order either way. The
# default (unset) keeps DecodeToOrderedValue so the patched dynclient stays unchanged.
# BAML_HACKS_BAMLUTILS_CHECKED=1 re-points this client's generated
# `type Checked[T any] = baml.Checked[T]` alias at bamlutils.Checked (de-BAML Slice
# 7.2b-2), so every generated `@check`-bearing field resolves to the carrier whose
# sonic bytes are deterministic. It is opt-in per client: the DYNAMIC client has no
# constraint channel at all (DynamicOutputSchema cannot express a @check — the #572
# ceiling), so its alias is unreachable and stays stock.
BAML_HACKS_STOCK_STATIC_MAP_DECODE=1 BAML_HACKS_BAMLUTILS_CHECKED=1 \
  go run ./cmd/hacks --skip-baml-module-patch \
  --baml-client-dir "$SF/baml_client" --baml-version "$BAML_VERSION"

rewrite_stray_type_builder "$SF/baml_client" \
  "github.com/invakid404/baml-rest/$SF/baml_client/type_builder"
goimports -w "$SF/baml_client"
gofmt -w "$SF/baml_client"

CGO_ENABLED=0 go run ./cmd/introspect \
  --input-dir "$SF/baml_client" \
  --baml-src-dir "$SF/baml_src" \
  --output-dir "$SF/introspected" \
  --module-path "github.com/invakid404/baml-rest/$SF" \
  --interfaces-pkg "github.com/invakid404/baml-rest/bamlutils" \
  --baml-module-path "github.com/boundaryml/baml"

# --- de-BAML Slice 7.2c-3: the ISOLATED OPERATOR fixtures -------------------
#
# One project per direct comparison the cutover newly admits (`>` stays with the
# main fixture above). Each declares the two PRODUCTION-PINNED class names
# `StaticCheckedAnswer` / `StaticAssertAnswer` exactly once with its own predicate,
# because one BAML project cannot declare a class twice and the 7.2c scope forbids
# renaming the classes to make the six variants coexist.
#
# They take the SAME transform as the main fixture — stock generate, then the
# ctx-first + lazy-runtime + bamlutils-Checked hacks, then introspect — so the live
# routes they back are generated the way production generates, not hand-written.
# BAML_HACKS_BAMLUTILS_CHECKED=1 is load-bearing here: every one of these projects
# carries a `@check`, so without it the generated field would resolve to stock
# baml_go's Checked (whose sonic key order is not deterministic) instead of
# bamlutils.Checked.
OP_FIXTURES="ge lt le eq ne"
for op in $OP_FIXTURES; do
  OF="$TESTDATA/staticserve_op_fixtures/$op"
  # Same reason as the main fixture: BAML's generator aborts on a file it did not
  # itself produce, so the hack-added helpers are removed before regenerating.
  find "$OF/baml_client" -name ordered_map_static.go -delete 2>/dev/null || true
  find "$OF/baml_client" -name checked_carrier_bridge.go -delete 2>/dev/null || true
  regen_client "$OF"

  echo "==> [$OF] cmd/hacks (context-fix + lazy-runtime + bamlutils Checked)"
  BAML_HACKS_STOCK_STATIC_MAP_DECODE=1 BAML_HACKS_BAMLUTILS_CHECKED=1 \
    go run ./cmd/hacks --skip-baml-module-patch \
    --baml-client-dir "$OF/baml_client" --baml-version "$BAML_VERSION"

  rewrite_stray_type_builder "$OF/baml_client" \
    "github.com/invakid404/baml-rest/$OF/baml_client/type_builder"
  goimports -w "$OF/baml_client"
  gofmt -w "$OF/baml_client"

  CGO_ENABLED=0 go run ./cmd/introspect \
    --input-dir "$OF/baml_client" \
    --baml-src-dir "$OF/baml_src" \
    --output-dir "$OF/introspected" \
    --module-path "github.com/invakid404/baml-rest/$OF" \
    --interfaces-pkg "github.com/invakid404/baml-rest/bamlutils" \
    --baml-module-path "github.com/boundaryml/baml"
done

# The generated serve adapters, for the main fixture AND every operator fixture, in
# one invocation — the command imports each introspected package, so a fixture whose
# introspection failed to regenerate cannot silently be skipped here.
echo "==> cmd/gen-staticserve-fixture (generated serve adapters: main + $OP_FIXTURES)"
( cd internal/nativebody/nanollmprepare && \
    GOWORK=off CGO_ENABLED=1 go run ./cmd/gen-staticserve-fixture -root ../../.. )

echo "==> done. A clean tree means the fixtures are converged/idempotent."
