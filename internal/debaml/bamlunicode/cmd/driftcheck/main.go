// Command driftcheck is the pure-Go half of the #555 Unicode-parity drift guard
// (checks A and D of the CI job). It re-derives BAML's Unicode compatibility
// profile from the pinned BAML SOURCE on every run and fails if any value drifts
// from internal/debaml/bamlunicode's committed manifest — so a BAML bump cannot
// merge while it silently changes the matcher's Unicode contract.
//
// Checks performed here:
//
//	A. Source/profile discovery
//	   - integration/baml_versions.json {source,latest} == manifest
//	   - root go.mod BAML pin == manifest.baml_version
//	   - go list -m -json Origin.Hash == manifest.baml_source_commit
//	   - engine/Cargo.lock unicode-normalization {version,checksum} == manifest
//	   - testdata/reference/Cargo.lock pins the SAME crate+checksum (our oracle
//	     is locked to BAML's normalization crate, not merely a compatible one)
//	D. Upstream matcher fingerprint
//	   - sha256(match_string.rs @ source commit) == manifest.match_string_fingerprint
//
// The rustc/char::UNICODE_VERSION and unicode_normalization::UNICODE_VERSION
// probes (which need the toolchain) are check A's remaining legs and run in the
// workflow via the Rust reference `versions` command; check B (generator
// freshness) is `cmd/gen -check`; check C (behavioral sentinels) is the
// unicode_rustref test suite.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

type manifest struct {
	BAMLVersion                string `json:"baml_version"`
	BAMLSourceCommit           string `json:"baml_source_commit"`
	RustcVersion               string `json:"rustc_version"`
	NormalizationCrate         string `json:"normalization_crate"`
	NormalizationCrateVersion  string `json:"normalization_crate_version"`
	NormalizationCrateChecksum string `json:"normalization_crate_checksum"`
	MatchStringPath            string `json:"match_string_path"`
	MatchStringFingerprint     string `json:"match_string_fingerprint"`
	ReleaseWorkflowPath        string `json:"release_workflow_path"`
	ReleaseWorkflowFingerprint string `json:"release_workflow_fingerprint"`
	SetupRustActionPath        string `json:"setup_rust_action_path"`
	SetupRustActionFingerprint string `json:"setup_rust_action_fingerprint"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\nUnicode-parity drift guard FAILED:\n  %v\n\n"+
			"A BAML bump changed the matcher's Unicode contract. Review the BAML source at the\n"+
			"new commit, update internal/debaml/bamlunicode (versions.go + regenerate tables),\n"+
			"rerun the differential proof, then update this manifest.\n", err)
		os.Exit(1)
	}
	fmt.Println("Unicode-parity drift guard: checks A + D passed")
}

func run() error {
	pkgDir := resolvePkgDir()
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(pkgDir))) // .../<repo>

	m, err := loadManifest(filepath.Join(pkgDir, "testdata", "manifest.json"))
	if err != nil {
		return err
	}

	var errs []string
	fail := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	// A1: integration/baml_versions.json
	if bv, err := loadBAMLVersions(filepath.Join(repoRoot, "integration", "baml_versions.json")); err != nil {
		fail("baml_versions.json: %v", err)
	} else {
		if bv.Source != m.BAMLSourceCommit {
			fail("baml_versions.json source %q != manifest %q", bv.Source, m.BAMLSourceCommit)
		}
		if bv.Latest != m.BAMLVersion {
			fail("baml_versions.json latest %q != manifest %q", bv.Latest, m.BAMLVersion)
		}
	}

	// A2: root go.mod BAML pin
	if pin, err := goModBAMLPin(filepath.Join(repoRoot, "go.mod")); err != nil {
		fail("go.mod: %v", err)
	} else if pin != "v"+m.BAMLVersion {
		fail("go.mod BAML pin %q != v%s", pin, m.BAMLVersion)
	}

	// A3: go list -m -json Origin.Hash — fail-closed. An error or an empty/
	// mismatched hash means we could not confirm the exact BAML source identity,
	// which must block the guard rather than pass silently.
	if hash, err := goListOriginHash(repoRoot, "github.com/boundaryml/baml@v"+m.BAMLVersion); err != nil {
		fail("go list Origin.Hash: %v", err)
	} else if hash == "" {
		fail("go list returned no Origin.Hash for github.com/boundaryml/baml@v%s (cannot confirm BAML source identity)", m.BAMLVersion)
	} else if hash != m.BAMLSourceCommit {
		fail("go list Origin.Hash %q != manifest %q", hash, m.BAMLSourceCommit)
	}

	// A6: BAML's own pinned Rust toolchain (rust-toolchain.toml @ source commit)
	// is the source of truth for the build toolchain — derive and validate it
	// rather than trusting a hardcoded CI value.
	if body, err := fetch(rawURL(m.BAMLSourceCommit, "rust-toolchain.toml")); err != nil {
		fail("fetch rust-toolchain.toml: %v", err)
	} else if ch := rustToolchainChannel(body); ch == "" {
		fail("rust-toolchain.toml: no [toolchain] channel found")
	} else if ch != m.RustcVersion {
		fail("BAML rust-toolchain.toml channel %q != manifest rustc_version %q", ch, m.RustcVersion)
	}

	// A6b: the BAML RELEASE workflow that actually builds libbaml_cffi
	// (cargo build --release -p baml_cffi). Fingerprint-pinned so a change to how
	// the released artifact is built (toolchain, cargo invocation, target flags)
	// fails the guard even if rust-toolchain.toml is untouched.
	if body, err := fetch(rawURL(m.BAMLSourceCommit, m.ReleaseWorkflowPath)); err != nil {
		fail("fetch release workflow %s: %v", m.ReleaseWorkflowPath, err)
	} else if sum := sha256Hex(body); sum != m.ReleaseWorkflowFingerprint {
		fail("release workflow %s fingerprint %s != manifest %s "+
			"(the released baml_cffi build changed — review before regenerating)", m.ReleaseWorkflowPath, sum, m.ReleaseWorkflowFingerprint)
	}

	// A6c: the setup-rust action the release workflow resolves. Fingerprint-pinned
	// AND its `toolchain` input default (the EFFECTIVE release toolchain) must
	// equal the manifest rustc_version. This derives the release toolchain from
	// the release path itself, so a divergence from rust-toolchain.toml is caught.
	if body, err := fetch(rawURL(m.BAMLSourceCommit, m.SetupRustActionPath)); err != nil {
		fail("fetch setup-rust action %s: %v", m.SetupRustActionPath, err)
	} else {
		if sum := sha256Hex(body); sum != m.SetupRustActionFingerprint {
			fail("setup-rust action %s fingerprint %s != manifest %s", m.SetupRustActionPath, sum, m.SetupRustActionFingerprint)
		}
		if tc := setupRustToolchainDefault(body); tc == "" {
			fail("setup-rust action: no `toolchain` input default found")
		} else if tc != m.RustcVersion {
			fail("setup-rust release toolchain default %q != manifest rustc_version %q "+
				"(the release build toolchain diverged)", tc, m.RustcVersion)
		}
	}

	// A4: engine/Cargo.lock unicode-normalization pin @ source commit
	lockURL := rawURL(m.BAMLSourceCommit, "engine/Cargo.lock")
	if body, err := fetch(lockURL); err != nil {
		fail("fetch engine/Cargo.lock: %v", err)
	} else {
		ver, sum := cargoLockCrate(body, m.NormalizationCrate)
		if ver != m.NormalizationCrateVersion {
			fail("engine/Cargo.lock %s version %q != manifest %q", m.NormalizationCrate, ver, m.NormalizationCrateVersion)
		}
		if sum != m.NormalizationCrateChecksum {
			fail("engine/Cargo.lock %s checksum %q != manifest %q", m.NormalizationCrate, sum, m.NormalizationCrateChecksum)
		}
	}

	// A5: our reference Cargo.lock pins the identical crate + checksum.
	refLock, err := os.ReadFile(filepath.Join(pkgDir, "testdata", "reference", "Cargo.lock"))
	if err != nil {
		fail("read reference Cargo.lock: %v", err)
	} else {
		ver, sum := cargoLockCrate(refLock, m.NormalizationCrate)
		if ver != m.NormalizationCrateVersion || sum != m.NormalizationCrateChecksum {
			fail("reference Cargo.lock pins %s %s/%s, manifest wants %s/%s",
				m.NormalizationCrate, ver, sum, m.NormalizationCrateVersion, m.NormalizationCrateChecksum)
		}
	}

	// D: match_string.rs source fingerprint @ source commit.
	msURL := rawURL(m.BAMLSourceCommit, m.MatchStringPath)
	if body, err := fetch(msURL); err != nil {
		fail("fetch match_string.rs: %v", err)
	} else {
		got := sha256Hex(body)
		if got != m.MatchStringFingerprint {
			fail("match_string.rs fingerprint %s != manifest %s "+
				"(the upstream matcher changed even if versions did not — semantic review required)",
				got, m.MatchStringFingerprint)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n  "))
	}
	return nil
}

func resolvePkgDir() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		panic("cannot resolve driftcheck source path")
	}
	// .../bamlunicode/cmd/driftcheck/main.go -> .../bamlunicode
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

func loadManifest(path string) (*manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

type bamlVersions struct {
	Source string `json:"source"`
	Latest string `json:"latest"`
}

func loadBAMLVersions(path string) (*bamlVersions, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bv bamlVersions
	if err := json.Unmarshal(data, &bv); err != nil {
		return nil, err
	}
	return &bv, nil
}

var goModPinRe = regexp.MustCompile(`(?m)^\s*github\.com/boundaryml/baml\s+(v[0-9][^\s/]*)`)

func goModBAMLPin(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := goModPinRe.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("no github.com/boundaryml/baml require found")
	}
	return string(m[1]), nil
}

func goListOriginHash(dir, mod string) (string, error) {
	// Bounded so a hung module proxy fails fast with a clear error instead of
	// stalling until the job-level timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-json", mod)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		// Fail-closed: the guard must confirm the exact BAML source, so an
		// inability to resolve the module is a failure, not a skip.
		return "", fmt.Errorf("go list -m -json %s: %w", mod, err)
	}
	var info struct {
		Origin struct {
			Hash string
		}
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return "", err
	}
	return info.Origin.Hash, nil
}

// rustToolchainChannel extracts the `channel = "..."` value from a
// rust-toolchain.toml body.
func rustToolchainChannel(body []byte) string {
	m := rustChannelRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return string(m[1])
}

var rustChannelRe = regexp.MustCompile(`(?m)^\s*channel\s*=\s*"([^"]+)"`)

// setupRustToolchainDefault extracts the default of the `toolchain` input from a
// setup-rust composite action.yml. `toolchain` is the first input, so the first
// `default: "..."` after it is its default.
func setupRustToolchainDefault(body []byte) string {
	m := setupRustDefaultRe.FindSubmatch(body)
	if m == nil {
		return ""
	}
	return string(m[1])
}

var setupRustDefaultRe = regexp.MustCompile(`(?s)toolchain:.*?default:\s*"([^"]+)"`)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// cargoLockCrate extracts a crate's version and checksum from a Cargo.lock.
func cargoLockCrate(lock []byte, name string) (version, checksum string) {
	blocks := strings.Split(string(lock), "[[package]]")
	target := `name = "` + name + `"`
	for _, b := range blocks {
		if !strings.Contains(b, target) {
			continue
		}
		for _, line := range strings.Split(b, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "version = "); ok {
				version = strings.Trim(v, `"`)
			}
			if c, ok := strings.CutPrefix(line, "checksum = "); ok {
				checksum = strings.Trim(c, `"`)
			}
		}
		return version, checksum
	}
	return "", ""
}

func rawURL(commit, path string) string {
	return fmt.Sprintf("https://raw.githubusercontent.com/BoundaryML/baml/%s/%s", commit, path)
}

// fetch GETs url with a bounded retry+backoff. fetch() gates several checks of a
// required, merge-blocking status; a single transient network blip or a 5xx/429
// must not fail the guard spuriously. Non-retryable responses (other 4xx, e.g. a
// genuine 404 for a moved file) fail fast — that is real drift, not a blip.
func fetch(url string) ([]byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	const attempts = 4
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 2 * time.Second) // 2s, 4s, 6s backoff
		}
		body, retryable, err := fetchOnce(client, url)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable {
			return nil, err
		}
	}
	return nil, fmt.Errorf("GET %s failed after %d attempts: %w", url, attempts, lastErr)
}

// fetchOnce performs one GET. retryable is true for transient failures (network
// errors, 5xx, 429).
func fetchOnce(client *http.Client, url string) (body []byte, retryable bool, err error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, true, err // network/timeout: transient
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		transient := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return nil, transient, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, err // truncated read: transient
	}
	return b, false, nil
}
