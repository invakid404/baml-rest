// Generator for the de-BAML Phase 3 slice-2 static differential oracle.
//
// This project exists ONLY to produce a stock, untouched BAML v0.223.0
// generated Go client used as a differential oracle for the native static
// prompt renderer (internal/nativeprompt.RenderStatic / SupportsStatic). The
// generated client under ../baml_client is an oracle artifact ONLY: it never
// enters the production build pipeline and is excluded from the customer/
// container source embed via .embedignore.
//
// Pinned to version 0.223.0 to match the exact stock github.com/boundaryml/baml
// test dependency the oracle links (verified at test time via
// debug.ReadBuildInfo). Regenerate with the stock CLI only:
//   cd internal/nativeprompt/testdata/static_oracle && \
//     npx @boundaryml/baml@0.223.0 generate
generator static_prompt_oracle {
  output_type "go"
  output_dir "../"
  version "0.223.0"
  default_client_mode async
  client_package_name "github.com/invakid404/baml-rest/internal/nativeprompt/testdata/static_oracle"
}
