package nativespine

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// ReconstructFunction rebuilds the scalar-input subset of a promptdescriptor.Function
// from a validated projectdescriptor.Project and one of its admitted static-unary
// Methods (ExecBridge-U1 §2). It is the root-owned reconstruction the production
// runtime consumes: the neutral Project projection retains everything the scalar
// cohort needs — the prompt, the ordered resolved scalar argument edges, the return
// Bundle, and the resolved client config (model + literal transport options) from the
// whole-project client graph — so the reconstructed Function is exactly what the
// existing native render/prepare admission path consumes for the exact-JSON cohort.
//
// It reconstructs ONLY the scalar-input subset: it does NOT rebuild the input
// enum/class universe (the Project carries none, and the exact cohort has none). A
// method with a class/enum/media/etc. argument is left for the bridge registration
// gate to decline; this function faithfully rebuilds whatever scalar/list arguments
// the Method carries, and never widens the M1 classifier.
func ReconstructFunction(proj projectdescriptor.Project, m projectdescriptor.Method) (promptdescriptor.Function, error) {
	if m.Class != projectdescriptor.ClassStaticUnary {
		return promptdescriptor.Function{}, fmt.Errorf("nativespine: reconstruct %q: class is %q, want %q", m.Name, m.Class, projectdescriptor.ClassStaticUnary)
	}
	cfg, err := clientConfigFor(proj, m)
	if err != nil {
		return promptdescriptor.Function{}, err
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

// clientConfigFor resolves the reconstructed ClientConfig for the method's resolved
// default client from the Project's whole-project client graph. The exact cohort
// resolves exactly one leaf, so the descriptor's Client names a client whose Config
// the Project carries verbatim (model + literal transport options + any body option).
func clientConfigFor(proj projectdescriptor.Project, m projectdescriptor.Method) (promptdescriptor.ClientConfig, error) {
	for i := range proj.Clients {
		if proj.Clients[i].Config.Name == m.Client {
			return proj.Clients[i].Config, nil
		}
	}
	return promptdescriptor.ClientConfig{}, fmt.Errorf("nativespine: reconstruct %q: client %q is not present in the project client graph", m.Name, m.Client)
}
