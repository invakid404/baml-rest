package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ucdArchive is a committed, SHA-256-verified UCD.zip. The archive is the single
// source of truth for the generator's inputs: every consumed UCD text file is
// extracted from the verified archive at generate/check time, so both the
// archive digest AND each extracted file digest are enforced offline (drift-guard
// check B / P2). No raw text file is committed separately.
type ucdArchive struct {
	file  string
	files map[string]*zip.File
}

// loadArchive reads testdata/ucd/archives/<file>, verifies its SHA-256 against
// the pinned digest, and indexes its entries.
func loadArchive(ucdDir, file, wantSHA string) (*ucdArchive, error) {
	raw, err := os.ReadFile(filepath.Join(ucdDir, "archives", file))
	if err != nil {
		return nil, fmt.Errorf("reading UCD archive %s: %w", file, err)
	}
	if sum := sha256Hex(raw); sum != wantSHA {
		return nil, fmt.Errorf("UCD archive hash drift for %s: manifest=%s actual=%s "+
			"(re-fetch the pinned UCD.zip and verify)", file, wantSHA, sum)
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("opening UCD archive %s: %w", file, err)
	}
	a := &ucdArchive{file: file, files: make(map[string]*zip.File)}
	for _, f := range zr.File {
		a.files[f.Name] = f
	}
	return a, nil
}

// extract returns the bytes of the named file at the archive root.
func (a *ucdArchive) extract(name string) ([]byte, error) {
	f, ok := a.files[name]
	if !ok {
		return nil, fmt.Errorf("%s not found at root of archive %s", name, a.file)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
