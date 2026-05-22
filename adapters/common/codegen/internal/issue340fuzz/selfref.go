package issue340fuzz

// AnalyzeGraph computes the graph-metadata flags (HasSelfRef,
// HasMutualCycle, RequiresDynamicSkip) from the class-ref
// reachability graph and returns a copy of `schema` with those
// fields refreshed. HasUnion is left unchanged — unions are
// excluded from v1's grammar, so the field is always false here
// but kept as a forward-compat slot.
//
// The reachability graph treats every class ref in any property
// type — including refs reached through optional / list / map
// wrappers — as an edge from the containing class to the referenced
// class. Self-loops mean A reaches itself directly via at least one
// property chain. Mutual cycles mean A and B (A≠B) each reach the
// other.
//
// RequiresDynamicSkip is true whenever the dynamic emitter cannot
// safely realize the schema. Today that means any self-ref edge:
// upstream BAML TypeBuilder cannot express class-self-reference
// (TODO(upstream-self-ref)). A schema with a mutual cycle but no
// direct self-ref also requires dynamic skip — TypeBuilder cannot
// represent the cycle via dynamic class addition either.
func AnalyzeGraph(schema FuzzSchema) FuzzSchema {
	reach := ReachabilityClosure(schema)
	out := schema
	out.HasSelfRef = false
	out.HasMutualCycle = false
	for _, cls := range schema.Classes {
		if reach[cls.Name][cls.Name] {
			out.HasSelfRef = true
		}
	}
	for _, a := range schema.Classes {
		for _, b := range schema.Classes {
			if a.Name == b.Name {
				continue
			}
			if reach[a.Name][b.Name] && reach[b.Name][a.Name] {
				out.HasMutualCycle = true
			}
		}
	}
	out.RequiresDynamicSkip = out.HasSelfRef || out.HasMutualCycle
	return out
}

// ReachabilityClosure returns the transitive class-ref reachability
// graph. reach[A][B] == true means A reaches B through one or more
// class-ref edges in the property types of A or one of A's
// reachable classes.
//
// Exported for the same-file generator + the invariant tests that
// independently re-derive the metadata flags and assert they match
// the generator's stamping.
func ReachabilityClosure(schema FuzzSchema) map[string]map[string]bool {
	direct := directClassRefs(schema)
	closure := make(map[string]map[string]bool, len(direct))
	for k, v := range direct {
		closure[k] = make(map[string]bool, len(v))
		for tgt := range v {
			closure[k][tgt] = true
		}
	}
	names := make([]string, 0, len(schema.Classes))
	for _, cls := range schema.Classes {
		names = append(names, cls.Name)
	}
	// Floyd–Warshall over a bounded (≤4) class set is fine.
	for _, k := range names {
		for _, i := range names {
			if !closure[i][k] {
				continue
			}
			for _, j := range names {
				if closure[k][j] {
					if closure[i] == nil {
						closure[i] = make(map[string]bool)
					}
					closure[i][j] = true
				}
			}
		}
	}
	return closure
}

// directClassRefs returns the one-hop class-ref edges per class.
func directClassRefs(schema FuzzSchema) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(schema.Classes))
	for _, cls := range schema.Classes {
		set := make(map[string]bool)
		for _, prop := range cls.Properties {
			collectClassRefs(prop.Type, set)
		}
		out[cls.Name] = set
	}
	return out
}

func collectClassRefs(t FuzzType, out map[string]bool) {
	switch t.Kind {
	case KindClassRef:
		out[t.Ref] = true
	case KindOptional, KindList, KindMap:
		if t.Inner != nil {
			collectClassRefs(*t.Inner, out)
		}
	}
}
