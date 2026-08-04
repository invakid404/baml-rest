// Generator for the de-BAML constraint EVALUATOR differential oracle.
//
// This project exists ONLY to produce a stock, untouched BAML v0.223.0
// generated Go client used as the oracle for the native constraint evaluator
// (internal/debaml.EvaluateConstraint / RenderConstraintExpression). The
// generated client under ../baml_client is an oracle artifact ONLY: it never
// enters the production build pipeline and is excluded from the customer/
// container source embed via .embedignore.
//
// Pinned to version 0.223.0 to match the exact stock github.com/boundaryml/baml
// test dependency the oracle links (asserted at test time by
// TestBAMLVersionPinned). Regenerate with the stock CLI only:
//   cd internal/debaml/testdata/constraint_oracle && \
//     npx @boundaryml/baml@0.223.0 generate
generator constraint_oracle {
  output_type "go"
  output_dir "../"
  version "0.223.0"
  default_client_mode async
  client_package_name "github.com/invakid404/baml-rest/internal/debaml/testdata/constraint_oracle"
}
