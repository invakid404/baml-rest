// Package roundrobin implements the cross-request state needed to drive
// BAML's baml-roundrobin strategy from the BuildRequest path. Round-robin
// requires each new request to pick a different starting child — without
// persistent per-client counters it would degrade to fallback (always
// starting at child 0), which is silently wrong.
//
// Two counter lifecycles coexist:
//
//   - Static clients (defined in .baml source) share a long-lived counter
//     keyed by client name, held in the Coordinator. The counter starts at
//     a uniformly-random offset so a fleet of fresh processes does not
//     lockstep on child 0.
//   - Dynamic clients (introduced purely via client_registry overrides)
//     intentionally do NOT share state across requests. BAML upstream
//     rebuilds a fresh Arc<LLMProvider> per request-scoped context, so the
//     initial index re-randomises each call. AdvanceDynamic mirrors that
//     by returning a fresh random index without touching the Coordinator.
package roundrobin

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

// Coordinator owns long-lived per-client counters used by round-robin
// strategy resolution. Exactly one Coordinator is created per generated
// adapter package and shared across every request. The zero value is NOT
// ready — callers must use NewCoordinator or NewCoordinatorWithStarts.
type Coordinator struct {
	counters sync.Map // map[string]*atomic.Uint64
	// starts is an immutable map of per-client seed values captured at
	// construction time. A client whose name appears here bypasses the
	// fastrand seed and uses the configured value instead, matching the
	// BAML `start N` option on a baml-roundrobin client. Clients not in
	// this map keep the random-seed behaviour.
	starts map[string]uint64
}

// NewCoordinator returns a Coordinator with no configured per-client
// seeds. Counters are created lazily on first Advance for each client
// name and start at a random offset. Equivalent to
// NewCoordinatorWithStarts(nil); kept for call sites (primarily tests)
// that do not care about the start option.
func NewCoordinator() *Coordinator {
	return NewCoordinatorWithStarts(nil)
}

// NewCoordinatorWithStarts returns a Coordinator that honours the
// BAML-level `start` option for the listed clients. Clients in starts
// skip the fastrand seed and start the rotation at the configured
// index; clients absent from the map retain the random-start behaviour.
//
// baml-rest clamps negative `start` values to zero. Upstream BAML
// computes `(start as usize) % strategy.len()`
// (engine/baml-runtime/src/internal/llm_client/strategy/roundrobin.rs:64-65),
// which is sign-extension + bitcast: -1i32 on 64-bit becomes
// 0xFFFFFFFFFFFFFFFF, then modulo strategy.len() yields a value that
// depends on the chain length (1 for len=2, 0 for len=3, …). That's a
// quirk of the unsigned cast, not a deliberate "wrap to last child"
// semantic — no operator types `start -1` expecting predictable
// behaviour. We intentionally diverge by clamping to a safe
// deterministic value rather than mirroring the cast artifact. See
// PR #192 cold-review-2 finding 3.
//
// The map is copied; callers may mutate the input afterwards without
// affecting the coordinator.
func NewCoordinatorWithStarts(starts map[string]int) *Coordinator {
	c := &Coordinator{}
	if len(starts) == 0 {
		return c
	}
	c.starts = make(map[string]uint64, len(starts))
	for name, v := range starts {
		if v < 0 {
			v = 0
		}
		c.starts[name] = uint64(v)
	}
	return c
}

// Advance returns the next child index for a static round-robin client,
// incrementing the underlying atomic counter. The first call for a given
// client seeds the counter at a random offset (via math/rand/v2, which is
// goroutine-safe and needs no explicit seeding on Go 1.22+), matching
// BAML upstream's fastrand::usize(..len) behaviour for the initial index.
//
// childCount must be > 0; a non-positive value is treated as 1 and returns
// (0, nil) so callers that skip validation still get deterministic output.
// The error return exists to satisfy the Advancer interface shape shared
// with the remote implementation; the in-process path never fails.
func (c *Coordinator) Advance(clientName string, childCount int) (int, error) {
	if childCount <= 0 {
		return 0, nil
	}
	counter := c.counterFor(clientName)
	// Add returns the post-increment value; subtract one so the returned
	// index matches BAML upstream semantics (fetch_add returns the OLD
	// value, then the modulo is applied).
	next := counter.Add(1) - 1
	return int(next % uint64(childCount)), nil
}

// counterFor returns the atomic counter for a client, creating it on
// first call. A configured start seeds the counter deterministically;
// otherwise a random offset avoids lockstep across a fresh fleet.
// LoadOrStore guarantees only one atomic survives even under concurrent
// creation — the loser is discarded.
func (c *Coordinator) counterFor(clientName string) *atomic.Uint64 {
	if v, ok := c.counters.Load(clientName); ok {
		return v.(*atomic.Uint64)
	}
	fresh := &atomic.Uint64{}
	if seed, ok := c.starts[clientName]; ok {
		fresh.Store(seed)
	} else {
		// Seed in a modest range so overflow is not a practical concern
		// even at sustained high request rates. Upper bits left unused on
		// purpose.
		fresh.Store(uint64(rand.Uint32()))
	}
	actual, _ := c.counters.LoadOrStore(clientName, fresh)
	return actual.(*atomic.Uint64)
}

// AdvanceDynamic returns the child index for a dynamic (per-request) round-
// robin client. No state is retained: each call picks a uniformly-random
// child. This matches BAML's behaviour for client_registry overrides —
// a fresh Arc<LLMProvider> per context means a fresh atomic counter that
// lives only for the duration of one request.
//
// childCount must be > 0; non-positive values return 0.
func AdvanceDynamic(childCount int) int {
	if childCount <= 0 {
		return 0
	}
	return rand.IntN(childCount)
}
