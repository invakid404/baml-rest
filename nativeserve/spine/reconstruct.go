package spine

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// reconstructFunction rebuilds the scalar-input subset of a promptdescriptor.Function
// from the validated projectdescriptor.Project and one of its admitted static-unary
// Methods (ExecBridge-U1 §2), and VALIDATES the project/client-level cohort facts the
// neutral Project projection carries but a bare Function does not — so it FAILS on a
// cohort-forbidden fact rather than silently stripping it (Codex review finding 2).
//
// It is a pure, neutral data transform over the descriptor packages (no nanollm, no
// generated BAML, no codegen toolchain), kept IN the bridge so the production serve
// package stays a thin lane — it does NOT pull internal/nativespine's classifier +
// code-generation dependency graph. It reconstructs everything the scalar cohort needs
// (prompt, ordered resolved scalar argument edges, return Bundle, resolved client
// config) so the reconstructed Function is exactly what the existing native
// render/prepare admission path consumes.
//
// It DECLINES (returns an error) when any of these cohort-forbidden facts is present:
//
//   - the project descriptor version is not the current version;
//   - the project declares ANY template_string (macro): BAML injects the project
//     macro set into every function's prompt, so a templated project's render is not
//     the template-free render this cohort proves — even for a method that does not
//     reference the macro;
//   - the method's resolved default client carries a retry_policy reference (native
//     runs one attempt; BAML would retry, so the two diverge on a provider fault);
//   - the method's resolved default client is a fallback / round-robin strategy.
//
// The return-shape totality predicate, the descriptor envelope, and the scalar-input
// gate are the registry's own (see register); this function owns the project/client
// facts that only the Project carries. It does NOT widen the M1 classifier — it
// NARROWS registration to the exact population cohort.
func reconstructFunction(proj projectdescriptor.Project, m projectdescriptor.Method) (promptdescriptor.Function, *reconstructError) {
	// A method that is admitted (in proj.Methods) yet not static-unary, or a project
	// whose descriptor version is wrong, is an INCONSISTENT descriptor — structural
	// corruption, not a population fact — so it is a HARD failure.
	// proj.Validate proves the version, so this is a defensive backstop.
	if m.Class != projectdescriptor.ClassStaticUnary {
		return promptdescriptor.Function{}, corruptReconstruct(fmt.Errorf("class is %q, want %q", m.Class, projectdescriptor.ClassStaticUnary))
	}
	if proj.Version != projectdescriptor.Version {
		return promptdescriptor.Function{}, corruptReconstruct(fmt.Errorf("project descriptor version %d, want %d", proj.Version, projectdescriptor.Version))
	}
	// A templated project and a retrying/strategy client are POPULATION facts (the
	// method is well-formed but outside the exact cohort), so they are cohort misses.
	if len(proj.Templates) != 0 {
		return promptdescriptor.Function{}, populationReconstruct(fmt.Errorf("project declares %d template_string(s); the exact cohort requires a template-free project (BAML injects the project macro set into every prompt)", len(proj.Templates)))
	}
	if err := validateClientCohort(proj, m); err != nil {
		return promptdescriptor.Function{}, populationReconstruct(err)
	}
	// A method whose default client is ABSENT from the project client graph is a
	// structural inconsistency Project.Validate does not catch (it validates client
	// NAME uniqueness, not that every method's client is defined) — a corrupt
	// descriptor, so a HARD failure, never a silent omission.
	cfg, err := clientConfigFor(proj, m)
	if err != nil {
		return promptdescriptor.Function{}, corruptReconstruct(err)
	}
	args := make([]promptdescriptor.Argument, 0, len(m.Args))
	for i := range m.Args {
		vt := m.Args[i].Type
		args = append(args, promptdescriptor.Argument{Name: m.Args[i].Name, ValueType: &vt})
	}
	return promptdescriptor.Function{
		Version:      promptdescriptor.Version,
		Method:       m.Name,
		Prompt:       m.Prompt,
		Args:         args,
		Client:       m.Client,
		Provider:     m.Provider,
		Return:       m.Return,
		Macros:       nil,
		ClientConfig: cfg,
	}, nil
}

// reconstructError distinguishes a reconstruction failure that is a POPULATION
// decline (the method is well-formed but outside the exact cohort — a templated
// project, a retrying/strategy client) from one that is structural CORRUPTION (an
// inconsistent descriptor — a client the project graph does not contain, a
// non-static-unary admitted method). classifyBinding maps the former to a cohort
// miss (omitted from the native-only registry) and the latter to a HARD boot
// failure, so a corrupt candidate can never be silently downgraded to a miss.
type reconstructError struct {
	corrupt bool
	err     error
}

func (e *reconstructError) Error() string { return e.err.Error() }
func (e *reconstructError) Unwrap() error { return e.err }

func populationReconstruct(err error) *reconstructError {
	return &reconstructError{corrupt: false, err: err}
}
func corruptReconstruct(err error) *reconstructError {
	return &reconstructError{corrupt: true, err: err}
}

// validateClientCohort declines the method if its resolved default client carries a
// retry_policy reference or is a fallback/round-robin strategy — cohort-forbidden
// facts that live on projectdescriptor.Client / project.Strategies, NOT on the copied
// promptdescriptor.ClientConfig.
func validateClientCohort(proj projectdescriptor.Project, m projectdescriptor.Method) error {
	for i := range proj.Clients {
		if proj.Clients[i].Config.Name == m.Client {
			if proj.Clients[i].RetryPolicy != "" {
				return fmt.Errorf("client %q declares a retry_policy (%q); the exact cohort forbids retries", m.Client, proj.Clients[i].RetryPolicy)
			}
			break
		}
	}
	for i := range proj.Strategies {
		if proj.Strategies[i].Name == m.Client {
			return fmt.Errorf("client %q is a %s strategy; the exact cohort requires a single resolved leaf", m.Client, proj.Strategies[i].Kind)
		}
	}
	return nil
}

// clientConfigFor resolves the reconstructed ClientConfig for the method's resolved
// default client from the Project's whole-project client graph.
func clientConfigFor(proj projectdescriptor.Project, m projectdescriptor.Method) (promptdescriptor.ClientConfig, error) {
	for i := range proj.Clients {
		if proj.Clients[i].Config.Name == m.Client {
			return proj.Clients[i].Config, nil
		}
	}
	return promptdescriptor.ClientConfig{}, fmt.Errorf("client %q is not present in the project client graph", m.Client)
}
