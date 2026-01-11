module github.com/invakid404/baml-rest/workerplugin

go 1.24.5

require (
	github.com/hashicorp/go-plugin v1.6.3
	github.com/invakid404/baml-rest/bamlutils v0.0.0-00010101000000-000000000000
	google.golang.org/grpc v1.76.0
	google.golang.org/protobuf v1.36.10
)

replace github.com/invakid404/baml-rest/bamlutils => ../bamlutils
