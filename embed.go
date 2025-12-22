package baml_rest

import (
	"embed"
	"fmt"
	"path/filepath"
	"github.com/invakid404/baml-rest/adapters/adapter_v0_204_0"
	"github.com/invakid404/baml-rest/adapters/adapter_v0_215_0"
	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/introspected"
)

//go:embed Dockerfile.builder README.md adapter.go adapters cmd embed.go go.mod go.sum go.work go.work.sum
var source embed.FS

var Sources = make(map[string]embed.FS)

func init() {
	Sources["."] = source
	for key, value := range adapter_v0_204_0.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "adapters/adapter_v0_204_0", key))
		Sources[path] = value
	}
	for key, value := range adapter_v0_215_0.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "adapters/adapter_v0_215_0", key))
		Sources[path] = value
	}
	for key, value := range common.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "adapters/common", key))
		Sources[path] = value
	}
	for key, value := range bamlutils.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "bamlutils", key))
		Sources[path] = value
	}
	for key, value := range introspected.Sources {
		path := filepath.Clean(fmt.Sprintf("./%s/%s", "introspected", key))
		Sources[path] = value
	}
}
