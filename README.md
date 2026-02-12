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

## Known broken versions

Recommended version: **v0.219.0**

- **v0.215.0**: Type builder is fully broken and panics the entire application
  when used ([issue](https://github.com/BoundaryML/baml/issues/2862))
- **≥v0.216.0 <0.218.0**: Various issues in dynamic field handling (e.g.,
  [issue](https://github.com/BoundaryML/baml/issues/2966))
