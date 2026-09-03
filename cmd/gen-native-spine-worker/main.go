package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	var (
		descriptors string
		outDir      string
		packagePath string
		allowEmpty  bool
	)
	flag.StringVar(&descriptors, "descriptors", "", "path to the projectdescriptor.Project JSON emitted by `cmd/introspect --native-spine-descriptors` (\"-\" for stdin)")
	flag.StringVar(&outDir, "out-dir", "", "output directory for the generated native spine registry (the extracted nanollmprepare/nativegenerated tree)")
	flag.StringVar(&packagePath, "package-path", defaultRegistryPackagePath, "import path the output directory is built at (base for per-method subpackage imports)")
	flag.BoolVar(&allowEmpty, "allow-empty", false, "permit an empty U1 population (the standard composite: NewExecutor is all-decline / all-BAML-fallback). Native-only builds omit this so a candidate-free project fails loud.")
	flag.Parse()

	if descriptors == "" || outDir == "" {
		fmt.Fprintln(os.Stderr, "gen-native-spine-worker: both --descriptors and --out-dir are required")
		flag.Usage()
		os.Exit(2)
	}

	data, err := readDescriptors(descriptors)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-native-spine-worker: %v\n", err)
		os.Exit(1)
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
