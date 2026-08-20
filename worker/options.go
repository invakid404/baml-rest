package worker

import (
	"fmt"

	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/bamlutils/clientdefaults"
	"github.com/invakid404/baml-rest/bamlutils/trustedclients"
	"github.com/invakid404/baml-rest/bamlutils/urlrewrite"
)

// workerBamlOptions wraps the options for JSON parsing.
type workerBamlOptions struct {
	Options *bamlutils.BamlOptions `json:"__baml_options__,omitempty"`
}

// apply installs the request-level option overrides on adapter. The
// deployment-wide client defaults, base-URL rewrites and approved
// configuration classes are passed in so the handler owns the merged
// config rather than reaching into package globals. Nil defaults is
// fine — clientdefaults.Config.Apply is nil-safe; nil baseURLRewrites
// skips the rewrite pass; a nil trusted set seals nothing.
func (o *workerBamlOptions) apply(adapter bamlutils.Adapter, defaults *clientdefaults.Config, baseURLRewrites []urlrewrite.Rule, trusted *trustedclients.Set) error {
	if o.Options == nil {
		return nil
	}

	if o.Options.ClientRegistry != nil {
		// Apply URL rewrite rules to custom client base_url options
		if len(baseURLRewrites) > 0 {
			rewriteClientBaseURLs(o.Options.ClientRegistry, baseURLRewrites)
		}
		// Merge deployment-wide defaults *after* URL rewrites (so injected
		// values aren't accidentally URL-rewritten) and *before*
		// SetClientRegistry (so BAML sees the merged options).
		defaults.Apply(o.Options.ClientRegistry)
		// De-BAML serving cutover S3a — the TRUSTED-CONFIGURATION SEAL, applied
		// LAST so what it seals is what BAML is actually handed.
		//
		// This is the ONE place a request registry becomes the adapter's
		// registry, which makes it the only place that can honestly answer "did
		// the DEPLOYMENT configure this client, or did the caller?". Seal walks
		// the registry and, for a client the request did no more than NAME,
		// installs the deployment's approved provider/options and marks it. Every
		// other client — including one whose caller-supplied values happen to
		// match an approved class byte for byte — is left exactly as it arrived
		// and carries no identity.
		//
		// With nothing declared (the shipped default) this is a no-op and no
		// request is altered in any way.
		trusted.Seal(o.Options.ClientRegistry)
		if err := adapter.SetClientRegistry(o.Options.ClientRegistry); err != nil {
			return fmt.Errorf("failed to set client registry: %w", err)
		}
	}

	if o.Options.TypeBuilder != nil {
		if err := adapter.SetTypeBuilder(o.Options.TypeBuilder); err != nil {
			return fmt.Errorf("failed to set type builder: %w", err)
		}
	}

	if o.Options.Retry != nil {
		adapter.SetRetryConfig(o.Options.Retry)
	}

	// Always pass IncludeReasoning through (even when false) so the
	// adapter reflects an explicit per-request choice. Default value
	// leaves the structured reasoning channel empty.
	adapter.SetIncludeReasoning(o.Options.IncludeReasoning)

	// Carry the original dynamic output schema (may be nil) so the
	// dynamic BuildRequest seam can drive the native ctx.output_format
	// renderer under BAML_REST_USE_DEBAML. Stored unconditionally; the
	// seam only consults it when the de-BAML flag is on, so a nil schema
	// or a disabled flag both leave the provider request BAML-as-today.
	adapter.SetDeBAMLOutputSchema(o.Options.OutputSchema)

	return nil
}

// rewriteClientBaseURLs applies URL rewrite rules to the base_url option
// of each client in the registry. This allows remapping external URLs
// (e.g., https://llm.mandel.ai) to internal URLs (e.g., http://litellm:4000)
// for custom clients passed via __baml_options__.
func rewriteClientBaseURLs(registry *bamlutils.ClientRegistry, rules []urlrewrite.Rule) {
	for _, client := range registry.Clients {
		if client == nil || client.Options == nil {
			continue
		}
		baseURL, ok := client.Options["base_url"]
		if !ok {
			continue
		}
		urlStr, ok := baseURL.(string)
		if !ok || urlStr == "" {
			continue
		}
		rewritten := urlrewrite.ApplyToURL(urlStr, rules)
		if rewritten != urlStr {
			client.Options["base_url"] = rewritten
		}
	}
}
