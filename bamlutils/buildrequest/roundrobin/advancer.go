package roundrobin

import (
	"github.com/invakid404/baml-rest/bamlutils"
)

// Advancer decides the next child index for a static round-robin client.
// The interface is defined in the bamlutils root package so the Adapter
// interface (which is passed through generated code) can declare a
// RoundRobinAdvancer accessor without introducing an import cycle. The
// alias here keeps existing call sites that spell it "roundrobin.Advancer"
// working unchanged.
//
// Two implementations ship today:
//
//   - *Coordinator: in-process counters. Used by standalone worker
//     binaries (no host shared state), tests, and any callsite that
//     predates the brokered SharedState wiring. Each process rotates
//     independently — with N workers you get N counters.
//   - workerplugin.RemoteAdvancer: calls FetchAdd on the host-side
//     SharedState service. Used by pool-managed workers so every worker
//     in the pool observes a single rotation. RemoteAdvancer lives in
//     the workerplugin package on purpose — it holds a gRPC
//     pb.SharedStateClient, and keeping that dep out of bamlutils keeps
//     the old version-pinned adapters building standalone (they
//     transitively import bamlutils via replace directives but have no
//     replace for workerplugin).
type Advancer = bamlutils.RoundRobinAdvancer

// Compile-time assertion that the in-process Coordinator satisfies
// Advancer. Keeps the two implementations contractually aligned.
var _ Advancer = (*Coordinator)(nil)
