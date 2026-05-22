//go:build integration

package testutil

import (
	"fmt"

	"github.com/invakid404/baml-rest/dynclient"
)

// NewDynclient constructs a public dynclient.Client wired with a base-URL
// rewrite that maps the container-internal mockllm URL to the
// host-reachable URL the test process can hit. Without this rewrite the
// public client would try to dial `http://mockllm:8080`, which is only
// resolvable inside the baml-rest container.
//
// Extracted from integration/dynclient_test.go so that other integration
// test files (e.g. the bamlfuzz dynamic oracle) can reuse the same
// rewrite plumbing without duplicating it.
func NewDynclient(env *TestEnvironment, opts ...dynclient.Option) (*dynclient.Client, error) {
	if env == nil {
		return nil, fmt.Errorf("testutil: NewDynclient requires a non-nil TestEnvironment")
	}
	rewrites := []dynclient.BaseURLRewriteRule{
		{From: env.MockLLMInternal, To: env.MockLLMURL},
	}
	all := append([]dynclient.Option{dynclient.WithBaseURLRewrites(rewrites)}, opts...)
	return dynclient.New(all...)
}

// DynRegistry reshapes a testutil ClientRegistry into the dynclient
// type. Wire JSON is identical between the two flavors; the Go types
// differ because dynclient lives in a separate module and cannot import
// testutil.
func DynRegistry(reg *ClientRegistry) *dynclient.ClientRegistry {
	if reg == nil {
		return nil
	}
	out := &dynclient.ClientRegistry{Primary: reg.Primary}
	for _, c := range reg.Clients {
		if c == nil {
			continue
		}
		provider := ""
		if c.Provider != nil {
			provider = *c.Provider
		}
		out.Clients = append(out.Clients, &dynclient.ClientProperty{
			Name:        c.Name,
			Provider:    provider,
			RetryPolicy: c.RetryPolicy,
			Options:     c.Options,
		})
	}
	return out
}
