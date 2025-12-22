module github.com/invakid404/baml-rest/adapters/adapter_v0_215_0

go 1.24.5

require (
	github.com/boundaryml/baml v0.215.0
	github.com/invakid404/baml-rest/adapters/common v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/bamlutils v0.0.0-00010101000000-000000000000
)

require (
	github.com/alecthomas/participle/v2 v2.1.4 // indirect
	github.com/dave/jennifer v1.7.1 // indirect
	github.com/invakid404/baml-rest/introspected v0.0.0-00010101000000-000000000000 // indirect
	github.com/stoewer/go-strcase v1.3.1 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
)

replace (
	github.com/invakid404/baml-rest/adapters/common => ../common
	github.com/invakid404/baml-rest/bamlutils => ../../bamlutils
	github.com/invakid404/baml-rest/introspected => ../../introspected
)
