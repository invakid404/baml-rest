package nativespine

import (
	"fmt"
	"sort"

	"github.com/invakid404/baml-rest/bamlutils/bamlparser"
	"github.com/invakid404/baml-rest/bamlutils/projectdescriptor"
	"github.com/invakid404/baml-rest/internal/nativeschema"
)

// M1FixtureSources is the representative .baml corpus for the M1 vertical. It is
// deliberately a small method CLASS demonstrator, not a single hard-coded
// function: one admitted ClassStaticUnary method (Greet) plus one method per
// decline category the classifier detects from source (non-OpenAI provider,
// env-model provenance, a @check constraint, and a fallback strategy client).
// Shared (non-test) so the introspect golden test and the fixture-package
// regeneration test build the exact same descriptor from the exact same source.
var M1FixtureSources = map[string]string{
	"clients.baml": `// M1 fixture clients.
client<llm> GPT4 {
  provider openai
  options {
    model "gpt-4o"
    api_key env.OPENAI_API_KEY
  }
}

client<llm> Claude {
  provider anthropic
  options {
    model "claude-3-5-sonnet"
    api_key env.ANTHROPIC_API_KEY
  }
}

client<llm> EnvModel {
  provider openai
  options {
    model env.OPENAI_MODEL
    api_key env.OPENAI_API_KEY
  }
}

client<llm> Fallback {
  provider baml-fallback
  options {
    strategy [GPT4, Claude]
  }
}
`,
	"types.baml": `// M1 fixture types.
class Greeting {
  text string
  formal bool
}

class Scored {
  score int @check(positive, {{ this.score > 0 }})
}
`,
	"functions.baml": `// M1 fixture functions.

// Admitted: static unary, primitive/class I/O, one literal-model OpenAI client.
function Greet(name: string, formal: bool) -> Greeting {
  client GPT4
  prompt #"Greet {{ name }}. Formal: {{ formal }}"#
}

// Declined: provider is not openai.
function AnthropicGreet(name: string) -> string {
  client Claude
  prompt #"Hi {{ name }}"#
}

// Declined: model provenance is env, not a literal.
function EnvGreet(name: string) -> string {
  client EnvModel
  prompt #"Hey {{ name }}"#
}

// Declined: return schema carries a @check constraint.
function ScoreName(name: string) -> Scored {
  client GPT4
  prompt #"Score {{ name }}"#
}

// Declined: client uses a fallback strategy.
function FallbackGreet(name: string) -> string {
  client Fallback
  prompt #"Yo {{ name }}"#
}
`,
}

// BuildFromSource runs the same pipeline cmd/introspect runs — parse each .baml
// file, build the static schemas / client configs / prompt descriptors via
// internal/nativeschema, then classify via BuildProjectDescriptor — and returns
// the resulting Project. Files are processed in sorted-name order so the result
// is independent of map iteration. It is the test-support entry that lets any
// package reconstruct the neutral artifact from raw source.
func BuildFromSource(sources map[string]string) (projectdescriptor.Project, error) {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)

	files := make([]nativeschema.SourceFile, 0, len(names))
	for _, name := range names {
		f, err := bamlparser.ParseBytes(name, []byte(sources[name]))
		if err != nil {
			return projectdescriptor.Project{}, fmt.Errorf("nativespine: parse %s: %w", name, err)
		}
		files = append(files, nativeschema.SourceFile{File: f, Path: name})
	}

	schemas, schemaDeclines := nativeschema.BuildStaticSchemas(files)
	clientConfigs := nativeschema.BuildClientConfigs(files)
	clientProvider := map[string]string{}
	for clientName, cc := range clientConfigs {
		if cc.Present {
			clientProvider[clientName] = cc.Provider
		}
	}
	funcs, preDeclines, preDeclineFeatures := nativeschema.BuildPromptDescriptorsWithFeatures(files, schemas, schemaDeclines, clientProvider, clientConfigs)
	clients, retries, strategies := nativeschema.BuildClientGraph(files)

	return BuildProjectDescriptor(SourceFacts{
		Funcs:              funcs,
		PreDeclines:        preDeclines,
		PreDeclineFeatures: preDeclineFeatures,
		Clients:            clients,
		RetryPolicies:      retries,
		Strategies:         strategies,
		Templates:          nativeschema.BuildProjectTemplates(files),
	}), nil
}
