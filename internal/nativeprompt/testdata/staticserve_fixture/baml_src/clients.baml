// One stock OpenAI client with a fake model + fake API key, whose base_url is the
// fixed LOOPBACK the de-BAML static SERVE e2e/cutover fixture server binds
// (127.0.0.1:17654). Unlike the render-only static_oracle (which never sends, so it
// uses a `.invalid` host), this ctx-first staticserve_fixture is DRIVEN end-to-end:
// its generated /call serve seam opens a real socket, so the descriptor's baked
// base_url must reach the loopback capture server. The client name maps to provider
// "openai" in the native leg's fixture client->provider constant.
client<llm> StaticOracleClient {
  provider openai
  options {
    model "fake-static-oracle-model"
    api_key "fake-static-oracle-key"
    base_url "http://127.0.0.1:17654/v1"
  }
}

// A second client carrying an ordered request_body plus a body-affecting
// top-level option. NO function references it, so BAML never builds a body from
// it; it exists ONLY so the de-BAML P4a descriptor extractor (BuildClientConfigs)
// has a source-map-fenced fixture proving ordered/typed request_body retention,
// last-wins duplicate resolution, nested object/list values, and the
// transport-only vs body-affecting split. P4a intentionally DECLINES a nonempty
// request_body, so this is also a native decline fixture (FeatureRequestBody).
client<llm> RichBodyClient {
  provider openai
  options {
    model "rich-body-model"
    api_key "rich-body-key"
    base_url "https://rich-body.invalid/v1"
    temperature 0.5
    request_body {
      top_p 0.9
      seed 1
      metadata {
        tag "unit"
        rank 3
      }
      stop ["a", "b"]
      seed 7
      stream_hint true
    }
  }
}

// A client declaring an EXPLICIT EMPTY request_body {} block. NO function
// references it. It is source-map-fenced proof that the P4a descriptor extractor
// records RequestBodyPresent==true with zero entries for an explicit empty block
// — which is NOT equivalent to an absent request_body: stock BAML v0.223 emits
// "request_body":{} in the body whenever the block is present (verified against
// the oracle), so native DECLINES it (FeatureRequestBody) rather than emit a
// model+messages body that omits the request_body key.
client<llm> EmptyBodyClient {
  provider openai
  options {
    model "empty-body-model"
    api_key "empty-body-key"
    base_url "https://empty-body.invalid/v1"
    request_body {}
  }
}

// A client whose model is a REGULAR string literal carrying an escape sequence
// (\t). NO function references it. Source-map-fenced proof that stock BAML v0.223
// DECODES a regular string literal (verified against the oracle: it emits
// "model":"gpt\t4" with a REAL tab) while the passive descriptor retains the raw
// "gpt\\t4" bytes — so native cannot prove the decoded model and DECLINES
// (FeatureModelEscape). A raw-string literal (#"..."#) would be verbatim and
// admitted; this regular-literal-with-escape case must not be.
client<llm> EscapedModelClient {
  provider openai
  options {
    model "gpt\t4"
    api_key "escaped-model-key"
    base_url "https://escaped-model.invalid/v1"
  }
}
