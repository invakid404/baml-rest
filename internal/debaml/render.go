// Package debaml provides the root-module wiring for native de-BAML
// behaviour. It lives in the root module because it imports the root
// internal packages internal/schema and internal/schema/outputformat,
// which the separate worker/ and dynclient/ modules cannot import across
// the module boundary.
//
// The single export, Render, has the public-typed signature
// bamlutils.DeBAMLRenderFunc. The root-module binaries (cmd/serve,
// cmd/worker) and root-module callers of dynclient (e.g. integration
// tests) pass it as a callback into worker.Config / dynclient so the
// generated dynclient adapter can drive the native ctx.output_format
// renderer without importing internal/schema itself.
package debaml

import (
	"github.com/invakid404/baml-rest/bamlutils"
	"github.com/invakid404/baml-rest/internal/schema"
	"github.com/invakid404/baml-rest/internal/schema/outputformat"
)

// Render is the bamlutils.DeBAMLRenderFunc implementation: it lowers the
// dynamic output schema and renders the native output-format block with
// zero-value options (BAML RenderOptions::default, matching the dynamic
// template's bare ctx.output_format). It returns an error on any genuine
// lowering/rendering failure so the caller falls back to BAML-as-today.
func Render(s *bamlutils.DynamicOutputSchema) (string, error) {
	bundle, err := schema.FromDynamicOutputSchema(s, schema.BuildOptions{})
	if err != nil {
		return "", err
	}
	return outputformat.Render(bundle, outputformat.Options{})
}

// Compile-time assertion that Render satisfies the public callback type.
var _ bamlutils.DeBAMLRenderFunc = Render
