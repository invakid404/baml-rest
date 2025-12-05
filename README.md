## Known bugs/limitations

### Upstream

- [ ] Compiler error if prompt inputs collide with imported package names in the
      generated BAML client
      ([issue](https://github.com/BoundaryML/baml/issues/2393))
- [ ] Dynamic classes that do not include any static fields aren't properly
      handled ([issue](https://github.com/BoundaryML/baml/issues/2432))
- [ ] LLM client serialization fails if options contain a nested map
      ([workaround](https://github.com/invakid404/baml-rest/commit/dead72721909a9b9ef47b0ffd025e58615ec23eb)
      applied) ([issue](https://github.com/BoundaryML/baml/issues/2767))
