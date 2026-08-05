// Generator for the de-BAML guard-removal ledger's stock-CFFI proof harness.
//
// This project exists ONLY to produce a stock, untouched BAML v0.223.0
// generated Go client that records the STOCK outcome envelope of every ledger
// witness row. The generated client under ../baml_client is a proof artifact
// ONLY: it never enters the production build pipeline and is excluded from the
// customer/container source embed via .embedignore.
//
// Pinned to 0.223.0 to match the exact stock github.com/boundaryml/baml test
// dependency the harness links (asserted at test time by
// TestGuardLedgerBAMLVersionPinned). Regenerate with the stock CLI only:
//   cd internal/debaml/testdata/guard_ledger && \
//     npx @boundaryml/baml@0.223.0 generate
generator guard_ledger {
  output_type "go"
  output_dir "../"
  version "0.223.0"
  default_client_mode async
  client_package_name "github.com/invakid404/baml-rest/internal/debaml/testdata/guard_ledger"
}
