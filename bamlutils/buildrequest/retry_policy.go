package buildrequest

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/retry"
)

// ResolveRetryPolicy determines the retry policy for a function.
// Resolution order, in priority:
//
//  1. Per-request override from adapter.RetryConfig()
//     (__baml_options__.retry).
//  2. Runtime client_registry retry_policy lookup, scoped by primary:
//     - When `reg.Primary` is non-nil and non-empty, ONLY consider the
//       primary client's RetryPolicy (look up `reg.Clients` for the entry
//       whose Name == *reg.Primary; if that entry has a non-nil
//       RetryPolicy, dereference it via introspectedPolicies and return).
//       The function-default runtime client (defaultClientName) is NOT
//       consulted — the primary is the one actually being used for
//       streaming, so inheriting a different client's retry config would
//       cross wires. If the primary entry is missing or has no
//       RetryPolicy, fall through to step 3.
//     - When primary is nil or "", consult the function-default runtime
//       client: scan reg.Clients for the entry whose Name ==
//       defaultClientName; if found and its RetryPolicy is non-nil,
//       dereference via introspectedPolicies and return.
//  3. Static introspected default: introspectedPolicies[introspectedPolicyName]
//     where introspectedPolicyName comes from FunctionRetryPolicy[method].
//
// introspectedPolicyName is the policy name from FunctionRetryPolicy[method].
// introspectedPolicies is the full RetryPolicies map from introspection.
func ResolveRetryPolicy(
	adapter bamlutils.Adapter,
	defaultClientName string,
	introspectedPolicyName string,
	introspectedPolicies map[string]*retry.Policy,
) *retry.Policy {
	// 1. Per-request override takes highest priority
	if rc := adapter.RetryConfig(); rc != nil {
		return RetryConfigToPolicy(rc)
	}

	// 2. Client-level retry_policy from runtime client_registry.
	if reg := adapter.OriginalClientRegistry(); reg != nil {
		// When primary is set (and non-empty), only check the primary
		// client's retry_policy. Do NOT fall through to the default
		// client — the primary client is the one actually being used
		// for streaming, so inheriting a different client's retry
		// policy would be incorrect. If the primary has no
		// retry_policy, skip straight to the introspected default.
		// Present-empty primary (`"primary": ""`) is treated the same
		// as nil here (matching the adapter SetClientRegistry guard)
		// so the function's default client retry policy is consulted
		// rather than skipped on a no-op payload.
		if reg.Primary != nil && *reg.Primary != "" {
			for _, client := range reg.Clients {
				if client == nil {
					continue
				}
				if client.Name == *reg.Primary && client.RetryPolicy != nil {
					if p, ok := introspectedPolicies[*client.RetryPolicy]; ok {
						return p
					}
				}
			}
			// Primary is set but has no retry_policy — fall through to
			// introspected default (step 3), NOT to the default client.
		} else if defaultClientName != "" {
			// No primary set — check the function's default client name
			for _, client := range reg.Clients {
				if client == nil {
					continue
				}
				if client.Name == defaultClientName && client.RetryPolicy != nil {
					if p, ok := introspectedPolicies[*client.RetryPolicy]; ok {
						return p
					}
				}
			}
		}
	}

	// 3. Static introspected default.
	if introspectedPolicyName != "" {
		return introspectedPolicies[introspectedPolicyName]
	}

	return nil
}

// EncodeRetryPolicy formats a retry.Policy into the compact string used by
// the Metadata.RetryPolicy field. Examples:
//
//	"const:200ms" — constant delay
//	"exp:200ms:1.5:10s" — exponential backoff (base:multiplier:max)
//	"" — nil or unresolved policy
func EncodeRetryPolicy(p *retry.Policy) string {
	if p == nil {
		return ""
	}
	cfg := p.StrategyConfig
	if cfg == nil {
		return ""
	}
	switch cfg.Type {
	case "constant_delay":
		return fmt.Sprintf("const:%dms", cfg.DelayMs)
	case "exponential_backoff":
		return fmt.Sprintf("exp:%dms:%.2f:%dms", cfg.DelayMs, cfg.Multiplier, cfg.MaxDelayMs)
	}
	return cfg.Type
}

// RetryConfigToPolicy converts a bamlutils.RetryConfig (from __baml_options__.retry)
// into a retry.Policy that the orchestrator can use.
func RetryConfigToPolicy(rc *bamlutils.RetryConfig) *retry.Policy {
	if rc == nil {
		return nil
	}
	p := &retry.Policy{
		MaxRetries: rc.MaxRetries,
		StrategyConfig: &retry.StrategyConfig{
			Type:       rc.Strategy,
			DelayMs:    rc.DelayMs,
			Multiplier: rc.Multiplier,
			MaxDelayMs: rc.MaxDelayMs,
		},
	}
	p.ResolveStrategy()
	return p
}
