// Claimed static corpus for the Phase 3 slice-2 differential oracle. Every
// function here stays strictly inside the narrow static surface the native
// renderer claims (literal text + direct primitive interpolation, fixed
// text-only role blocks, and bare ctx.output_format). The oracle drives each
// function's generated Request.<Function> method and compares BAML's rendered
// provider body byte-for-byte with native RenderStatic.

// Literal text + direct string interpolation, rendered as a Completion (no
// roles). Exercises raw-string dedent/trim and KindCompletion.
function StaticCompletion(topic: string) -> string {
  client StaticOracleClient
  prompt #"
    Write a short, plain note about {{ topic }}.
  "#
}

// Exact string/int/float/bool binding and MiniJinja scalar display in a single
// Completion body. The oracle drives several value rows (ASCII/Unicode strings,
// int extremes, finite floats, both bools) so the byte-exact scalar formatting
// of native minijinja-go is proven against BAML's minijinja fork.
function StaticPrimitiveArgs(text: string, count: int, ratio: float, flag: bool) -> string {
  client StaticOracleClient
  prompt #"
    text={{ text }}
    count={{ count }}
    ratio={{ ratio }}
    flag={{ flag }}
  "#
}

// Text-only chat with fixed system -> user -> assistant roles. Uses the
// positional _.role("...") form and the _.chat(role="...") alias/kwarg form,
// each message carrying one literal line plus one primitive interpolation.
function StaticRoleChat(topic: string, count: int) -> string {
  client StaticOracleClient
  prompt #"
    {{ _.role("system") }}
    You are a concise assistant.
    {{ _.role("user") }}
    Tell me about {{ topic }}.
    {{ _.chat(role="assistant") }}
    Here are {{ count }} facts.
  "#
}

// Bare ctx.output_format inside a chat message, alongside a primitive
// interpolation. Rendered from the function's retained StaticAnswer return
// descriptor with default native output-format options.
function StaticOutputFormat(topic: string) -> StaticAnswer {
  client StaticOracleClient
  prompt #"
    {{ _.role("user") }}
    Summarize {{ topic }} for me.
    {{ ctx.output_format }}
  "#
}

// Bare ctx.output_format inside a Completion (no roles), proving the completion
// `prompt` body preserves the rendered output-format block exactly.
function StaticCompletionOutputFormat(topic: string) -> StaticAnswer {
  client StaticOracleClient
  prompt #"
    Summarize {{ topic }}.
    {{ ctx.output_format }}
  "#
}
