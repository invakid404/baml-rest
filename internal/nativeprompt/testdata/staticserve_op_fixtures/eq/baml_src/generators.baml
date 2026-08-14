// Generator for the de-BAML Slice 7.2c-3 ISOLATED operator serve fixture `eq`.
//
// WHY THIS PROJECT EXISTS AT ALL. Slice 7.2c-3 widens the admitted static predicate
// from `this > I` to the six direct comparisons, and the live proof has to drive a
// REAL generated /call route per operator. One BAML project cannot declare
// `StaticCheckedAnswer` six times, and the 7.2c scope forbids renaming the classes to
// make the variants coexist — a renamed class is a different family with no stock
// capture behind it, and the main staticserve_fixture deliberately keeps a
// `StaticGtePredicateAnswer` sibling DECLINED so the name pin means something.
//
// So each operator gets an ISOLATED project that declares the two PINNED names ONCE
// with its own predicate. `>` is not here: it is the main staticserve_fixture's, which
// is unchanged.
//
// It is a TEST-ONLY artifact — excluded from the customer/container source embed via
// .embedignore — and never ships in a production build. Its own loopback PORT (17658)
// keeps it addressable independently of the other fixtures, so two operator packages
// can run concurrently and a request is attributable to the project that received it.
//
// Pinned to 0.223.0 to match the stock github.com/boundaryml/baml the serve tests link.
// Regenerate via scripts/regen-staticserve-fixture.sh.
generator static_serve_op_fixture_eq {
  output_type "go"
  output_dir "../"
  version "0.223.0"
  default_client_mode async
  client_package_name "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/staticserve_op_fixtures/eq"
}
