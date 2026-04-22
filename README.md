## Runtime configuration

baml-rest reads the following environment variables at startup:

- `BAML_REST_USE_BUILD_REQUEST` — toggle the BuildRequest/StreamRequest code
  path. `1`/`true`/`yes`/`on` enables it; any other value (including empty)
  falls back to the legacy CallStream+OnTick path.
- `BAML_REST_BASE_URL_REWRITES` — semicolon-separated URL rewrite rules
  applied to LLM provider base URLs, both at build time and at runtime.
  Format: `from1=to1;from2=to2`. See
  [bamlutils/urlrewrite](bamlutils/urlrewrite/urlrewrite.go) for the full
  semantics.
- `BAML_REST_CLIENT_DEFAULTS` — JSON object pinning deployment-wide defaults
  for BAML `ClientRegistry` option values. Each key in `options` is merged
  into every caller's `client_registry.clients[].options` map unless the
  caller already set it. Example:

  ```json
  {
    "client_defaults": {
      "options": {
        "allowed_role_metadata": ["cache_control"]
      }
    }
  }
  ```

  v1 recognizes `allowed_role_metadata` only. See
  [bamlutils/clientdefaults](bamlutils/clientdefaults/clientdefaults.go) for
  the merge contract, supported opt-outs, and BuildRequest caveat.
- `BAML_LOG` — BAML internal log level (`debug`, `info`, `warn`, `error`).

## Known bugs/limitations

### Upstream

- [ ] Compiler error if prompt inputs collide with imported package names in the
      generated BAML client
      ([issue](https://github.com/BoundaryML/baml/issues/2393))
- [x] Dynamic classes that do not include any static fields aren't properly
      handled ([issue](https://github.com/BoundaryML/baml/issues/2432))
- [ ] BAML runtime leaks goroutines
      ([workaround](https://github.com/invakid404/baml-rest/blob/master/cmd/hacks/hacks/context_fix.go))
      ([issue](https://github.com/BoundaryML/baml/issues/2883))
- [x] LLM client serialization fails if options contain a nested map
      ([workaround](https://github.com/invakid404/baml-rest/commit/dead72721909a9b9ef47b0ffd025e58615ec23eb)
      applied) ([issue](https://github.com/BoundaryML/baml/issues/2767); fixed
      in >=0.215.0)
- [ ] Streaming API doesn't propagate dynamically created classes to the parser.
      When creating a new class via TypeBuilder and referencing it from an
      existing `@@dynamic` class, the streaming API fails with "Class X not
      found". The sync API and Parse API work correctly. (reported to BAML team)

### baml-rest

- [ ] Optional (nullable) media types (`image?`, `(image | null)[]`, etc.) are
      not supported on BAML versions prior to 0.215.0. The BAML runtime's
      encoder rejects `*Image` with `unsupported type for BAML encoding`.
      Non-optional media types and nil optional values work on all versions.

## Known broken versions

Recommended version: **v0.221.0**

The BuildRequest code path (`BAML_REST_USE_BUILD_REQUEST=true`) requires
BAML **v0.219.0** or newer. Older BAML versions remain functional via the
CallStream+OnTick path, but baml-rest logs a warning at startup when no
BuildRequest API is detected in the generated client.

- **v0.215.0**: Type builder is fully broken and panics the entire application
  when used ([issue](https://github.com/BoundaryML/baml/issues/2862))
- **≥v0.216.0 <0.218.0**: Various issues in dynamic field handling (e.g.,
  [issue](https://github.com/BoundaryML/baml/issues/2966))
