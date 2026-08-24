package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// TestListRunnableNamesAgainstTheRealToolchain is the one test in this
// package that shells out. Everything else runs against a synthetic tree,
// but the runnable universe is defined by `go test -list`'s own behaviour,
// and that is exactly what P0-1 got wrong: `-list '^Test'` hides Examples
// and Fuzz targets that `-run` would have selected. Asserting the filter in
// isolation cannot catch a wrong regexp handed to the toolchain, so this
// builds a throwaway module and asks the real `go`.
func TestListRunnableNamesAgainstTheRealToolchain(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a test binary; skipped under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/runnables\n\ngo 1.26.5\n")
	write("x.go", "package runnables\n\nimport \"fmt\"\n\n// Greet prints a greeting.\nfunc Greet() { fmt.Println(\"hi\") }\n")
	write("x_test.go", `package runnables

import "testing"

func TestGreet(t *testing.T) { Greet() }

func BenchmarkGreet(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Greet()
	}
}

func FuzzGreet(f *testing.F) {
	f.Fuzz(func(t *testing.T, s string) { _ = s })
}

func ExampleGreet() {
	Greet()
	// Output: hi
}

// ExampleGreet_second has no Output comment, so the test binary never
// registers it and -run cannot select it.
func ExampleGreet_second() { Greet() }
`)

	p := LivePackage{
		ImportPath: "example.com/runnables",
		Dir:        ".",
		Module:     ".",
		Mode:       modeOff, // resolve against this module alone, no go.work
		Atomic:     true,
		HasTests:   true,
	}
	got, err := listRunnableNames(dir, p)
	if err != nil {
		t.Fatalf("listRunnableNames: %v", err)
	}

	// Exactly what `go test -run` can select: the test, the fuzz target and
	// the runnable example. Not the benchmark (-bench selects those), and
	// not the example the binary never registers.
	want := []string{"ExampleGreet", "FuzzGreet", "TestGreet"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("runnable universe = %v, want %v", got, want)
	}
}
