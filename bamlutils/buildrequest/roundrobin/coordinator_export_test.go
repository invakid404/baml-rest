package roundrobin

// Test-only seam for the unlisted-clients-stay-random regression check.
// Production callers leave Coordinator.randSeed nil, falling back to
// rand.Uint32 from math/rand/v2; tests inject a deterministic value
// here so they can assert that a client absent from the configured
// `starts` map observes this seed rather than leaking the seed of a
// listed sibling.
func (c *Coordinator) SetRandSeedForTest(fn func() uint32) {
	c.randSeed = fn
}
