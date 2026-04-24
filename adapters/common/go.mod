module github.com/invakid404/baml-rest/adapters/common

go 1.26.1

require (
	github.com/dave/jennifer v1.7.1
	github.com/invakid404/baml-rest/bamlutils v0.0.0-00010101000000-000000000000
	github.com/invakid404/baml-rest/introspected v0.0.0-00010101000000-000000000000
	github.com/stoewer/go-strcase v1.3.1
)

require (
	github.com/alecthomas/participle/v2 v2.1.4 // indirect
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/goccy/go-json v0.10.6 // indirect
	github.com/klauspost/compress v1.18.5 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/stretchr/testify v1.11.1 // indirect
	github.com/tidwall/gjson v1.18.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.70.0 // indirect
	golang.org/x/mod v0.35.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
)

replace (
	github.com/invakid404/baml-rest/bamlutils => ../../bamlutils
	github.com/invakid404/baml-rest/introspected => ../../introspected
)
