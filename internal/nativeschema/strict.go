package nativeschema

// strict.go implements the M2 STRICT whole-project source diagnostics
// (codegen-spine §1.3, D3). The best-effort introspect walk that feeds the
// GENERATED lane deliberately skips unreadable/invalid files and tolerates
// duplicate/ambiguous declarations; a NATIVE executable, by contrast, must fail
// generation on invalid retained source. Unreadable-file and parse errors are
// collected by the caller during the walk; this file adds the project-integrity
// half — duplicate/ambiguous top-level declarations and unresolved type
// references — over the already-parsed AST, reusing the same schema type index
// the rest of the native lane builds (never a second parser).

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
)

// CheckProjectIntegrity returns a joined, deterministically-ordered error naming
// every whole-project integrity violation a strict native build must reject, or
// nil when the project is clean: a class/enum/alias/function/client/retry_policy
// name declared more than once, or a bare type reference that resolves to no
// declared class, enum, or alias. Namespaced/path references are external and are
// not resolved here.
func CheckProjectIntegrity(files []SourceFile) error {
	idx := buildSchemaTypeIndex(files)

	var msgs []string

	// Duplicate class/enum/alias names are recorded by the index.
	for name := range idx.ambiguous {
		msgs = append(msgs, fmt.Sprintf("type %q is declared more than once", name))
	}

	// Duplicate function / client / retry_policy / template_string names.
	funcCounts := map[string]int{}
	clientCounts := map[string]int{}
	retryCounts := map[string]int{}
	templateCounts := map[string]int{}
	for _, sf := range files {
		if sf.File == nil {
			continue
		}
		for _, it := range sf.File.Items {
			switch {
			case it.Function != nil && it.Function.Name != "":
				funcCounts[it.Function.Name]++
			case it.Client != nil && it.Client.Name != "" && (it.Client.TypeParam == "llm" || it.Client.TypeParam == ""):
				clientCounts[it.Client.Name]++
			case it.RetryPolicy != nil && it.RetryPolicy.Name != "":
				retryCounts[it.RetryPolicy.Name]++
			case it.Template != nil && it.Template.Name != "":
				templateCounts[it.Template.Name]++
			}
		}
	}
	for name, n := range funcCounts {
		if n > 1 {
			msgs = append(msgs, fmt.Sprintf("function %q is declared more than once", name))
		}
	}
	for name, n := range clientCounts {
		if n > 1 {
			msgs = append(msgs, fmt.Sprintf("client %q is declared more than once", name))
		}
	}
	for name, n := range retryCounts {
		if n > 1 {
			msgs = append(msgs, fmt.Sprintf("retry_policy %q is declared more than once", name))
		}
	}
	for name, n := range templateCounts {
		if n > 1 {
			msgs = append(msgs, fmt.Sprintf("template_string %q is declared more than once", name))
		}
	}

	// Unresolved bare type references reachable from function signatures and class
	// field types. A reference is unresolved when its name binds to no declared
	// class, enum, or alias.
	unresolved := map[string]bool{}
	var walk func(t *bamlparser.TypeExpr)
	walk = func(t *bamlparser.TypeExpr) {
		if t == nil {
			return
		}
		if t.Kind == bamlparser.KindNameRef && t.Name != "" && !t.Namespaced && !t.Path {
			if _, isClass := idx.classes[t.Name]; !isClass {
				if _, isEnum := idx.enums[t.Name]; !isEnum {
					if _, isAlias := idx.aliases[t.Name]; !isAlias {
						unresolved[t.Name] = true
					}
				}
			}
		}
		walk(t.Elem)
		walk(t.Key)
		walk(t.Value)
		walk(t.Inner)
		for _, v := range t.Variants {
			walk(v)
		}
		for _, it := range t.Items {
			walk(it)
		}
	}
	for _, sf := range files {
		if sf.File == nil {
			continue
		}
		for _, it := range sf.File.Items {
			switch {
			case it.Function != nil:
				for _, p := range it.Function.Params {
					walk(p.Type)
				}
				walk(it.Function.Return)
			case it.TypeBlock != nil:
				for _, m := range it.TypeBlock.Fields {
					walk(m.Type)
				}
			case it.TypeAlias != nil:
				// The alias RHS must resolve — otherwise `type T = Missing` binds T in
				// the index while Missing is never visited, masking the invalid target.
				walk(it.TypeAlias.Expr)
			case it.Template != nil:
				for _, a := range it.Template.Args {
					walk(a.Type)
				}
			}
		}
	}
	for name := range unresolved {
		msgs = append(msgs, fmt.Sprintf("type reference %q resolves to no declared class, enum, or alias", name))
	}

	// Client-graph reference resolution: a function's client, a client's
	// retry_policy, and every fallback/round-robin strategy child must name a
	// declared client / retry_policy. A client name containing "/" is a shorthand
	// spec (provider/model) and needs no declared block.
	declaredClients := map[string]bool{}
	declaredRetries := map[string]bool{}
	for _, sf := range files {
		if sf.File == nil {
			continue
		}
		for _, it := range sf.File.Items {
			switch {
			case it.Client != nil && it.Client.Name != "" && (it.Client.TypeParam == "llm" || it.Client.TypeParam == ""):
				declaredClients[it.Client.Name] = true
			case it.RetryPolicy != nil && it.RetryPolicy.Name != "":
				declaredRetries[it.RetryPolicy.Name] = true
			}
		}
	}
	clientResolved := func(name string) bool {
		return declaredClients[name] || strings.Contains(name, "/")
	}
	for _, sf := range files {
		if sf.File == nil {
			continue
		}
		for _, it := range sf.File.Items {
			switch {
			case it.Function != nil:
				for _, f := range it.Function.Fields {
					if f.Key == "client" && f.Value != nil {
						if s, ok := f.Value.String(); ok && s != "" && !clientResolved(s) {
							msgs = append(msgs, fmt.Sprintf("function %q references undeclared client %q", it.Function.Name, s))
						}
					}
				}
			case it.Client != nil && it.Client.Name != "" && (it.Client.TypeParam == "llm" || it.Client.TypeParam == ""):
				var chain []string // accumulated across options blocks (see BuildClientGraph)
				for _, f := range it.Client.Fields {
					if f.Key == "retry_policy" && f.Value != nil {
						if s, ok := f.Value.String(); ok && s != "" && !declaredRetries[s] {
							msgs = append(msgs, fmt.Sprintf("client %q references undeclared retry_policy %q", it.Client.Name, s))
						}
					}
					if f.Key == "options" && f.Block != nil {
						if blockChain, _ := extractOptionsStrategy(f.Block); len(blockChain) > 0 {
							chain = blockChain
						}
					}
				}
				// Resolve strategy children ONLY for a client that actually becomes a
				// strategy wrapper — i.e. a recognized strategy provider, matching
				// BuildClientGraph's emit guard. A stray `strategy` on a non-strategy
				// client (e.g. provider openai) produces no descriptor Strategy, so its
				// children must not fail the strict build.
				if _, isStrategyProvider := strategyKinds[buildOneClientConfig(it.Client).Provider]; isStrategyProvider {
					for _, child := range chain {
						if !clientResolved(child) {
							msgs = append(msgs, fmt.Sprintf("strategy client %q references undeclared child client %q", it.Client.Name, child))
						}
					}
				}
			}
		}
	}

	if len(msgs) == 0 {
		return nil
	}
	sort.Strings(msgs)
	errs := make([]error, len(msgs))
	for i, m := range msgs {
		errs[i] = errors.New(m)
	}
	return errors.Join(errs...)
}
