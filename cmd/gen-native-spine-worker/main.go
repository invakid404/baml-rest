package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/schemadescriptor"
)

func main() {
	var (
		descriptors string
		outDir      string
		packagePath string
		allowEmpty  bool
		empty       bool
	)
	flag.StringVar(&descriptors, "descriptors", "", "path to the projectdescriptor.Project JSON emitted by `cmd/introspect --native-spine-descriptors` (\"-\" for stdin)")
	flag.StringVar(&outDir, "out-dir", "", "output directory for the generated native spine registry (the extracted nanollmprepare/nativegenerated tree)")
	flag.StringVar(&packagePath, "package-path", defaultRegistryPackagePath, "import path the output directory is built at (base for per-method subpackage imports)")
	flag.BoolVar(&allowEmpty, "allow-empty", false, "permit an empty U1 population (the standard composite: NewExecutor is all-decline / all-BAML-fallback). Native-only builds omit this so a candidate-free project fails loud.")
	flag.BoolVar(&empty, "empty", false, "generate an all-decline (empty-population) registry from a minimal valid project — no --descriptors needed. For a standard build whose deployment has no static-unary method (implies --allow-empty).")
	flag.Parse()

	if outDir == "" {
		fmt.Fprintln(os.Stderr, "gen-native-spine-worker: --out-dir is required")
		flag.Usage()
		os.Exit(2)
	}
	if empty == (descriptors != "") {
		fmt.Fprintln(os.Stderr, "gen-native-spine-worker: exactly one of --descriptors or --empty is required")
		flag.Usage()
		os.Exit(2)
	}

	var data []byte
	if empty {
		allowEmpty = true
		// A minimal, VALID, candidate-free project descriptor built from the real version
		// constants (no hard-coding), so the standard composite links an all-decline
		// executor rather than the fail-loud stub.
		b, err := json.Marshal(projectdescriptor.Project{
			Version:                 projectdescriptor.Version,
			PromptDescriptorVersion: promptdescriptor.Version,
			SchemaVersion:           schemadescriptor.Version,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "gen-native-spine-worker: marshal empty project: %v\n", err)
			os.Exit(1)
		}
		data = b
	} else {
		b, err := readDescriptors(descriptors)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gen-native-spine-worker: %v\n", err)
			os.Exit(1)
		}
		data = b
	}
	if err := Generate(data, outDir, packagePath, allowEmpty); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func readDescriptors(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}
