module github.com/invakid404/baml-rest/adapters/adapter_v0_219_0

go 1.26.2

require (
	github.com/boundaryml/baml v0.219.0
	github.com/invakid404/baml-rest/adapters/common v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/bamlutils v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/introspected v0.0.0-00010101000000-000000000000
)

require (
	github.com/alecthomas/participle/v2 v2.1.4 // indirect
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/dave/jennifer v1.7.1 // indirect
	github.com/goccy/go-json v0.10.6 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/stoewer/go-strcase v1.3.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.71.0 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
)

replace (
	github.com/invakid404/baml-rest/adapters/common => ../common
	github.com/invakid404/baml-rest/bamlutils => ../../bamlutils
	github.com/invakid404/baml-rest/introspected => ../../introspected
)
