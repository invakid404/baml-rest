package main

// De-BAML Slice 7.2c-3 — the guard that keeps the out-of-`go.work` first-party PIN
// follow-up honest.
//
// # What problem this exists for
//
// `nativeserve` and `internal/nativebody/nanollmprepare` sit outside `go.work`, so an
// external consumer resolves their first-party requires as real pseudo-versions off the
// origin. `nativeserve/go.mod`'s BUMP RULE says those must land on a MASTER commit,
// because only master survives a squash-merge and a branch deletion — and it records the
// Slice 7.1b (#655) incident where a branch pin turned `nativeserve-goget` red once the
// branch was gone.
//
// That rule was a COMMENT. A comment cannot notice that the pins currently point at a
// branch commit, cannot notice a partial bump, and cannot notice that the follow-up was
// forgotten. This does all three, in the ordinary CGO-free `go test ./...` lane:
//
//	LOCKSTEP     all five selections name ONE commit. A partial bump is otherwise
//	             invisible until the out-of-work packaging build fails with
//	             "updates to go.mod needed", which is a long way from the edit.
//	FRESHNESS    nativeserve/pin_followup.md names exactly the commit the pins name,
//	             so the tracked record cannot drift away from what it describes.
//	COMPLETENESS the record enumerates exactly the five real (file, module) pairs, so
//	             the follow-up cannot be performed on four of them.
//	ANCESTRY     wherever a master ref is resolvable, STATUS must agree with actual
//	             master-reachability. That is what makes the follow-up self-clearing:
//	             the moment the re-pin lands on master this test goes RED until the
//	             record is flipped to RESOLVED.
//
// # The declared bound on ANCESTRY, logged rather than implied
//
// The ancestry clause needs a local `master` (or `origin/master`) ref. A shallow CI
// checkout of a PR head has neither, and this repository is developed in a jj workspace
// that is not colocated with a `.git` directory at all. When the ref cannot be resolved
// the clause is REPORTED AS UNDETERMINED and skipped — the other three still run, and
// they are the ones that catch an edit. A test that pretended to have checked
// reachability it could not observe would be the exact false green this file is against.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
)

// pinFollowupRelPath is the tracked record, relative to the repo root.
const pinFollowupRelPath = "nativeserve/pin_followup.md"

// firstPartyPin is one pinned selection: which manifest carries it and which module it
// selects.
//
// The five are written out here INDEPENDENTLY of both the manifests and the record, so
// the guard has its own idea of what "all five" means. A list derived from either side
// would shrink silently with it.
type firstPartyPin struct {
	file   string
	module string
}

func firstPartyPins() []firstPartyPin {
	const (
		nativeserveMod    = "nativeserve/go.mod"
		nanollmprepareMod = "internal/nativebody/nanollmprepare/go.mod"
		root              = "github.com/invakid404/baml-rest"
	)
	return []firstPartyPin{
		{nativeserveMod, root},
		{nativeserveMod, root + "/bamlutils"},
		{nativeserveMod, root + "/worker"},
		// nanollmprepare's own `github.com/invakid404/baml-rest v0.0.48` is NOT here: it
		// is a released TAG rather than a pseudo-version tracking a commit, so it does
		// not move with a bump and requiring it to would be wrong.
		{nanollmprepareMod, root + "/bamlutils"},
		{nanollmprepareMod, root + "/worker"},
	}
}

// pseudoVersionRe matches the `<stamp>-<sha12>` tail every first-party pseudo-version
// carries.
//
// The two BASE forms differ in their separator and it is easy to get wrong: the root
// module is pinned below an untagged base as `v0.0.0-<stamp>-<sha>`, while `bamlutils`
// and `worker` are pinned ABOVE a released tag as `v0.0.49-0.<stamp>-<sha>` — a DOT
// after the `-0`, not a dash. Both are matched here, and
// TestFirstPartyPinVersionParsingIsProvenToBite drives both alongside the near-misses.
var pseudoVersionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-0\.|-)([0-9]{14})-([0-9a-f]{12})$`)

// TestFirstPartyPinVersionParsingIsProvenToBite pins the pseudo-version parser against
// both real base forms and against the spellings next to them.
//
// It exists because the first draft of [pseudoVersionRe] got the `v0.0.49-0.` separator
// wrong and rejected three of the five real pins outright. That was caught by the guard
// itself rather than by review — which is the argument for the parser having its own
// control, since a parser that was merely too PERMISSIVE would have failed silently
// instead.
func TestFirstPartyPinVersionParsingIsProvenToBite(t *testing.T) {
	for _, tc := range []struct {
		version    string
		stamp, sha string
		ok         bool
	}{
		// The two REAL forms.
		{version: "v0.0.0-20260814142858-4168895ed76d", stamp: "20260814142858", sha: "4168895ed76d", ok: true},
		{version: "v0.0.49-0.20260814142858-4168895ed76d", stamp: "20260814142858", sha: "4168895ed76d", ok: true},
		// A released TAG is not a pseudo-version: it tracks no commit, so a pin that
		// became one would leave the out-of-work modules unable to move in lockstep.
		{version: "v0.0.48"},
		{version: "v0.0.49"},
		// Near-misses.
		{version: "v0.0.49-0-20260814142858-4168895ed76d"}, // dash where the dot belongs
		{version: "v0.0.0.20260814142858-4168895ed76d"},    // dot where the dash belongs
		{version: "v0.0.0-2026081414285-4168895ed76d"},     // 13-digit stamp
		{version: "v0.0.0-20260814142858-4168895ed76"},     // 11-hex sha
		{version: "v0.0.0-20260814142858-4168895ed76dd"},   // 13-hex sha
		{version: "v0.0.0-20260814142858-4168895ED76D"},    // upper-case sha
		{version: "v0.0.0-20260814142858-4168895ed76d+dirty"},
		{version: ""},
	} {
		m := pseudoVersionRe.FindStringSubmatch(tc.version)
		if (m != nil) != tc.ok {
			t.Errorf("pseudoVersionRe.Match(%q) = %v, want %v", tc.version, m != nil, tc.ok)
			continue
		}
		if !tc.ok {
			continue
		}
		if m[1] != tc.stamp || m[2] != tc.sha {
			t.Errorf("pseudoVersionRe.Match(%q) = (%q, %q), want (%q, %q)",
				tc.version, m[1], m[2], tc.stamp, tc.sha)
		}
	}
}

// TestFirstPartyPinFollowupIsTracked is the guard itself.
func TestFirstPartyPinFollowupIsTracked(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}

	// (1) LOCKSTEP — read the five real selections and require one commit.
	stamps, shas := map[string][]string{}, map[string][]string{}
	for _, pin := range firstPartyPins() {
		version := requireFirstPartyRequire(t, repoRoot, pin)
		m := pseudoVersionRe.FindStringSubmatch(version)
		if m == nil {
			t.Fatalf("%s selects %s %s, which is not a first-party pseudo-version; the pin rule tracks a "+
				"COMMIT, and a plain tag here would mean the out-of-work modules no longer move together",
				pin.file, pin.module, version)
		}
		stamps[m[1]] = append(stamps[m[1]], pin.file+" "+pin.module)
		shas[m[2]] = append(shas[m[2]], pin.file+" "+pin.module)
	}
	if len(shas) != 1 || len(stamps) != 1 {
		t.Fatalf("the five first-party selections do NOT move in lockstep — commits %v, stamps %v.\n"+
			"A partial bump is invisible to MVS (every one of these is directory-replaced for local "+
			"development, so only the version strings reach it) until the out-of-work packaging build "+
			"fails with \"updates to go.mod needed\".", shas, stamps)
	}
	var pinnedSHA, pinnedStamp string
	for sha := range shas {
		pinnedSHA = sha
	}
	for stamp := range stamps {
		pinnedStamp = stamp
	}

	// (2) FRESHNESS + STATUS + COMPLETENESS — the tracked record describes THESE pins.
	//
	// Routed through [pinFollowupViolations], which is the SAME function
	// TestFirstPartyPinFollowupGuardIsProvenToBite drives its mutants through. Spelling
	// these checks out again here would leave two implementations of one rule, and the
	// mutation controls would then protect the copy rather than the production guard —
	// so a later weakening of THIS test could leave the bite test green. One rule, one
	// implementation, both callers.
	record := readPinFollowup(t, repoRoot)
	for _, v := range pinFollowupViolations(record, pinnedSHA, pinnedStamp) {
		t.Errorf("%s: %s", pinFollowupRelPath, v)
	}
	status := record.fields["STATUS"]

	// (3) ANCESTRY — enforced wherever it can be observed, reported when it cannot.
	reachable, ref, ok := masterReachable(t, repoRoot, pinnedSHA)
	if !ok {
		t.Logf("pin follow-up: master-ancestry UNDETERMINED here (no resolvable master ref — a shallow "+
			"CI checkout of a PR head has none, and this repo is developed in a non-colocated jj "+
			"workspace). STATUS=%s, pins at %s. The lockstep/freshness/completeness clauses above still "+
			"ran; only this one was skipped.", status, pinnedSHA)
		return
	}
	switch {
	case reachable && status != "RESOLVED":
		t.Errorf("the pinned commit %s IS reachable from %s, but %s still records STATUS: %s.\n"+
			"The post-squash re-pin has happened (or the pins already pointed at master), so the "+
			"tracked follow-up must be closed — flip it to RESOLVED.", pinnedSHA, ref, pinFollowupRelPath, status)
	case !reachable && status != "OUTSTANDING":
		t.Errorf("the pinned commit %s is NOT reachable from %s, but %s records STATUS: %s.\n"+
			"A branch-only pin does not survive a squash-merge and branch deletion (the Slice 7.1b "+
			"failure nativeserve/go.mod's BUMP RULE records), so the follow-up is still OUTSTANDING.",
			pinnedSHA, ref, pinFollowupRelPath, status)
	default:
		t.Logf("pin follow-up: pinned commit %s, reachable from %s = %v, STATUS = %s — consistent",
			pinnedSHA, ref, reachable, status)
	}
}

// requireFirstPartyRequire returns the version one manifest selects for one module,
// failing if the require is absent.
func requireFirstPartyRequire(t *testing.T, repoRoot string, pin firstPartyPin) string {
	t.Helper()
	path := filepath.Join(repoRoot, filepath.FromSlash(pin.file))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", pin.file, err)
	}
	f, err := modfile.Parse(pin.file, data, nil)
	if err != nil {
		t.Fatalf("parse %s: %v", pin.file, err)
	}
	for _, r := range f.Require {
		if r.Mod.Path == pin.module {
			return r.Mod.Version
		}
	}
	t.Fatalf("%s no longer requires %s; the pin set this guard tracks has changed shape",
		pin.file, pin.module)
	return ""
}

// pinFollowupRecord is the parsed tracked record.
type pinFollowupRecord struct {
	fields map[string]string
	// enumerated holds the (file, module) pairs the record's table names, and rows is
	// how many table rows it had — so a duplicated row cannot stand in for a missing one.
	enumerated map[string]bool
	rows       int
}

func (r pinFollowupRecord) lists(file, module string) bool {
	return r.enumerated[file+"|"+module]
}

// pinFollowupFence is the minimum backtick run (three) that delimits the record's
// DESIGNATED metadata block. The keys the guard reads are taken from inside it and from
// nowhere else. The opening fence is a run of >= 3 backticks optionally followed by a
// CommonMark info string (a language tag); the closing fence is a BARE run of at least the
// opener's length (see parsePinFollowup / leadingBackticks).
const pinFollowupFence = "```"

// pinFollowupRequiredKeys are the metadata keys the guard reads. They must ALL be present
// in the designated block, and — because they decide whether a released serve core is
// pinned to a durable commit — a line that tries to set one of them anywhere else is an
// error rather than something to ignore.
func pinFollowupRequiredKeys() []string {
	return []string{"STATUS", "PINNED-COMMIT", "PINNED-STAMP"}
}

// readPinFollowup reads and STRICTLY parses the tracked record.
//
// It FAILS rather than skipping when the file is missing or unparseable. A guard that
// treated an absent or malformed record as "nothing to track" would pass exactly when the
// follow-up had been deleted or tampered with, which is the state it exists to prevent.
func readPinFollowup(t *testing.T, repoRoot string) pinFollowupRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(pinFollowupRelPath)))
	if err != nil {
		t.Fatalf("read %s: %v — the tracked pin follow-up is what records whether the released serve "+
			"core is pinned to a durable commit, so its absence is a failure, not a no-op",
			pinFollowupRelPath, err)
	}
	rec, perr := parsePinFollowup(string(data))
	if perr != nil {
		t.Fatalf("parse %s: %v", pinFollowupRelPath, perr)
	}
	return rec
}

// parsePinFollowup is the STRICT decoder for the tracked record, as a pure function of
// the document text so a malformed-input control can drive the SAME parser the production
// guard uses.
//
// # Why it is strict, and what a lenient version cost
//
// The first version scanned the WHOLE Markdown file for `KEY: value` lines and let a
// later one overwrite an earlier one. That was a DEMONSTRATED false green, not a
// hypothetical: appending a plain `STATUS: RESOLVED` after the fenced
// `STATUS: OUTSTANDING` made the production guard read RESOLVED and PASS — in a guard
// whose entire purpose is to stay red until the post-squash re-pin is really done. The
// review reproduced it end to end with the pins moved to a simulated master.
//
// So the contract is now:
//
//   - the metadata comes from the FIRST fenced block and from nowhere else;
//   - a duplicate key INSIDE that block is an error, never last-wins;
//   - a line that sets one of [pinFollowupRequiredKeys] anywhere OUTSIDE that block is an
//     error, not something silently ignored — including inside a second fenced block.
//     Ignoring it would defeat the attack but leave the file quietly saying two different
//     things, and a record that reads one way and parses another is exactly what this is
//     meant to prevent;
//   - every required key must be present.
//
// The selection TABLE is read from the whole document, which is safe for a different
// reason: it is checked by BOTH exact row count and per-pair presence, so neither an
// extra row nor a duplicate standing in for a missing one survives
// ([pinFollowupViolations]).
// leadingBackticks returns the length of the run of backtick characters at the start of s.
func leadingBackticks(s string) int {
	n := 0
	for n < len(s) && s[n] == '`' {
		n++
	}
	return n
}

func parsePinFollowup(src string) (pinFollowupRecord, error) {
	rec := pinFollowupRecord{fields: map[string]string{}, enumerated: map[string]bool{}}
	lines := strings.Split(src, "\n")

	// Locate the designated metadata block: the FIRST fenced region. Track the OPENING
	// fence's backtick-run LENGTH (>= 3, optionally followed by a CommonMark info string
	// such as ```text for markdownlint MD040), and require the CLOSING fence to be a BARE
	// run of AT LEAST that many backticks. A SHORTER bare run (e.g. a ``` line inside a
	// ```` block) does NOT close the region, so a longer opener can never be terminated
	// early and the guard can never parse metadata from the wrong region (CodeRabbit #3).
	// The block boundaries stay exact and the out-of-block key check below is unchanged, so
	// the metadata-hiding guard is not weakened.
	open, close, openLen := -1, -1, 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		nb := leadingBackticks(trimmed)
		if open < 0 {
			if nb >= 3 {
				open, openLen = i, nb
			}
			continue
		}
		// A closing fence is a BARE run (only backticks) of at least the opener's length.
		if nb >= openLen && strings.TrimLeft(trimmed, "`") == "" {
			close = i
			break
		}
	}
	if open < 0 {
		return rec, fmt.Errorf("no fenced metadata block (%s) — the guard reads STATUS/PINNED-COMMIT/"+
			"PINNED-STAMP from that block and from nowhere else", pinFollowupFence)
	}
	if close < 0 {
		return rec, fmt.Errorf("the fenced metadata block opened at line %d is never closed", open+1)
	}

	for n, line := range lines[open+1 : close] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		key, value, ok := strings.Cut(trimmed, ": ")
		if !ok || key == "" || key != strings.ToUpper(key) || strings.ContainsAny(key, " \t") {
			return rec, fmt.Errorf("metadata line %d (%q) is not a KEY: value pair; the designated "+
				"block carries metadata only", open+2+n, trimmed)
		}
		if prev, dup := rec.fields[key]; dup {
			return rec, fmt.Errorf("metadata key %q appears twice in the designated block (%q then "+
				"%q); a last-wins duplicate is how a stale record hides a live one", key, prev,
				strings.TrimSpace(value))
		}
		rec.fields[key] = strings.TrimSpace(value)
	}
	for _, key := range pinFollowupRequiredKeys() {
		if _, ok := rec.fields[key]; !ok {
			return rec, fmt.Errorf("the designated metadata block does not set %s", key)
		}
	}

	// Anywhere OUTSIDE the designated block — prose, a table, or a SECOND fenced block —
	// a line that sets one of the required keys is rejected. This is the demonstrated
	// attack, and it is refused loudly rather than ignored.
	for i, line := range lines {
		if i > open && i < close {
			continue
		}
		trimmed := strings.TrimSpace(line)
		for _, key := range pinFollowupRequiredKeys() {
			if strings.HasPrefix(trimmed, key+":") {
				return rec, fmt.Errorf("line %d (%q) sets %s OUTSIDE the designated metadata block; "+
					"the guard reads that key only from the block, so an out-of-fence line is either "+
					"an override attempt or a record that says two different things", i+1, trimmed, key)
			}
		}
	}

	// The selection table: | # | `file` | `module` | `version` |
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(trimmed, "|"), "|")
		if len(cells) != 4 {
			continue
		}
		file := strings.Trim(strings.TrimSpace(cells[1]), "`")
		module := strings.Trim(strings.TrimSpace(cells[2]), "`")
		if !strings.HasSuffix(file, "go.mod") || !strings.HasPrefix(module, "github.com/") {
			continue // the header row, or a table that is not the selection table
		}
		rec.enumerated[file+"|"+module] = true
		rec.rows++
	}
	return rec, nil
}

// masterReachable reports whether commit is an ancestor of a resolvable master ref.
//
// The third return is false when reachability could not be OBSERVED — no git directory,
// no master ref, or the object is absent from a shallow clone — which the caller reports
// rather than treating as either answer.
func masterReachable(t *testing.T, repoRoot, commit string) (reachable bool, ref string, ok bool) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		return false, "", false
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoRoot
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	if _, err := run("rev-parse", "--git-dir"); err != nil {
		return false, "", false
	}
	for _, candidate := range []string{"origin/master", "master"} {
		if _, err := run("rev-parse", "--verify", candidate+"^{commit}"); err != nil {
			continue
		}
		// The pinned object itself must be present, or `merge-base` would report a
		// misleading answer on a shallow clone.
		if _, err := run("rev-parse", "--verify", commit+"^{commit}"); err != nil {
			return false, "", false
		}
		cmd := exec.Command("git", "merge-base", "--is-ancestor", commit, candidate)
		cmd.Dir = repoRoot
		err := cmd.Run()
		if err == nil {
			return true, candidate, true
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, candidate, true // definitively NOT an ancestor
		}
		return false, "", false // git could not answer
	}
	return false, "", false
}

// TestFirstPartyPinFollowupGuardIsProvenToBite drives the record parser and the
// consistency rules against DELIBERATELY BROKEN records and requires each to be caught.
//
// Without it, "the follow-up is tracked" would be a claim about a file's existence
// rather than about a check that fires.
func TestFirstPartyPinFollowupGuardIsProvenToBite(t *testing.T) {
	const goodSHA, goodStamp = "4168895ed76d", "20260814142858"
	base := pinFollowupRecord{
		fields:     map[string]string{"STATUS": "OUTSTANDING", "PINNED-COMMIT": goodSHA, "PINNED-STAMP": goodStamp},
		enumerated: map[string]bool{},
	}
	for _, pin := range firstPartyPins() {
		base.enumerated[pin.file+"|"+pin.module] = true
		base.rows++
	}
	// CONTROL: the well-formed record agrees with the real pins, or every rejection
	// below could be for the wrong reason.
	if got := pinFollowupViolations(base, goodSHA, goodStamp); len(got) != 0 {
		t.Fatalf("the well-formed record already fails the consistency rules: %v", got)
	}
	for _, m := range []struct {
		name   string
		mutate func(*pinFollowupRecord)
	}{
		{"a stale PINNED-COMMIT", func(r *pinFollowupRecord) { r.fields["PINNED-COMMIT"] = "deadbeefcafe" }},
		{"a stale PINNED-STAMP", func(r *pinFollowupRecord) { r.fields["PINNED-STAMP"] = "20200101000000" }},
		{"an unknown STATUS", func(r *pinFollowupRecord) { r.fields["STATUS"] = "MAYBE" }},
		{"a dropped selection", func(r *pinFollowupRecord) {
			p := firstPartyPins()[3]
			delete(r.enumerated, p.file+"|"+p.module)
			r.rows--
		}},
		{"a duplicated row standing in for a missing one", func(r *pinFollowupRecord) {
			p := firstPartyPins()[4]
			delete(r.enumerated, p.file+"|"+p.module)
			// rows stays at five: the pair check is what catches this, not the count.
		}},
	} {
		t.Run(m.name, func(t *testing.T) {
			mutated := pinFollowupRecord{
				fields:     map[string]string{},
				enumerated: map[string]bool{},
				rows:       base.rows,
			}
			for k, v := range base.fields {
				mutated.fields[k] = v
			}
			for k, v := range base.enumerated {
				mutated.enumerated[k] = v
			}
			m.mutate(&mutated)
			if got := pinFollowupViolations(mutated, goodSHA, goodStamp); len(got) == 0 {
				t.Fatalf("the guard ACCEPTED %s; the tracked follow-up could then drift from the pins "+
					"it describes", m.name)
			}
		})
	}
}

// pinFollowupViolations is the record/pin consistency rule, factored out so it can be
// driven with a deliberately broken record and shown to report it.
func pinFollowupViolations(rec pinFollowupRecord, sha, stamp string) []string {
	var out []string
	if rec.fields["PINNED-COMMIT"] != sha {
		out = append(out, fmt.Sprintf("records PINNED-COMMIT %q but the five selections name %q; the "+
			"tracked follow-up has drifted from the pins it describes",
			rec.fields["PINNED-COMMIT"], sha))
	}
	if rec.fields["PINNED-STAMP"] != stamp {
		out = append(out, fmt.Sprintf("records PINNED-STAMP %q but the five selections carry %q",
			rec.fields["PINNED-STAMP"], stamp))
	}
	switch rec.fields["STATUS"] {
	case "OUTSTANDING", "RESOLVED":
	default:
		out = append(out, fmt.Sprintf("records STATUS %q, want OUTSTANDING or RESOLVED — the ANCESTRY "+
			"clause compares against those two, so any other value cannot be enforced",
			rec.fields["STATUS"]))
	}
	for _, pin := range firstPartyPins() {
		if !rec.lists(pin.file, pin.module) {
			out = append(out, fmt.Sprintf("does not enumerate the selection %s %s; a follow-up written "+
				"from it would re-pin only some of the five", pin.file, pin.module))
		}
	}
	if rec.rows != len(firstPartyPins()) {
		out = append(out, fmt.Sprintf("enumerates %d selections, want exactly %d", rec.rows, len(firstPartyPins())))
	}
	return out
}

// TestMasterReachableDiscriminates is the control for the ANCESTRY clause, and the
// reason it is worth having: in the development workspace and in a shallow CI checkout
// that clause reports UNDETERMINED and skips, so without a test that puts it in front of
// a real git repository it would be an unexercised branch claiming to enforce something.
//
// It builds a synthetic repository with a master commit and a branch-only commit on top,
// and requires all three answers: master-reachable, NOT master-reachable, and — for a
// directory that is not a git repository at all — undeterminable.
func TestMasterReachableDiscriminates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	git("init", "-q", "-b", "master")
	write("a.txt", "one\n")
	git("add", "a.txt")
	git("commit", "-q", "-m", "on master")
	onMaster := git("rev-parse", "HEAD")

	git("checkout", "-q", "-b", "feature")
	write("a.txt", "two\n")
	git("add", "a.txt")
	git("commit", "-q", "-m", "branch only")
	branchOnly := git("rev-parse", "HEAD")

	if onMaster == branchOnly {
		t.Fatal("the two synthetic commits are the same; this control discriminates nothing")
	}

	// (1) A commit that IS on master.
	reachable, ref, ok := masterReachable(t, dir, onMaster)
	if !ok {
		t.Fatal("master-ancestry was undeterminable in a real git repository with a master ref")
	}
	if ref != "master" {
		t.Errorf("resolved ref = %q, want master", ref)
	}
	if !reachable {
		t.Error("a commit ON master was reported as NOT reachable from it")
	}

	// (2) A commit that is BRANCH-ONLY — the state this whole record exists for.
	reachable, _, ok = masterReachable(t, dir, branchOnly)
	if !ok {
		t.Fatal("master-ancestry was undeterminable for the branch-only commit")
	}
	if reachable {
		t.Error("a BRANCH-ONLY commit was reported as reachable from master; the guard would then " +
			"demand the follow-up be closed while the pin is still not durable")
	}

	// (3) Not a git repository at all: UNDETERMINABLE, never a silent answer either way.
	if _, _, ok := masterReachable(t, t.TempDir(), onMaster); ok {
		t.Error("a directory that is not a git repository returned a definite reachability answer")
	}
	// (4) A commit git has never heard of is undeterminable too, rather than "not an
	// ancestor" — a shallow clone missing the object must not read as a durable No.
	if _, _, ok := masterReachable(t, dir, "0123456789ab"); ok {
		t.Error("an unknown object returned a definite reachability answer")
	}
}

// pinFollowupDoc is a minimal WELL-FORMED record, used as the base every malformed
// mutation below is derived from.
//
// It is deliberately not a copy of the real file: the negatives have to differ from the
// positive by exactly the property under test, and a 100-line document makes that hard
// to see.
const pinFollowupDoc = "# record\n" +
	"\n" +
	pinFollowupFence + "\n" +
	"STATUS: OUTSTANDING\n" +
	"PINNED-COMMIT: 4168895ed76d\n" +
	"PINNED-STAMP: 20260814142858\n" +
	"REACHABLE-FROM: some-branch\n" +
	pinFollowupFence + "\n" +
	"\n" +
	"| # | file | module | selection |\n" +
	"| --- | --- | --- | --- |\n" +
	"| 1 | `nativeserve/go.mod` | `github.com/invakid404/baml-rest` | `v0.0.0-x` |\n"

// TestPinFollowupParserHandlesLongerFence is the regression for CodeRabbit #3: the parser
// tracks the OPENING fence's backtick-run LENGTH and requires a matching-length BARE
// closer. A metadata block delimited by FOUR backticks must be read correctly — closed by
// its own four-backtick fence, never by a shorter (three-backtick) line — so a longer
// opener can never be terminated early into the wrong metadata region. Under the pre-fix
// parser (prefix-matched opener + bare-3 closer) this four-backtick block failed to parse
// with "never closed", because it searched only for a bare THREE-backtick close.
func TestPinFollowupParserHandlesLongerFence(t *testing.T) {
	const fence4 = "````"
	doc := "# record\n\n" +
		fence4 + "text\n" + // four-backtick opener + a CommonMark info string
		"STATUS: OUTSTANDING\n" +
		"PINNED-COMMIT: 4168895ed76d\n" +
		"PINNED-STAMP: 20260814142858\n" +
		fence4 + "\n" // four-backtick close — a bare three-backtick line would NOT close it
	rec, err := parsePinFollowup(doc)
	if err != nil {
		t.Fatalf("a four-backtick metadata block did not parse: %v", err)
	}
	if rec.fields["STATUS"] != "OUTSTANDING" || rec.fields["PINNED-COMMIT"] != "4168895ed76d" || rec.fields["PINNED-STAMP"] != "20260814142858" {
		t.Fatalf("four-backtick block parsed to %v", rec.fields)
	}

	// The complement: a FOUR-backtick opener closed only by a THREE-backtick line is NOT
	// closed (the shorter run cannot terminate the longer fence), so the block is
	// unterminated rather than truncated into the wrong region.
	tooShortClose := "# record\n\n" + fence4 + "\nSTATUS: OUTSTANDING\n" + pinFollowupFence + "\n"
	if _, err := parsePinFollowup(tooShortClose); err == nil || !strings.Contains(err.Error(), "never closed") {
		t.Fatalf("a four-backtick block closed only by three backticks: err = %v, want 'never closed'", err)
	}
}

// TestPinFollowupParserIsStrict is the PARSER-level mutation control, and it is paired
// with the strict decoder rather than optional: strict decoding without a biting control
// can regress silently, and a control that never drives the real decoder proves nothing
// about the document the guard actually reads.
//
// Every case drives [parsePinFollowup] — the SAME function [readPinFollowup] delegates
// to, so this cannot drift away from the production path the way a hand-built
// [pinFollowupRecord] can (which is what the previous version of the bite test did, and
// why the false green below survived).
//
// The first two negatives are the DEMONSTRATED attack and its sibling. The review
// reproduced the first end to end: with the fenced `STATUS: OUTSTANDING` retained, the
// pins moved to a simulated master and an out-of-fence `STATUS: RESOLVED` appended, the
// production guard PASSED and logged RESOLVED.
func TestPinFollowupParserIsStrict(t *testing.T) {
	// POSITIVE CONTROL FIRST. Without it every negative below could pass because the
	// parser rejects everything.
	rec, err := parsePinFollowup(pinFollowupDoc)
	if err != nil {
		t.Fatalf("the well-formed record did not parse: %v", err)
	}
	if rec.fields["STATUS"] != "OUTSTANDING" || rec.fields["PINNED-COMMIT"] != "4168895ed76d" {
		t.Fatalf("the well-formed record parsed to %v", rec.fields)
	}
	if rec.rows != 1 {
		t.Fatalf("the well-formed record parsed %d selection rows, want 1", rec.rows)
	}

	// THE REAL FILE is the other positive control: the strictness must not have broken
	// the record this guard exists for.
	repoRoot, rerr := filepath.Abs(filepath.Join("..", ".."))
	if rerr != nil {
		t.Fatalf("resolving repo root: %v", rerr)
	}
	data, rerr := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(pinFollowupRelPath)))
	if rerr != nil {
		t.Fatalf("read %s: %v", pinFollowupRelPath, rerr)
	}
	real, rerr := parsePinFollowup(string(data))
	if rerr != nil {
		t.Fatalf("the REAL %s no longer parses under the strict decoder: %v", pinFollowupRelPath, rerr)
	}
	for _, key := range pinFollowupRequiredKeys() {
		if real.fields[key] == "" {
			t.Errorf("the real record parsed no %s", key)
		}
	}

	for _, tc := range []struct {
		name string
		doc  string
		want string // a fragment the error must name
	}{{
		// (a) THE DEMONSTRATED ATTACK: an out-of-fence override appended after the block.
		name: "an out-of-fence STATUS override",
		doc:  pinFollowupDoc + "\nSTATUS: RESOLVED\n",
		want: "OUTSIDE the designated metadata block",
	}, {
		// (b) A DUPLICATE key inside the block — last-wins is how a stale record hides a
		// live one.
		name: "a duplicate STATUS inside the block",
		doc: strings.Replace(pinFollowupDoc, "STATUS: OUTSTANDING\n",
			"STATUS: OUTSTANDING\nSTATUS: RESOLVED\n", 1),
		want: "appears twice",
	}, {
		name: "a duplicate PINNED-COMMIT inside the block",
		doc: strings.Replace(pinFollowupDoc, "PINNED-COMMIT: 4168895ed76d\n",
			"PINNED-COMMIT: 4168895ed76d\nPINNED-COMMIT: deadbeefcafe\n", 1),
		want: "appears twice",
	}, {
		name: "an out-of-fence PINNED-COMMIT override",
		doc:  pinFollowupDoc + "\nPINNED-COMMIT: deadbeefcafe\n",
		want: "OUTSIDE the designated metadata block",
	}, {
		// A SECOND fenced block is still outside the DESIGNATED one, so it cannot shadow
		// it either — the attack with one more layer of disguise.
		name: "a second fenced block carrying STATUS",
		doc: pinFollowupDoc + "\n" + pinFollowupFence + "\nSTATUS: RESOLVED\n" +
			pinFollowupFence + "\n",
		want: "OUTSIDE the designated metadata block",
	}, {
		name: "an out-of-fence override BEFORE the block",
		doc:  "STATUS: RESOLVED\n" + pinFollowupDoc,
		want: "OUTSIDE the designated metadata block",
	}, {
		name: "no fenced block at all",
		doc:  "# record\n\nSTATUS: OUTSTANDING\nPINNED-COMMIT: 4168895ed76d\nPINNED-STAMP: 2026\n",
		want: "no fenced metadata block",
	}, {
		name: "an unterminated fence",
		doc:  "# record\n\n" + pinFollowupFence + "\nSTATUS: OUTSTANDING\n",
		want: "never closed",
	}, {
		name: "a missing required key",
		doc:  strings.Replace(pinFollowupDoc, "PINNED-STAMP: 20260814142858\n", "", 1),
		want: "does not set PINNED-STAMP",
	}, {
		name: "prose smuggled into the block",
		doc: strings.Replace(pinFollowupDoc, "STATUS: OUTSTANDING\n",
			"STATUS: OUTSTANDING\nthis block carries metadata only\n", 1),
		want: "is not a KEY: value pair",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePinFollowup(tc.doc)
			if err == nil {
				t.Fatalf("the parser ACCEPTED %s; the guard reads STATUS from whatever this returns, so "+
					"that is a false green in the post-squash re-pin workflow", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the parser rejected %s with %q, which does not name %q — the failure would "+
					"not say what is wrong with the record", tc.name, err, tc.want)
			}
		})
	}
}
