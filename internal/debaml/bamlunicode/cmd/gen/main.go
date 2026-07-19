// Command gen deterministically regenerates the committed bamlunicode tables and
// provenance manifest from pinned UCD inputs.
//
// Modes:
//
//	go run ./internal/debaml/bamlunicode/cmd/gen          # write outputs
//	go run ./internal/debaml/bamlunicode/cmd/gen -check    # verify committed outputs are fresh (no writes)
//
// The generator NEVER downloads: it reads the SHA-256-pinned UCD text under
// testdata/ucd (kept out of the production embed bundle) and fails if any input
// hash drifts. Output is fully deterministic and independent of GOOS/GOARCH, the
// host Go version, and the network — so `-check` is a first-class CI gate.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Generator self-contained compatibility constants. Kept independent of the
// bamlunicode package so a broken/missing table cannot block regeneration.
// The emitted manifest cross-checks these against versions.go values in CI.
const (
	genRevision = "2"

	genBAMLVersion  = "0.223.0"
	genBAMLCommit   = "85247f452fe202300ec38d41fdc4035ea3020983"
	genRustcVersion = "1.93.0"
	genRustcCommit  = "254b59607d4417e9dffbc307138ae5c86280fe4c"
	genRustUnicode  = "17.0.0"
	genNormCrate    = "unicode-normalization"
	genNormVersion  = "0.1.24"
	genNormChecksum = "5033c97c4262335cded6d6fc3e5c18ab755e1a3dc96376350f3d8e9f009ad956"
	genNormUnicode  = "16.0.0"
	genMatchPath    = "engine/baml-lib/jsonish/src/deserializer/coercer/match_string.rs"
	genMatchFinger  = "6ced35949641470658930d60787d128f9ccf5bb9b058a2606b64c81f54d1d228"

	genReleaseWorkflowPath   = ".github/workflows/build-cli-release.reusable.yaml"
	genReleaseWorkflowFinger = "1aa156be43a455693245aad57be996673f03384f8045589550837ab6ef6c1ad2"
	genSetupRustPath         = ".github/actions/setup-rust/action.yml"
	genSetupRustFinger       = "31bdf1bf41065f8b1e429ffeef768b97d63ae7f5196be094c498057cadfcdfa3"

	// The pinned UCD.zip provenance. 16.0.0 is mirrored at the canonical
	// Public/zipped path; 17.0.0 is NOT published under Public/zipped (verified
	// 404), so its reproducible canonical URL is the versioned Public/<v>/ucd
	// path. Both are SHA-256-verified against the committed archives.
	ucd17ArchiveURL  = "https://www.unicode.org/Public/17.0.0/ucd/UCD.zip"
	ucd16ArchiveURL  = "https://www.unicode.org/Public/zipped/16.0.0/UCD.zip"
	ucd17ArchiveHash = "2066d1909b2ea93916ce092da1c0ee4808ea3ef8407c94b4f14f5b7eb263d28e"
	ucd16ArchiveHash = "c86dd81f2b14a43b0cc064aa5f89aa7241386801e35c59c7984e579832634eb2"
)

// ucdInput is a pinned UCD text file consumed by the generator (or, for
// NormalizationTest.txt, pinned as a conformance corpus).
type ucdInput struct {
	ver  string
	name string
	sha  string
	// parsed reports whether the generator reads this file (vs. hash-pinned only).
	parsed bool
}

var inputs = []ucdInput{
	{"17.0.0", "UnicodeData.txt", "2e1efc1dcb59c575eedf5ccae60f95229f706ee6d031835247d843c11d96470c", true},
	{"17.0.0", "SpecialCasing.txt", "efc25faf19de21b92c1194c111c932e03d2a5eaf18194e33f1156e96de4c9588", true},
	{"17.0.0", "DerivedCoreProperties.txt", "24c7fed1195c482faaefd5c1e7eb821c5ee1fb6de07ecdbaa64b56a99da22c08", true},
	{"17.0.0", "PropList.txt", "130dcddcaadaf071008bdfce1e7743e04fdfbc910886f017d9f9ac931d8c64dd", true},
	{"16.0.0", "UnicodeData.txt", "ff58e5823bd095166564a006e47d111130813dcf8bf234ef79fa51a870edb48f", true},
	{"16.0.0", "NormalizationTest.txt", "d811971453e7075e1ad56fb1b301eece5aa80757b81f6156e74a1bfb3ae5ceb1", false},
	{"16.0.0", "DerivedNormalizationProps.txt", "4d4c03892dea9146d674b686e495df2d55a28d071ac474041d73518f887abddc", false},
}

func ucdURL(ver, name string) string {
	return fmt.Sprintf("https://www.unicode.org/Public/%s/ucd/%s", ver, name)
}

// archiveSpecs are the pinned UCD.zip archives (committed under
// testdata/ucd/archives) that inputs are extracted from. Their SHA-256 is
// enforced in both generate and check mode.
var archiveSpecs = []struct {
	ver, file, sha string
}{
	{"17.0.0", "UCD-17.0.0.zip", ucd17ArchiveHash},
	{"16.0.0", "UCD-16.0.0.zip", ucd16ArchiveHash},
}

// outputFile is one generated Go table file emitted at the package root.
type outputFile struct {
	name  string
	build func(t *tables, source string) ([]byte, error)
	src   string // human-readable source description in the header
}

var outputFiles = []outputFile{
	{"lower_17_0_0_gen.go", emitLowerFile, "UCD 17.0.0 UnicodeData.txt + SpecialCasing.txt"},
	{"props_17_0_0_gen.go", emitPropsFile, "UCD 17.0.0 DerivedCoreProperties.txt + PropList.txt + UnicodeData.txt"},
	{"nfkd_16_0_0_gen.go", emitNFKDFile, "UCD 16.0.0 UnicodeData.txt"},
	{"marks_16_0_0_gen.go", emitMarksFile, "UCD 16.0.0 UnicodeData.txt"},
}

func main() {
	check := flag.Bool("check", false, "verify committed outputs are byte-for-byte fresh without writing")
	flag.Parse()

	pkgDir, ucdDir := resolveDirs()

	if err := run(pkgDir, ucdDir, *check); err != nil {
		fmt.Fprintf(os.Stderr, "bamlunicode/cmd/gen: %v\n", err)
		os.Exit(1)
	}
}

func resolveDirs() (pkgDir, ucdDir string) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot resolve generator source path")
	}
	genDir := filepath.Dir(thisFile)            // .../bamlunicode/cmd/gen
	pkgDir = filepath.Dir(filepath.Dir(genDir)) // .../bamlunicode
	ucdDir = filepath.Join(pkgDir, "testdata", "ucd")
	return pkgDir, ucdDir
}

func run(pkgDir, ucdDir string, check bool) error {
	// 1. Load + verify the pinned UCD archives. Extracting inputs from the
	//    verified archive enforces BOTH the archive digest and each extracted
	//    file digest, offline, in generate AND check mode.
	archives := map[string]*ucdArchive{}
	for _, a := range archiveSpecs {
		arch, err := loadArchive(ucdDir, a.file, a.sha)
		if err != nil {
			return err
		}
		archives[a.ver] = arch
	}

	// 2. Extract + hash-verify every pinned input file from its archive.
	hashes := map[string]string{}
	fileBytes := map[string][]byte{}
	parsedCount := 0
	for _, in := range inputs {
		arch, ok := archives[in.ver]
		if !ok {
			return fmt.Errorf("no archive for Unicode %s", in.ver)
		}
		data, err := arch.extract(in.name)
		if err != nil {
			return err
		}
		sum := sha256Hex(data)
		if sum != in.sha {
			return fmt.Errorf("input hash drift for %s/%s: manifest=%s actual=%s "+
				"(the pinned UCD archive contents changed)", in.ver, in.name, in.sha, sum)
		}
		hashes[in.ver+"/"+in.name] = sum
		fileBytes[in.ver+"/"+in.name] = data
		if in.parsed {
			parsedCount++
		}
	}
	fmt.Printf("bamlunicode: verified %d UCD archives + %d inputs (%d parsed, %d hash-pinned corpora)\n",
		len(archives), len(inputs), parsedCount, len(inputs)-parsedCount)

	get := func(ver, name string) []byte { return fileBytes[ver+"/"+name] }

	// 3. Parse the inputs the generator consumes.
	ud17, err := parseUnicodeData("17.0.0/UnicodeData.txt", get("17.0.0", "UnicodeData.txt"))
	if err != nil {
		return err
	}
	sc17, err := parseSpecialCasing("17.0.0/SpecialCasing.txt", get("17.0.0", "SpecialCasing.txt"))
	if err != nil {
		return err
	}
	dcp17 := get("17.0.0", "DerivedCoreProperties.txt")
	alpha, err := parseProperty("17.0.0/DerivedCoreProperties.txt", "Alphabetic", dcp17)
	if err != nil {
		return err
	}
	cased, err := parseProperty("17.0.0/DerivedCoreProperties.txt", "Cased", dcp17)
	if err != nil {
		return err
	}
	caseIgn, err := parseProperty("17.0.0/DerivedCoreProperties.txt", "Case_Ignorable", dcp17)
	if err != nil {
		return err
	}
	ws, err := parseProperty("17.0.0/PropList.txt", "White_Space", get("17.0.0", "PropList.txt"))
	if err != nil {
		return err
	}
	ud16, err := parseUnicodeData("16.0.0/UnicodeData.txt", get("16.0.0", "UnicodeData.txt"))
	if err != nil {
		return err
	}

	// 3. Build tables.
	single, full := buildLower(ud17, sc17)
	nfkdIndex, nfkdPool := buildNFKD(ud16)
	t := &tables{
		lowerSingle:     single,
		lowerFull:       full,
		alphabetic17:    alpha,
		numberGC17:      gcRanges(ud17, map[string]bool{"Nd": true, "Nl": true, "No": true}),
		cased17:         cased,
		caseIgnorable17: caseIgn,
		whiteSpace17:    ws,
		ccc16:           buildCCC(ud16),
		nfkdIndex:       nfkdIndex,
		nfkdPool:        nfkdPool,
		combining:       gcRanges(ud16, map[string]bool{"Mn": true, "Mc": true, "Me": true}),
	}

	// 4. Emit the four table files, capturing digests.
	rendered := map[string][]byte{}
	var outputDigests []ManifestFile
	for _, of := range outputFiles {
		data, err := of.build(t, of.src)
		if err != nil {
			return fmt.Errorf("emitting %s: %w", of.name, err)
		}
		rendered[of.name] = data
		outputDigests = append(outputDigests, ManifestFile{Name: of.name, SHA256: sha256Hex(data)})
	}
	sort.Slice(outputDigests, func(i, j int) bool { return outputDigests[i].Name < outputDigests[j].Name })

	// 5. Build the manifest (Go + JSON) from the same pinned data.
	manifest := buildManifest(hashes, outputDigests)
	manifestGo, err := emitManifestGo(manifest)
	if err != nil {
		return err
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestJSON = append(manifestJSON, '\n')
	rendered["manifest_gen.go"] = manifestGo

	// 6. Write or check.
	if check {
		return checkOutputs(pkgDir, rendered, manifestJSON)
	}
	return writeOutputs(pkgDir, rendered, manifestJSON)
}

func writeOutputs(pkgDir string, rendered map[string][]byte, manifestJSON []byte) error {
	for name, data := range rendered {
		if err := os.WriteFile(filepath.Join(pkgDir, name), data, 0o644); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "testdata", "manifest.json"), manifestJSON, 0o644); err != nil {
		return err
	}
	fmt.Printf("bamlunicode: wrote %d Go files + manifest.json\n", len(rendered))
	return nil
}

func checkOutputs(pkgDir string, rendered map[string][]byte, manifestJSON []byte) error {
	var stale []string
	cmp := func(rel string, want []byte) {
		got, err := os.ReadFile(filepath.Join(pkgDir, rel))
		if err != nil {
			stale = append(stale, fmt.Sprintf("%s: %v", rel, err))
			return
		}
		if string(got) != string(want) {
			stale = append(stale, rel+": committed content differs from freshly generated")
		}
	}
	for name, data := range rendered {
		cmp(name, data)
	}
	cmp(filepath.Join("testdata", "manifest.json"), manifestJSON)
	if len(stale) > 0 {
		sort.Strings(stale)
		return fmt.Errorf("generated outputs are stale; run `go generate ./internal/debaml/bamlunicode`:\n  %s",
			strings.Join(stale, "\n  "))
	}
	fmt.Println("bamlunicode: generated outputs are up to date")
	return nil
}

func buildManifest(hashes map[string]string, outputs []ManifestFile) ProfileManifest {
	var inFiles []ManifestFile
	for _, in := range inputs {
		inFiles = append(inFiles, ManifestFile{
			UnicodeVersion: in.ver,
			Name:           in.name,
			URL:            ucdURL(in.ver, in.name),
			SHA256:         hashes[in.ver+"/"+in.name],
		})
	}
	return ProfileManifest{
		BAMLVersion:                 genBAMLVersion,
		BAMLSourceCommit:            genBAMLCommit,
		RustcVersion:                genRustcVersion,
		RustcCommit:                 genRustcCommit,
		RustStdUnicodeVersion:       genRustUnicode,
		NormalizationCrate:          genNormCrate,
		NormalizationCrateVersion:   genNormVersion,
		NormalizationCrateChecksum:  genNormChecksum,
		NormalizationUnicodeVersion: genNormUnicode,
		MatchStringPath:             genMatchPath,
		MatchStringFingerprint:      genMatchFinger,
		ReleaseWorkflowPath:         genReleaseWorkflowPath,
		ReleaseWorkflowFingerprint:  genReleaseWorkflowFinger,
		SetupRustActionPath:         genSetupRustPath,
		SetupRustActionFingerprint:  genSetupRustFinger,
		GeneratorRevision:           genRevision,
		Inputs:                      inFiles,
		Archives: []ManifestArchive{
			{UnicodeVersion: "17.0.0", URL: ucd17ArchiveURL, SHA256: ucd17ArchiveHash},
			{UnicodeVersion: "16.0.0", URL: ucd16ArchiveURL, SHA256: ucd16ArchiveHash},
		},
		Outputs: outputs,
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
