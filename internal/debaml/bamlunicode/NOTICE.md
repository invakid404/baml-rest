# Unicode data provenance & license (bamlunicode)

The generated tables in this package (`*_gen.go`) are derived from the Unicode
Character Database (UCD). The UCD data is committed ONLY as the two pinned,
SHA-256-verified `UCD.zip` archives under `testdata/ucd/archives/` (kept out of
the production embed bundle via the repository `.embedignore`); each archive
carries its original upstream `ReadMe.txt` provenance/terms notice. The
generator and the always-on tests extract inputs from these verified archives —
no raw UCD text file is committed separately.

## License

The Unicode Character Database is © Unicode, Inc. It is used here under the
**Unicode License v3** (SPDX identifier: `Unicode-3.0`). Full terms of use and
license: <https://www.unicode.org/terms_of_use.html>.

"Unicode" and the Unicode Logo are registered trademarks of Unicode, Inc. in the
U.S. and other countries. This package is not endorsed by or affiliated with
Unicode, Inc.

## Provenance

This is the dual-version BAML 0.223.0 compatibility profile:

| Dataset | Unicode version | Source archive |
|---|---|---|
| Case + character properties (Rust std) | 17.0.0 | <https://www.unicode.org/Public/17.0.0/ucd/UCD.zip> |
| Normalization (unicode-normalization) | 16.0.0 | <https://www.unicode.org/Public/zipped/16.0.0/UCD.zip> |

(17.0.0 is not published under `Public/zipped/` — verified 404 — so its canonical
reproducible archive URL is the versioned `Public/17.0.0/ucd/UCD.zip` path. Both
archives are SHA-256-verified against the committed bytes.)

The exact upstream URLs and SHA-256 digests of every consumed UCD file, both
UCD archives, and every generated output are recorded in the machine-readable
manifest (`manifest_gen.go` / `testdata/manifest.json`) and re-verified by the
generator (`go run ./internal/debaml/bamlunicode/cmd/gen -check`) and the CI
Unicode-parity drift guard. See `versions.go` and `doc.go` for the full
compatibility key and the rationale for the dual-version pin.
