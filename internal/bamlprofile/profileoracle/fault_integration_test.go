//go:build integration

package profileoracle

// The FAULT leg of the stock-BAML differential.
//
// Most corpus rows render, and are compared byte-for-byte. A few do not: stock
// BAML v0.223 either raises a normal template error or hits an INTERNAL
// invariant failure. Those rows are compared by classified outcome instead
// (Row.Fault), because the de-BAML parity-decline rule says a native success
// where BAML fails is an out-do — the profile must fault in the same class, and
// a conservative rendered `false` is NOT green.
//
// The hard part is the panic class. Stock BAML's `unreachable!()` cannot be
// observed in-process at all:
//
//	thread 'tokio-runtime-worker' panicked at minijinja/src/value/mod.rs:660:42:
//	internal error: entered unreachable code
//
// The panic unwinds the tokio worker that owns the request, so the CFFI call
// never delivers a result and BuildRequest blocks in its select FOREVER
// (measured: a 600s test timeout, with the panic already on stderr). There is no
// recover() on the Go side — the panic is in Rust, across the FFI boundary — so
// an OutcomePanic row runs the stock leg in a SUBPROCESS, reads the panic off
// the child's stderr, and kills the child. A stock panic is therefore never
// converted into a rendered string; it is reported as what it is.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	baml "github.com/boundaryml/baml/engine/language_client_go/pkg"
)

const (
	// faultChildRowEnv names the row a helper-process invocation should run. Its
	// presence is what distinguishes a child from an ordinary test run.
	faultChildRowEnv = "PROFILE_ORACLE_FAULT_CHILD_ROW"
	// faultChildMarker prefixes the child's single stdout result line. A child that
	// panics in Rust never reaches it, which is precisely the signal.
	faultChildMarker = "PROFILEORACLE-OUTCOME"
	// faultChildHarnessKind is the marker kind the child emits when a STOCK-side
	// HARNESS step fails (CreateRuntime, arg encode, body/text/decode, or the
	// one-system-message shape invariant) rather than the engine faulting. It is
	// deliberately NOT an OutcomeKind: the parent fails LOUD on it instead of
	// classifying it, so a harness bug can never masquerade as an engine outcome.
	faultChildHarnessKind = "harness-failure"
	// faultChildTimeout bounds a child. A healthy child answers in well under a
	// second once the CFFI runtime is built; a panicking one hangs forever, so this
	// is the deadline that turns the hang into a verdict rather than a stuck suite.
	faultChildTimeout = 90 * time.Second
	// rustPanicNeedle is stock Rust's panic banner on stderr. It is the POSITIVE
	// evidence that a hung child hung because the engine panicked — without it, a
	// timeout is just a timeout and the harness must not claim a panic.
	rustPanicNeedle = "panicked at"
)

// TestProfileOracleFaultChild is the helper process for an OutcomePanic row. It
// is inert in a normal run; a parent invokes it with faultChildRowEnv set, via
// the very test binary it is already running in.
//
// It renders exactly one row through stock BAML and prints ONE marker line to
// stdout. If the Rust engine panics, the process never gets here and the parent
// classifies from stderr instead.
func TestProfileOracleFaultChild(t *testing.T) {
	id := os.Getenv(faultChildRowEnv)
	if id == "" {
		t.Skip("helper process for the fault leg; runs only with " + faultChildRowEnv + " set")
	}
	row, ok := rowByID(id)
	if !ok {
		fmt.Printf("%s unknown-row %s\n", faultChildMarker, strconv.Quote(id))
		return
	}
	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", GenerateBAMLSource([]Row{row}), env)
	if err != nil {
		// CreateRuntime is HARNESS setup, not the engine render.
		fmt.Printf("%s %s %s\n", faultChildMarker, faultChildHarnessKind, strconv.Quote("CreateRuntime: "+err.Error()))
		return
	}
	msgs, err := bamlRenderErr(&rt, row, env)
	if err != nil {
		// Only a genuine ENGINE error (rt.BuildRequest) is an `error` marker, which
		// the parent classifies as OutcomeError. A *harnessError is a distinct
		// harness marker the parent fails loud on. For a panic row the engine
		// panics rather than returning, so a returned error here is almost always
		// the harness — but the distinction is kept exact rather than assumed.
		kind := string(OutcomeError)
		var he *harnessError
		if errors.As(err, &he) {
			kind = faultChildHarnessKind
		}
		fmt.Printf("%s %s %s\n", faultChildMarker, kind, strconv.Quote(err.Error()))
		return
	}
	if len(msgs) != 1 || msgs[0].Role != "system" {
		// The one-system-message shape is a HARNESS/codegen invariant, not an
		// engine fault, so it is a harness marker (loud), never an OutcomeError.
		fmt.Printf("%s %s %s\n", faultChildMarker, faultChildHarnessKind, strconv.Quote(fmt.Sprintf("expected one system message, got %#v", msgs)))
		return
	}
	fmt.Printf("%s rendered %s\n", faultChildMarker, strconv.Quote(msgs[0].Text))
}

// rowByID finds a corpus row.
func rowByID(id string) (Row, bool) {
	for _, r := range Corpus() {
		if r.ID == id {
			return r, true
		}
	}
	return Row{}, false
}

// bamlRenderErr is bamlRender without a *testing.T: it RETURNS the failure
// instead of calling t.Fatal, so the child can classify it as OutcomeError
// rather than dying with a test failure the parent would have to parse.
func bamlRenderErr(rt *baml.BamlRuntime, r Row, env map[string]string) ([]Message, error) {
	kwargs := map[string]any{"stream": false}
	for k, v := range r.Args {
		kwargs[k] = v
	}
	args := baml.BamlFunctionArguments{Kwargs: kwargs, Env: env}
	// Only rt.BuildRequest runs the ENGINE (it renders the template); every other
	// step here is the stock-side HARNESS — encoding the Go args, extracting the
	// request body, and decoding the rendered messages. They are wrapped as
	// *harnessError (the same type the profile leg uses in corpus.go) so
	// bamlOutcomeInProcess can tell them apart from a genuine engine template
	// error and never classify a harness failure as an OutcomeError. This is the
	// stock-leg mirror of the profile-leg fix, keeping the two legs symmetric.
	encoded, err := args.Encode()
	if err != nil {
		return nil, &harnessError{fmt.Errorf("encode args: %w", err)}
	}
	req, err := rt.BuildRequest(context.Background(), r.FuncName(), encoded)
	if err != nil {
		return nil, fmt.Errorf("BuildRequest: %w", err) // ENGINE error — stays plain
	}
	body, err := req.Body()
	if err != nil {
		return nil, &harnessError{fmt.Errorf("request Body(): %w", err)}
	}
	text, err := body.Text()
	if err != nil {
		return nil, &harnessError{fmt.Errorf("body Text(): %w", err)}
	}
	msgs, err := decodeMessages([]byte(text))
	if err != nil {
		return nil, &harnessError{fmt.Errorf("decode messages: %w", err)}
	}
	return msgs, nil
}

// bamlOutcomeInProcess classifies a row stock BAML can safely be asked to render
// in this process — a successful row or an OutcomeError fault row.
func bamlOutcomeInProcess(rt *baml.BamlRuntime, r Row, env map[string]string) Outcome {
	msgs, err := bamlRenderErr(rt, r, env)
	if err != nil {
		// Only a genuine ENGINE error (rt.BuildRequest) is an OutcomeError. A
		// stock-side harness failure is re-raised loudly instead of classified, so
		// a fault row declaring OutcomeError cannot match because the stock harness
		// broke rather than because the engine faulted — the symmetric guard to
		// the profile leg's RenderProfileOutcome.
		var he *harnessError
		if errors.As(err, &he) {
			panic(fmt.Sprintf("profileoracle: row %q: stock BAML harness failed around the engine render (not an engine outcome): %v", r.ID, err))
		}
		return Outcome{Kind: OutcomeError, Detail: err.Error()}
	}
	if len(msgs) != 1 || msgs[0].Role != "system" {
		// The one-system-message shape is a HARNESS/codegen invariant — every
		// corpus function wraps its content in exactly one system-role message —
		// not an engine fault. A mismatch is a harness bug: failing loud here, like
		// the *harnessError guard above, keeps it from classifying as OutcomeError
		// and matching a fault row on a broken harness.
		panic(fmt.Sprintf("profileoracle: row %q: stock BAML rendered but not as one system message (harness invariant, not an engine outcome): %#v", r.ID, msgs))
	}
	return Outcome{Kind: OutcomeRendered, Text: msgs[0].Text}
}

// panicWatcher buffers a child's stderr and signals the instant Rust's panic
// banner appears in it.
//
// Without this the harness would have to wait out faultChildTimeout on every
// panic row — the child hangs by construction, so it never exits on its own. The
// banner is written immediately, so watching for it turns a 90-second wait into
// a sub-second one WITHOUT weakening the evidence: the verdict still requires the
// banner, never a bare timeout.
type panicWatcher struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	seen  chan struct{}
	fired bool
}

func newPanicWatcher() *panicWatcher { return &panicWatcher{seen: make(chan struct{})} }

func (w *panicWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	before := w.buf.Len()
	n, err := w.buf.Write(p)
	if !w.fired {
		// Scan only the newly-written bytes plus a needle-length overlap, so a child
		// that emits a large backtrace does not turn this into a quadratic rescan of
		// the whole buffer on every chunk. The overlap is what keeps a needle split
		// across two writes detectable.
		from := max(before-len(rustPanicNeedle)+1, 0)
		if strings.Contains(string(w.buf.Bytes()[from:]), rustPanicNeedle) {
			w.fired = true
			close(w.seen)
		}
	}
	return n, err
}

func (w *panicWatcher) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// bamlOutcomeSubprocess classifies an OutcomePanic row by running the stock leg
// in a child process and reading its fate.
//
// Three outcomes are possible and all three are handled explicitly:
//
//   - the child printed a marker line -> that classification (so a row declared
//     as a panic that stock BAML actually RENDERS is caught, not silently green);
//   - the child produced no marker but Rust's panic banner is on stderr ->
//     OutcomePanic, whether it exited or had to be killed;
//   - neither -> the harness FAILS. An unexplained hang or crash is never
//     upgraded into a matching fault.
func bamlOutcomeSubprocess(t *testing.T, r Row) Outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), faultChildTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProfileOracleFaultChild$", "-test.timeout=0")
	cmd.Env = append(os.Environ(), faultChildRowEnv+"="+r.ID)
	var stdout bytes.Buffer
	stderr := newPanicWatcher()
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("fault row %q: start stock BAML child: %v", r.ID, err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var runErr error
	select {
	case runErr = <-waitCh:
		// The child finished on its own: it either reported an outcome or died.
	case <-stderr.seen:
		// The engine panicked. The child will now block forever in BuildRequest, so
		// stop waiting and reap it; Wait still has to return before the buffers are
		// safe to read.
		cancel()
		runErr = <-waitCh
	case <-ctx.Done():
		runErr = <-waitCh
	}

	// A stock-side harness failure is never an engine outcome: fail loud rather
	// than let it be classified. Checked before parseChildOutcome so it can never
	// be read as an OutcomeError/OutcomeRendered marker.
	if msg, ok := childMarker(stdout.String(), faultChildHarnessKind); ok {
		t.Fatalf("fault row %q: the stock BAML harness failed, not the engine (%s).\n"+
			"A harness invariant is never an engine outcome and must not classify one.", r.ID, msg)
	}
	if o, ok := parseChildOutcome(stdout.String()); ok {
		return o
	}
	if line, ok := rustPanicLine(stderr.String()); ok {
		return Outcome{Kind: OutcomePanic, Detail: line}
	}
	t.Fatalf("fault row %q: the stock BAML child neither reported an outcome nor panicked.\n"+
		"run error: %v\nstdout:\n%s\nstderr:\n%s", r.ID, runErr, stdout.String(), stderr.String())
	return Outcome{}
}

// parseChildOutcome reads the child's single marker line.
func parseChildOutcome(stdout string) (Outcome, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), faultChildMarker+" ")
		if !ok {
			continue
		}
		kind, quoted, ok := strings.Cut(rest, " ")
		if !ok {
			continue
		}
		payload, err := strconv.Unquote(quoted)
		if err != nil {
			continue
		}
		switch OutcomeKind(kind) {
		case OutcomeRendered:
			return Outcome{Kind: OutcomeRendered, Text: payload}, true
		case OutcomeError:
			return Outcome{Kind: OutcomeError, Detail: payload}, true
		}
	}
	return Outcome{}, false
}

// childMarker returns the payload of the child's marker line of the given kind,
// if the child emitted one. It is how the parent reads the out-of-band
// harness-failure marker, which is deliberately not an OutcomeKind and so is not
// recognized by parseChildOutcome.
func childMarker(stdout, kind string) (string, bool) {
	for _, line := range strings.Split(stdout, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), faultChildMarker+" ")
		if !ok {
			continue
		}
		k, quoted, ok := strings.Cut(rest, " ")
		if !ok || k != kind {
			continue
		}
		if payload, err := strconv.Unquote(quoted); err == nil {
			return payload, true
		}
	}
	return "", false
}

// rustPanicLine extracts the stock engine's panic banner (and the line after it,
// which carries the actual message) from a child's stderr.
func rustPanicLine(stderr string) (string, bool) {
	lines := strings.Split(stderr, "\n")
	for i, line := range lines {
		if !strings.Contains(line, rustPanicNeedle) {
			continue
		}
		detail := strings.TrimSpace(line)
		if i+1 < len(lines) {
			if next := strings.TrimSpace(lines[i+1]); next != "" {
				detail += " | " + next
			}
		}
		return detail, true
	}
	return "", false
}

// TestProfileFaultDifferential is the fault-row proof. For every row that
// declares a Fault, it asserts BOTH that stock BAML really produces the declared
// failure class (so the declaration cannot rot into a fiction) AND that the
// profile fails in the same class.
//
// It never compares the two failure MESSAGES: Rust's "internal error: entered
// unreachable code" and the fork's "cannot order these mappings: …" describe the
// same invariant failure in different words, and pinning the text would pin a
// string rather than a behavior. The CLASS is the contract.
func TestProfileFaultDifferential(t *testing.T) {
	assertBAMLAuthority(t)

	var faults []Row
	for _, r := range Corpus() {
		if r.Fault != "" {
			faults = append(faults, r)
		}
	}
	if len(faults) == 0 {
		t.Fatal("no fault rows in the corpus; the fault leg must not silently cover nothing")
	}

	// Only the in-process rows need a shared runtime; a panic row gets its own
	// child. Building it from the WHOLE corpus keeps this leg's runtime identical
	// to the ordinary differential's.
	env := envVars()
	rt, err := baml.CreateRuntime("./baml_src", GenerateBAMLSource(Corpus()), env)
	if err != nil {
		t.Fatalf("CreateRuntime from in-memory corpus source: %v", err)
	}

	for _, r := range faults {
		t.Run(r.ID, func(t *testing.T) {
			var stock Outcome
			switch r.Fault {
			case OutcomePanic:
				stock = bamlOutcomeSubprocess(t, r)
			case OutcomeError:
				stock = bamlOutcomeInProcess(&rt, r, env)
			default:
				t.Fatalf("row declares Fault %q, which is not a failure class", r.Fault)
			}

			if stock.Kind != r.Fault {
				t.Fatalf("row declares stock BAML Fault=%s, but stock BAML actually produced %s.\n"+
					"The declaration is stale — fix the row, not this assertion.", r.Fault, stock)
			}
			prof := RenderProfileOutcome(r)
			if prof.Kind != stock.Kind {
				t.Errorf("outcome-class mismatch\nstock BAML: %s\nprofile:    %s\n"+
					"A fault row is green ONLY when the profile fails in BAML's class; "+
					"rendering a conservative value where BAML faults is an out-do.", stock, prof)
				return
			}
			t.Logf("both legs %s — stock: %s | profile: %s", stock.Kind, stock.Detail, prof.Detail)
		})
	}
}
