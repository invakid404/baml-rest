module github.com/invakid404/baml-rest/adapters/adapter_v0_204_0

go 1.24.5

require (
	github.com/boundaryml/baml v0.204.0
	github.com/dave/jennifer v1.7.1
	github.com/invakid404/baml-rest/adapters/common v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/bamlutils v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/introspected v0.0.0-00010101000000-000000000000
	github.com/stoewer/go-strcase v1.3.1
)

replace (
	github.com/invakid404/baml-rest/adapters/common => ../common
	github.com/invakid404/baml-rest/bamlutils => ../../bamlutils
	github.com/invakid404/baml-rest/introspected => ../../introspected
)

require (
	github.com/alecthomas/participle/v2 v2.1.4 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)
