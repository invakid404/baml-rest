package introspected

// NOTE: this file will be overwritten during build

// Stream is the BAML streaming client instance
var Stream = &struct{}{}

// StreamMethods is a map from method name to argument names
var StreamMethods = map[string][]string{}

// SyncMethods maps sync function names to their argument names
var SyncMethods = map[string][]string{}

// SyncFuncs maps sync function names to their function values (for reflection)
var SyncFuncs = map[string]any{}

// Parse is the parse API for parsing raw LLM responses into final types
var Parse = &struct{}{}

// ParseMethods is a set of method names available on Parse
var ParseMethods = map[string]struct{}{}

// ParseStream is the parse_stream API for parsing raw LLM responses into partial/stream types
var ParseStream = &struct{}{}

// ParseStreamMethods is a set of method names available on ParseStream
var ParseStreamMethods = map[string]struct{}{}
