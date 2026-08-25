package nativeschema

// clientgraph.go builds the whole-project CLIENT GRAPH — every client<llm> block
// (its resolved body/transport config + retry reference), every retry_policy
// block, and every fallback/round-robin strategy wrapper with its ordered
// children — from the SAME parsed .baml AST the rest of the native lane reads.
//
// It is the M2 generalization of BuildClientConfigs: the native input lane owns
// its own client read (codegen-spine D10) so the fixture and the introspect pass
// build the identical whole-project descriptor without depending on the legacy
// generated-lane parsing in cmd/introspect. It introduces no second parser — it
// walks bamlparser AST items exactly like BuildClientConfigs — records the RAW
// provider spelling (the classifier folds canonical vs raw), and orders every
// output deterministically by name (unique per kind; duplicate declarations are
// last-wins), so the descriptor is stable across file-ordering rules.

import (
	"sort"
	"strconv"
	"strings"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/bamlutils/promptdescriptor"
)

// strategyKinds maps a raw/canonical strategy provider spelling to its kind. A
// client is a strategy wrapper only when its provider is one of these AND it
// declares a `strategy [...]` option.
var strategyKinds = map[string]projectdescriptor.StrategyKind{
	"baml-fallback":    projectdescriptor.StrategyFallback,
	"fallback":         projectdescriptor.StrategyFallback,
	"baml-roundrobin":  projectdescriptor.StrategyRoundRobin,
	"baml-round-robin": projectdescriptor.StrategyRoundRobin,
	"round-robin":      projectdescriptor.StrategyRoundRobin,
}

// BuildClientGraph reads the whole-project client graph from the parsed files and
// returns ordered-by-name Clients, RetryPolicies, and Strategies.
func BuildClientGraph(files []SourceFile) ([]projectdescriptor.Client, []projectdescriptor.RetryPolicy, []projectdescriptor.Strategy) {
	clientByName := map[string]projectdescriptor.Client{}
	retryByName := map[string]projectdescriptor.RetryPolicy{}
	strategyByName := map[string]projectdescriptor.Strategy{}

	for _, sf := range files {
		if sf.File == nil {
			continue
		}
		for _, it := range sf.File.Items {
			switch {
			case it.Client != nil && it.Client.Name != "" && (it.Client.TypeParam == "llm" || it.Client.TypeParam == ""):
				c := it.Client
				client := projectdescriptor.Client{Config: buildOneClientConfig(c)}
				var chain []string
				var start *int
				for _, f := range c.Fields {
					switch {
					case f.Key == "retry_policy" && f.Value != nil:
						if s, ok := scalarOptionString(f.Value); ok {
							client.RetryPolicy = s
						}
					case f.Key == "options" && f.Block != nil:
						// ACCUMULATE across every options block, mirroring
						// processBAMLOptionsBlock's conditional writes to bc.fallbackChains
						// / bc.roundRobinStart: overwrite the chain ONLY when a block yields
						// a non-empty strategy, and the start ONLY when a block yields a
						// valid immediate-depth start — so a later `options { model … }`
						// never wipes an earlier `strategy`/`start`.
						blockChain, blockStart := extractOptionsStrategy(f.Block)
						if len(blockChain) > 0 {
							chain = blockChain
						}
						if blockStart != nil {
							start = blockStart
						}
					}
				}
				// Last-wins on a duplicate client name (mirrors the config map and
				// cmd/introspect's clear-and-rebuild), and clear any prior strategy so
				// a redeclaration that drops the strategy option does not leave a stale
				// wrapper behind.
				clientByName[c.Name] = client
				delete(strategyByName, c.Name)
				// A strategy wrapper is emitted only for a recognized strategy provider
				// with a NON-EMPTY chain (mirroring processBAMLOptionsBlock's
				// `len(chain) > 0` guard); the round-robin `start` seed is meaningful
				// only for round-robin (nil on a fallback strategy).
				if kind, ok := strategyKinds[client.Config.Provider]; ok && len(chain) > 0 {
					strat := projectdescriptor.Strategy{Name: c.Name, Kind: kind, Children: chain}
					if kind == projectdescriptor.StrategyRoundRobin {
						strat.Start = start
					}
					strategyByName[c.Name] = strat
				}
			case it.RetryPolicy != nil && it.RetryPolicy.Name != "":
				retryByName[it.RetryPolicy.Name] = buildRetryPolicy(it.RetryPolicy)
			}
		}
	}

	clients := make([]projectdescriptor.Client, 0, len(clientByName))
	for _, c := range clientByName {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].Config.Name < clients[j].Config.Name })

	retries := make([]projectdescriptor.RetryPolicy, 0, len(retryByName))
	for _, r := range retryByName {
		retries = append(retries, r)
	}
	sort.Slice(retries, func(i, j int) bool { return retries[i].Name < retries[j].Name })

	strategies := make([]projectdescriptor.Strategy, 0, len(strategyByName))
	for _, s := range strategyByName {
		strategies = append(strategies, s)
	}
	sort.Slice(strategies, func(i, j int) bool { return strategies[i].Name < strategies[j].Name })

	return clients, retries, strategies
}

// buildRetryPolicy lowers one retry_policy block into a neutral descriptor,
// mirroring cmd/introspect's processBAMLRetryPolicyBlock: direct fields first,
// then a nested `strategy` block overrides them.
func buildRetryPolicy(r *bamlparser.RetryPolicyBlock) projectdescriptor.RetryPolicy {
	rp := projectdescriptor.RetryPolicy{Name: r.Name}
	apply := func(key string, v *bamlparser.Value) {
		switch key {
		case "max_retries":
			if n, ok := scalarInt(v); ok {
				rp.MaxRetries = n
			}
		case "type":
			if v != nil {
				if s, ok := scalarOptionString(v); ok {
					rp.Strategy = s
				}
			}
		case "delay_ms":
			if n, ok := scalarInt(v); ok {
				rp.DelayMs = n
			}
		case "multiplier":
			if f, ok := scalarFloat(v); ok {
				rp.Multiplier = f
			}
		case "max_delay_ms":
			if n, ok := scalarInt(v); ok {
				rp.MaxDelayMs = n
			}
		}
	}
	for _, f := range r.Fields {
		if f.Block != nil {
			continue
		}
		apply(f.Key, f.Value)
	}
	for _, f := range r.Fields {
		if f.Key != "strategy" || f.Block == nil {
			continue
		}
		for _, sf := range f.Block.Fields {
			apply(sf.Key, sf.Value)
		}
	}
	return rp
}

// extractOptionsStrategy mirrors cmd/introspect's processBAMLOptionsBlock +
// processBAMLOptionsStrategyRecursive EXACTLY: the `strategy` chain is captured at
// ANY depth inside the options block with last-write-wins in depth-first field
// order (a nested `strategy` overrides an outer one — the OptionsNestedDepthGating
// contract), while `start` is honored ONLY at the immediate options depth. An
// empty `strategy []` never yields a chain (the pinned EmptyStrategyList contract).
func extractOptionsStrategy(opts *bamlparser.Block) (chain []string, start *int) {
	for _, f := range opts.Fields {
		switch f.Key {
		case "strategy":
			if f.Value != nil && f.Value.List != nil {
				if list := strategyChildList(f.Value); len(list) > 0 {
					chain = list
				}
			}
		case "start":
			if f.Value != nil && f.Value.Number != nil {
				if n, err := strconv.ParseInt(*f.Value.Number, 10, 32); err == nil {
					v := int(n)
					start = &v
				}
			}
		}
		if f.Block != nil {
			if nested := nestedStrategyChain(f.Block); len(nested) > 0 {
				chain = nested
			}
		}
	}
	return chain, start
}

// nestedStrategyChain recurses into block-valued option fields looking for a
// nested `strategy [...]` (last-write-wins), mirroring
// processBAMLOptionsStrategyRecursive. Only `strategy` is captured; `start` is not.
func nestedStrategyChain(blk *bamlparser.Block) []string {
	var chain []string
	for _, f := range blk.Fields {
		if f.Key == "strategy" && f.Value != nil && f.Value.List != nil {
			if list := strategyChildList(f.Value); len(list) > 0 {
				chain = list
			}
		}
		if f.Block != nil {
			if nested := nestedStrategyChain(f.Block); len(nested) > 0 {
				chain = nested
			}
		}
	}
	return chain
}

// scalarInt extracts an integer from a numeric scalar value (retry-policy fields).
func scalarInt(v *bamlparser.Value) (int, bool) {
	if v == nil {
		return 0, false
	}
	if s, ok := v.NumberValue(); ok {
		if n, err := strconv.Atoi(s); err == nil {
			return n, true
		}
	}
	return 0, false
}

// strategyChildList flattens a `strategy [A, B, ...]` list into ordered child
// client names, mirroring cmd/introspect's bamlValueStrategyList EXACTLY:
// scalar-String each element (any scalar shape), trim, and drop empties.
func strategyChildList(v *bamlparser.Value) []string {
	if v == nil || v.List == nil {
		return nil
	}
	var out []string
	for _, e := range v.List {
		s, ok := e.String()
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// scalarFloat extracts a float from a numeric scalar value.
func scalarFloat(v *bamlparser.Value) (float64, bool) {
	if v == nil {
		return 0, false
	}
	if s, ok := v.NumberValue(); ok {
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// BuildProjectTemplates returns the whole-project template_string (macro) set the
// prompt builder installs, in name order for descriptor determinism. It reuses
// the same macro reader BuildPromptDescriptors uses; the macros are listed even
// when a bad/duplicate macro poisons every function's prompt descriptor (that
// effect is reflected in each method's decline, not by dropping the template).
func BuildProjectTemplates(files []SourceFile) []promptdescriptor.TemplateString {
	idx := buildSchemaTypeIndex(files)
	macros, _, _ := buildMacros(files, idx, idx.recursion())
	sorted := append([]promptdescriptor.TemplateString(nil), macros...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	return sorted
}
