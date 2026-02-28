package hacks

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/invakid404/baml-rest/bamlutils"
)

//go:embed patches/*.diff
var runtimePatchFS embed.FS

const (
	pr3185PatchV219Path         = "patches/pr3185_v219.diff"
	pr3185PatchV218BackportPath = "patches/pr3185_backport_v218.diff"
	patchedModuleCopyMarkerName = ".patched_copy_complete"
)

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
	version := bamlutils.NormalizeVersion(bamlVersion)
	if bamlutils.CompareVersions(version, "v0.218.0") < 0 {
		fmt.Printf("Skipping runtime-deadlock-fix (not needed for version %s)\n", bamlVersion)
		return nil
	}

	moduleDir, err := bamlModuleDir()
	if err != nil {
		return err
	}

	moduleDir, err = preparePatchedBamlModuleDir(moduleDir, version)
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

	applied, alreadyApplied, err := applyPatch(moduleDir, patchData)
	if err != nil {
		return fmt.Errorf("applying %s in %s: %w", filepath.Base(patchPath), moduleDir, err)
	}

	if applied {
		fmt.Printf("  Applied patch: %s\n", filepath.Base(patchPath))
		return nil
	}

	if alreadyApplied {
		fmt.Printf("  Patch already applied: %s\n", filepath.Base(patchPath))
		return nil
	}

	return fmt.Errorf("patch %s made no changes", filepath.Base(patchPath))
}

func readEmbeddedPatch(path string) ([]byte, error) {
	data, err := runtimePatchFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading embedded patch %s: %w", path, err)
	}
	return data, nil
}

func bamlModuleDir() (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/boundaryml/baml")
	out, err := cmd.Output()
	if err != nil {
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

func preparePatchedBamlModuleDir(moduleDir, version string) (string, error) {
	goModCache, err := goEnv("GOMODCACHE")
	if err != nil {
		return "", err
	}

	if !pathWithin(moduleDir, goModCache) {
		return moduleDir, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting working directory: %w", err)
	}

	safeVersion, err := sanitizeVersionPathComponent(version)
	if err != nil {
		return "", err
	}

	patchedRoot := filepath.Join(cwd, ".baml_patched_modules")
	patchedDir := filepath.Join(patchedRoot, "github.com_boundaryml_baml_"+safeVersion)
	if !pathWithin(patchedDir, patchedRoot) {
		return "", fmt.Errorf("computed patched directory escapes patched root: %q", patchedDir)
	}

	copyMarkerPath := filepath.Join(patchedDir, patchedModuleCopyMarkerName)
	if _, err := os.Stat(copyMarkerPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if err := copyDir(moduleDir, patchedDir); err != nil {
				return "", fmt.Errorf("copying BAML module to patched directory: %w", err)
			}
			if err := writeAtomicFile(copyMarkerPath, []byte("ok\n"), 0o644); err != nil {
				return "", fmt.Errorf("marking patched BAML module copy complete: %w", err)
			}
		} else {
			return "", fmt.Errorf("checking patched BAML module directory: %w", err)
		}
	}

	if err := setGoWorkReplace("github.com/boundaryml/baml", patchedDir); err != nil {
		return "", err
	}

	fmt.Printf("  Using patched BAML module copy: %s\n", patchedDir)
	return patchedDir, nil
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
	cmd := exec.Command("go", "env", key)
	out, err := cmd.Output()
	if err != nil {
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
	cmd := exec.Command("go", "work", "edit", "-replace", arg)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("setting go.work replace for %s: %v (%s)", modulePath, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func activeGoWorkFile() (string, error) {
	cmd := exec.Command("go", "env", "GOWORK")
	out, err := cmd.Output()
	if err != nil {
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
			return os.MkdirAll(targetPath, info.Mode().Perm())
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
			return os.Symlink(linkTarget, targetPath)
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

	cmd := exec.Command(patchPathBinary, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()
	if err == nil {
		return 0, out.String(), nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), out.String(), nil
	}

	return -1, out.String(), fmt.Errorf("running patch command: %w", err)
}

func ensureGNUPatchBinary() (string, error) {
	gnuPatchPathOnce.Do(func() {
		patchPathBinary, err := exec.LookPath("patch")
		if err != nil {
			gnuPatchPathErr = fmt.Errorf("patch binary not found; install patch or run in an environment with patch available: %w", err)
			return
		}

		versionCmd := exec.Command(patchPathBinary, "--version")
		versionOut, versionErr := versionCmd.CombinedOutput()
		if versionErr != nil {
			gnuPatchPathErr = fmt.Errorf("failed to validate patch binary capabilities; GNU patch is required: %v (%s)", versionErr, strings.TrimSpace(string(versionOut)))
			return
		}
		if !strings.Contains(strings.ToLower(string(versionOut)), "gnu patch") {
			gnuPatchPathErr = fmt.Errorf("unsupported patch binary implementation detected; GNU patch is required (got: %s)", strings.TrimSpace(strings.SplitN(string(versionOut), "\n", 2)[0]))
			return
		}

		gnuPatchPath = patchPathBinary
	})

	if gnuPatchPathErr != nil {
		return "", gnuPatchPathErr
	}
	return gnuPatchPath, nil
}
