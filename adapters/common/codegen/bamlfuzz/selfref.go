package bamlfuzz

// AnalyzeGraph computes the graph-metadata flags (HasSelfRef,
// HasMutualCycle, RequiresDynamicSkip) from the class-ref
// reachability graph and returns a copy of `schema` with those
// fields refreshed. HasUnion is left unchanged — unions are excluded
// from the v1 grammar, so the field is always false here but kept
// as a forward-compat slot.
//
// HasSelfRef is true when at least one class C has a class-ref edge
// to C in its own property tree (the direct one-hop edges, before
// closure). Self-ref means C reaches itself through its own type
// declaration, not through a chain of other classes.
//
// HasMutualCycle is true when at least one class is in a cycle
// through OTHER classes — i.e. the transitive closure reports
// reach[C][C]=true but C does not have a direct self-ref edge.
//
// RequiresDynamicSkip is true whenever the dynamic emitter cannot
// safely realize the schema. Two upstream limitations gate it today:
//   - self-ref classes (TODO(upstream-self-ref)): upstream BAML
//     TypeBuilder cannot express a class referencing itself.
//   - mutual cycles between distinct classes
//     (TODO(upstream-mutual-rec-dynamic-crash)): the BAML cgo
//     TypeBuilder aborts the host process with a signal-level fault
//     when a schema carries A→B→A cross-references. The value
//     generator already terminates such cycles via the per-class
//     recursion cap, so the IR + walker side is ready; only the
//     dynamic emission path is gated.
func AnalyzeGraph(schema FuzzSchema) FuzzSchema {
	direct := directClassRefs(schema)
	reach := closureFromDirect(schema, direct)
	out := schema
	out.HasSelfRef = false
	out.HasMutualCycle = false
	for _, cls := range schema.Classes {
		hasDirectSelf := direct[cls.Name][cls.Name]
		hasCycleHere := reach[cls.Name][cls.Name]
		if hasDirectSelf {
			out.HasSelfRef = true
			continue
		}
		if hasCycleHere {
			out.HasMutualCycle = true
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
	return closureFromDirect(schema, directClassRefs(schema))
}

func closureFromDirect(schema FuzzSchema, direct map[string]map[string]bool) map[string]map[string]bool {
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
		// Map keys are always KindString in the v1 grammar
		// (enforced at schema construction by the generator and at
		// the IR doc on FuzzType), so class-ref reachability only
		// needs to descend into Inner.
		if t.Inner != nil {
			collectClassRefs(*t.Inner, out)
		}
	}
}
