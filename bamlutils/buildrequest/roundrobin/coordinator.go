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
// adapter package and shared across every request.
//
// The zero value is usable: it behaves like NewCoordinator() /
// NewCoordinatorWithStarts(nil), giving every client a random first-call
// seed and never honouring a configured `start` (because the starts map
// is nil). Constructors are only required when a configured `starts`
// map needs to be installed; callers that don't care about per-client
// seeds can use the zero value directly.
type Coordinator struct {
	counters sync.Map // map[string]*atomic.Uint64
	// starts is an immutable map of per-client seed values captured at
	// construction time. A client whose name appears here bypasses the
	// fastrand seed and uses the configured value instead, matching the
	// BAML `start N` option on a baml-roundrobin client. Clients not in
	// this map keep the random-seed behaviour.
	starts map[string]uint64
	// randSeed produces the initial counter value for a client absent
	// from `starts`. Production callers leave this nil, in which case
	// rand.Uint32 from math/rand/v2 supplies the seed (the historical
	// behaviour). Tests use SetRandSeedForTest to inject a
	// deterministic source so they can assert that an unlisted client
	// observes the random seed rather than the configured seed for
	// some other listed client.
	randSeed func() uint32
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
// Negative `start` values are reinterpreted as unsigned, matching
// upstream BAML's runtime which computes
// `(start as usize) % strategy.len()`
// (engine/baml-runtime/src/internal/llm_client/strategy/roundrobin.rs:64-65) —
// a sign-extending bitcast from i32 on 64-bit. So -1i32 becomes
// 0xFFFFFFFFFFFFFFFF, which mod 2 is 1 (last child), mod 3 is 0,
// and so on. Mirroring the cast keeps the centralised host counter
// aligned with BAML's per-worker runtime: a request with start=-1
// dispatches the same child whether it traverses the SharedState
// path or BAML's legacy rotation. The starts map is uint64 already,
// so the wire format stays unchanged.
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
// childCount must be > 0; a non-positive value returns (0, nil) without
// touching state (no counter created, no atomic increment), so callers
// that skip validation still get deterministic output. The error return
// exists to satisfy the Advancer interface shape shared with the remote
// implementation; the in-process path never fails.
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
		// purpose. The injected randSeed seam (test-only; nil in
		// production) lets the unlisted-clients-stay-random regression
		// test assert independence from the configured-starts map.
		seedFn := c.randSeed
		if seedFn == nil {
			seedFn = rand.Uint32
		}
		fresh.Store(uint64(seedFn()))
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
