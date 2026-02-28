package hacks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/invakid404/baml-rest/bamlutils"
)

//go:embed patches/*.diff
var runtimePatchFS embed.FS

const (
	pr3185PatchV219Path         = "patches/pr3185_v219.diff"
	pr3185PatchV219Checksum     = "e52ff819519b29b34b350e5144286081ff3a60ed43a358fc65cf80a51ec664ed"
	pr3185PatchV219Provenance   = "BoundaryML/baml PR #3185"
	pr3185PatchV218BackportPath = "patches/pr3185_backport_v218.diff"
	pr3185PatchV218Checksum     = "fc234a28025f8b2d7cd807dfd898f0b026c07801321bb8d59ce1ed21956c68df"
	pr3185PatchV218Provenance   = "baml-rest backport aligned to BoundaryML/baml PR #3185"
	patchedModuleCopyMarkerName = ".patched_copy_complete"
	execTimeout                 = 30 * time.Second
	patchExecTimeout            = 2 * time.Minute
)

type embeddedPatchMetadata struct {
	checksum   string
	provenance string
}

var patchMetadataByPath = map[string]embeddedPatchMetadata{
	pr3185PatchV219Path: {
		checksum:   pr3185PatchV219Checksum,
		provenance: pr3185PatchV219Provenance,
	},
	pr3185PatchV218BackportPath: {
		checksum:   pr3185PatchV218Checksum,
		provenance: pr3185PatchV218Provenance,
	},
}

var (
	gnuPatchPath     string
	gnuPatchPathErr  error
	gnuPatchPathOnce sync.Once
)

// ApplyRuntimeDeadlockFix applies the PR #3185 runtime deadlock fix from
// BoundaryML/baml to installed Go runtime sources.
//
// This patch also improves stream cancellation callback cleanup behavior.
func ApplyRuntimeDeadlockFix(bamlVersion string) error {
	requestedVersion := strings.TrimSpace(bamlVersion)
	if requestedVersion != "" {
		requestedVersion = bamlutils.NormalizeVersion(requestedVersion)
	}

	resolvedVersion, err := bamlModuleVersion()
	if err != nil {
		return err
	}

	if requestedVersion != "" && bamlutils.CompareVersions(requestedVersion, resolvedVersion) != 0 {
		fmt.Printf("Requested BAML version %s differs from resolved module version %s; using resolved version for runtime-deadlock-fix\n", requestedVersion, resolvedVersion)
	}

	version := resolvedVersion
	if bamlutils.CompareVersions(version, "v0.218.0") < 0 {
		fmt.Printf("Skipping runtime-deadlock-fix (resolved version %s is below v0.218.0)\n", version)
		return nil
	}

	moduleDir, err := bamlModuleDir()
	if err != nil {
		return err
	}

	moduleDir, usingPatchedCopy, err := preparePatchedBamlModuleDir(moduleDir, version)
	if err != nil {
		return err
	}

	patchPath := pr3185PatchV218BackportPath
	if bamlutils.CompareVersions(version, "v0.219.0") >= 0 {
		patchPath = pr3185PatchV219Path
	}

	patchData, err := readEmbeddedPatch(patchPath)
	if err != nil {
		return err
	}

	applied, _, err := applyPatch(moduleDir, patchData)
	if err != nil {
		return fmt.Errorf("applying %s in %s: %w", filepath.Base(patchPath), moduleDir, err)
	}
	if usingPatchedCopy {
		if err := setGoWorkReplace("github.com/boundaryml/baml", moduleDir); err != nil {
			return err
		}
		fmt.Printf("  Using patched BAML module copy: %s\n", moduleDir)
	}

	if applied {
		fmt.Printf("  Applied patch: %s\n", filepath.Base(patchPath))
		return nil
	}

	fmt.Printf("  Patch already applied: %s\n", filepath.Base(patchPath))
	return nil
}

func readEmbeddedPatch(path string) ([]byte, error) {
	data, err := runtimePatchFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading embedded patch %s: %w", path, err)
	}
	if err := verifyEmbeddedPatchIntegrity(path, data); err != nil {
		return nil, err
	}
	return data, nil
}

func verifyEmbeddedPatchIntegrity(path string, data []byte) error {
	meta, ok := patchMetadataByPath[path]
	if !ok {
		return fmt.Errorf("missing embedded patch metadata for %s", path)
	}

	digest := sha256.Sum256(data)
	actualChecksum := fmt.Sprintf("%x", digest)
	if actualChecksum != meta.checksum {
		return fmt.Errorf(
			"embedded patch integrity check failed for %s (provenance: %s): expected sha256 %s, got %s",
			path,
			meta.provenance,
			meta.checksum,
			actualChecksum,
		)
	}

	return nil
}

func bamlModuleVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Version}}", "github.com/boundaryml/baml")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timed out resolving baml module version after %s", execTimeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("failed to resolve baml module version: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("failed to resolve baml module version: %w", err)
	}

	rawVersion := strings.TrimSpace(string(out))
	if rawVersion == "" {
		return "", fmt.Errorf("failed to resolve baml module version: go list returned empty version")
	}

	version := bamlutils.NormalizeVersion(rawVersion)

	return version, nil
}

func bamlModuleDir() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "list", "-m", "-f", "{{.Dir}}", "github.com/boundaryml/baml")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("timed out locating baml module dir after %s", execTimeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("failed to locate baml module dir: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("failed to locate baml module dir: %w", err)
	}

	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("failed to locate baml module dir: go list returned empty path")
	}

	return dir, nil
}

func preparePatchedBamlModuleDir(moduleDir, version string) (string, bool, error) {
	goModCache, err := goEnv("GOMODCACHE")
	if err != nil {
		return "", false, err
	}

	if !pathWithin(moduleDir, goModCache) {
		return moduleDir, false, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", false, fmt.Errorf("getting working directory: %w", err)
	}

	safeVersion, err := sanitizeVersionPathComponent(version)
	if err != nil {
		return "", false, err
	}

	patchedRoot := filepath.Join(cwd, ".baml_patched_modules")
	patchedDir := filepath.Join(patchedRoot, "github.com_boundaryml_baml_"+safeVersion)
	if !pathWithin(patchedDir, patchedRoot) {
		return "", false, fmt.Errorf("computed patched directory escapes patched root: %q", patchedDir)
	}

	copyMarkerPath := filepath.Join(patchedDir, patchedModuleCopyMarkerName)
	if _, err := os.Stat(copyMarkerPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := copyDir(moduleDir, patchedDir); err != nil {
				return "", false, fmt.Errorf("copying BAML module to patched directory: %w", err)
			}
			if err := writeAtomicFile(copyMarkerPath, []byte("ok\n"), 0o644); err != nil {
				return "", false, fmt.Errorf("marking patched BAML module copy complete: %w", err)
			}
		} else {
			return "", false, fmt.Errorf("checking patched BAML module directory: %w", err)
		}
	}

	return patchedDir, true, nil
}

func sanitizeVersionPathComponent(version string) (string, error) {
	component := strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if component == "" {
		return "", fmt.Errorf("invalid baml version %q: empty normalized version", version)
	}
	if strings.Contains(component, "..") {
		return "", fmt.Errorf("invalid baml version %q: must not contain '..'", version)
	}
	if strings.ContainsAny(component, `/\\`) {
		return "", fmt.Errorf("invalid baml version %q: must not contain path separators", version)
	}

	for _, r := range component {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '.', r == '-', r == '_', r == '+':
		default:
			return "", fmt.Errorf("invalid baml version %q: contains unsupported character %q", version, string(r))
		}
	}

	return component, nil
}

func goEnv(key string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "env", key)
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("go env %s timed out after %s", key, execTimeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("go env %s failed: %s", key, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("go env %s failed: %w", key, err)
	}

	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("go env %s returned empty value", key)
	}

	return value, nil
}

func setGoWorkReplace(modulePath, moduleDir string) error {
	workspaceFile, err := activeGoWorkFile()
	if err != nil {
		return err
	}
	if workspaceFile == "" {
		return fmt.Errorf("go.work file not found; run `go work init` at repository root before applying patched modules")
	}
	if _, err := os.Stat(workspaceFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("go.work file not found at %q; run `go work init` at repository root before applying patched modules", workspaceFile)
		}
		return fmt.Errorf("checking go.work file at %q: %w", workspaceFile, err)
	}

	arg := fmt.Sprintf("%s=%s", modulePath, moduleDir)
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "work", "edit", "-replace", arg)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("setting go.work replace for %s timed out after %s", modulePath, execTimeout)
		}
		return fmt.Errorf("setting go.work replace for %s: %v (%s)", modulePath, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func activeGoWorkFile() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "env", "GOWORK")
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("go env GOWORK timed out after %s", execTimeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("go env GOWORK failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("go env GOWORK failed: %w", err)
	}

	workspaceFile := strings.TrimSpace(string(out))
	if workspaceFile == "" || workspaceFile == "off" {
		return "", nil
	}

	return workspaceFile, nil
}

func pathWithin(path, parent string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absParent, err := filepath.Abs(parent)
	if err != nil {
		return false
	}

	if resolvedPath, err := filepath.EvalSymlinks(absPath); err == nil {
		absPath = resolvedPath
	}
	if resolvedParent, err := filepath.EvalSymlinks(absParent); err == nil {
		absParent = resolvedParent
	}

	rel, err := filepath.Rel(absParent, absPath)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func copyDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("removing existing destination directory: %w", err)
	}

	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dst, relPath)

		if info.IsDir() {
			dirPerm := info.Mode().Perm() | 0o700
			return os.MkdirAll(targetPath, dirPerm)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}

			resolvedTarget := linkTarget
			if !filepath.IsAbs(resolvedTarget) {
				resolvedTarget = filepath.Join(filepath.Dir(path), resolvedTarget)
			}
			resolvedTarget = filepath.Clean(resolvedTarget)
			if !pathWithin(resolvedTarget, src) {
				return fmt.Errorf("symlink target %q for %q escapes source directory %q", linkTarget, path, src)
			}

			if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
				return err
			}

			symlinkTarget := linkTarget
			if filepath.IsAbs(linkTarget) {
				relToSrc, err := filepath.Rel(src, resolvedTarget)
				if err != nil {
					return err
				}
				rewrittenAbs := filepath.Join(dst, relToSrc)
				rewrittenRel, err := filepath.Rel(filepath.Dir(targetPath), rewrittenAbs)
				if err != nil {
					return err
				}
				symlinkTarget = filepath.Clean(rewrittenRel)
			}

			return os.Symlink(symlinkTarget, targetPath)
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type %s for %q", info.Mode().Type(), path)
		}

		return copyFile(path, targetPath, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	perm := mode.Perm() | 0o200
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}

	if err := out.Close(); err != nil {
		return err
	}

	return nil
}

func writeAtomicFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(path), ".tmp-baml-marker-*")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(content); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Chmod(mode); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	return nil
}

func applyPatch(moduleDir string, patchData []byte) (applied bool, alreadyApplied bool, err error) {
	tmpFile, err := os.CreateTemp("", "baml-pr3185-deadlock-*.diff")
	if err != nil {
		return false, false, fmt.Errorf("creating temp patch file: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := tmpFile.Write(patchData); err != nil {
		tmpFile.Close()
		return false, false, fmt.Errorf("writing temp patch file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return false, false, fmt.Errorf("closing temp patch file: %w", err)
	}

	forwardDryRunCode, forwardDryRunOutput, err := runPatch(moduleDir, tmpPath, true, false)
	if err != nil {
		return false, false, err
	}

	if forwardDryRunCode == 0 {
		applyCode, applyOutput, err := runPatch(moduleDir, tmpPath, false, false)
		if err != nil {
			return false, false, err
		}
		if applyCode != 0 {
			return false, false, fmt.Errorf("patch apply failed:\n%s", strings.TrimSpace(applyOutput))
		}
		return true, false, nil
	}

	reverseDryRunCode, reverseDryRunOutput, err := runPatch(moduleDir, tmpPath, true, true)
	if err != nil {
		return false, false, err
	}
	if reverseDryRunCode == 0 {
		return false, true, nil
	}

	return false, false, fmt.Errorf(
		"patch does not apply cleanly\nforward dry-run:\n%s\n\nreverse dry-run:\n%s",
		strings.TrimSpace(forwardDryRunOutput),
		strings.TrimSpace(reverseDryRunOutput),
	)
}

func runPatch(moduleDir, patchPath string, dryRun, reverse bool) (exitCode int, output string, err error) {
	patchPathBinary, err := ensureGNUPatchBinary()
	if err != nil {
		return -1, "", err
	}

	args := []string{"-p1", "-d", moduleDir, "--batch"}
	if dryRun {
		args = append(args, "--dry-run")
	}
	if reverse {
		args = append(args, "--reverse")
	} else {
		args = append(args, "--forward")
	}
	args = append(args, "--input", patchPath)

	ctx, cancel := context.WithTimeout(context.Background(), patchExecTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, patchPathBinary, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if err == nil {
		return 0, out.String(), nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return -1, out.String(), fmt.Errorf("running patch command timed out after %s", patchExecTimeout)
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), out.String(), nil
	}

	return -1, out.String(), fmt.Errorf("running patch command: %w", err)
}

func ensureGNUPatchBinary() (string, error) {
	gnuPatchPathOnce.Do(func() {
		attemptErrors := make([]string, 0, 2)
		for _, binaryName := range []string{"patch", "gpatch"} {
			patchPathBinary, err := exec.LookPath(binaryName)
			if err != nil {
				attemptErrors = append(attemptErrors, fmt.Sprintf("%s: not found", binaryName))
				continue
			}

			ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
			versionCmd := exec.CommandContext(ctx, patchPathBinary, "--version")
			versionOut, versionErr := versionCmd.CombinedOutput()
			cancel()

			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				attemptErrors = append(attemptErrors, fmt.Sprintf("%s: timed out after %s", patchPathBinary, execTimeout))
				continue
			}
			if versionErr != nil {
				attemptErrors = append(attemptErrors, fmt.Sprintf("%s: %v (%s)", patchPathBinary, versionErr, strings.TrimSpace(string(versionOut))))
				continue
			}
			if !strings.Contains(strings.ToLower(string(versionOut)), "gnu patch") {
				attemptErrors = append(attemptErrors, fmt.Sprintf("%s: unsupported implementation (%s)", patchPathBinary, strings.TrimSpace(strings.SplitN(string(versionOut), "\n", 2)[0])))
				continue
			}

			gnuPatchPath = patchPathBinary
			return
		}

		gnuPatchPathErr = fmt.Errorf("GNU patch binary not found or unsupported; tried patch and gpatch (%s)", strings.Join(attemptErrors, "; "))
	})

	if gnuPatchPathErr != nil {
		return "", gnuPatchPathErr
	}
	return gnuPatchPath, nil
}
