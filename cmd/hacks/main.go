package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/invakid404/baml-rest/cmd/hacks/hacks"
)

func main() {
	bamlClientDir := flag.String("baml-client-dir", "", "Path to the generated baml_client directory (required)")
	bamlVersion := flag.String("baml-version", "", "Current BAML version (required)")

	flag.Parse()

	if *bamlClientDir == "" {
		fmt.Fprintln(os.Stderr, "Error: --baml-client-dir is required")
		flag.Usage()
		os.Exit(1)
	}

	if *bamlVersion == "" {
		fmt.Fprintln(os.Stderr, "Error: --baml-version is required")
		flag.Usage()
		os.Exit(1)
	}

	// Verify the directory exists
	if _, err := os.Stat(*bamlClientDir); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: baml_client directory does not exist: %s\n", *bamlClientDir)
		os.Exit(1)
	}

	fmt.Printf("Applying hacks for BAML version %s...\n", *bamlVersion)

	if err := hacks.ApplyAll(*bamlVersion, *bamlClientDir); err != nil {
		fmt.Fprintf(os.Stderr, "Error applying hacks: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("All applicable hacks applied successfully")
}
