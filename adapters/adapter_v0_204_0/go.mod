module github.com/invakid404/baml-rest/adapters/adapter_v0_204_0

go 1.26.0

require (
	github.com/boundaryml/baml v0.204.0
	github.com/invakid404/baml-rest/adapters/common v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/bamlutils v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/introspected v0.0.0-00010101000000-000000000000
)

replace (
	github.com/invakid404/baml-rest/adapters/common => ../common
	github.com/invakid404/baml-rest/bamlutils => ../../bamlutils
	github.com/invakid404/baml-rest/introspected => ../../introspected
)

require (
	github.com/alecthomas/participle/v2 v2.1.4 // indirect
	github.com/dave/jennifer v1.7.1 // indirect
	github.com/stoewer/go-strcase v1.3.1 // indirect
	golang.org/x/mod v0.32.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)
