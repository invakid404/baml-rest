// Simple two-field class return for the bare ctx.output_format corpus. Two
// scalar fields keep the rendered output-format block small and exact (field
// order, newlines, and the "Answer in JSON..." prefix) so it can be compared
// byte-for-byte between BAML's renderer and the native
// internal/schema/outputformat renderer.
//
// NOTE: no enum, no @@dynamic, no @skip, no media, no template_string anywhere
// in this project — the claimed corpus must stay inside the narrow static
// surface, and a project template_string would attach to every function and
// make the whole claimed set decline (FeatureTemplateString).
class StaticAnswer {
  answer string
  confidence int
}
