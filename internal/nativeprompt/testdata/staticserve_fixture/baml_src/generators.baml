// Generator for the de-BAML ctx-first STATIC SERVE fixture — the serve-side companion
// to the render-only static_oracle.
//
// UNLIKE static_oracle (a stock, never-sent differential oracle), this project's
// generated Go client under ../baml_client is DRIVEN end-to-end: after the ctx-first +
// lazy-runtime client hacks it backs the generated static /call SERVE seam that the
// de-BAML static serve e2e / cutover tests exercise over a loopback capture server (its
// StaticOracleClient base_url is the fixture's loopback, not a `.invalid` host). It is
// still a TEST-ONLY artifact — excluded from the customer/container source embed via
// .embedignore — and never ships in a production build.
//
// Pinned to version 0.223.0 to match the stock github.com/boundaryml/baml the serve
// tests link. Regenerate via scripts/regen-staticserve-fixture.sh (the documented,
// idempotent transform), or the stock CLI directly:
//   cd internal/nativeprompt/testdata/staticserve_fixture && \
//     npx @boundaryml/baml@0.223.0 generate
generator static_serve_fixture {
  output_type "go"
  output_dir "../"
  version "0.223.0"
  default_client_mode async
  client_package_name "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_fixture"
}
