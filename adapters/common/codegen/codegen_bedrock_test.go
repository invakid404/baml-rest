package codegen

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/dave/jennifer/jen"

	"github.com/invakid404/baml-rest/adapters/common"
	"github.com/invakid404/baml-rest/introspected"
)

// TestEmitBAMLHTTPRequestConversion_NilPostProcess_NoBedrockAttach pins
// the SSE-path contract: when emitBAMLHTTPRequestConversion is called
// with nil postProcess (as the non-bedrock streaming buildRequestFn
// emit does), the generated body must NOT contain
// MaybeAttachBedrockAuth. The bedrock streaming path uses its own
// closure (buildBedrockStreamRequestFn) with
// emitBedrockStreamPostProcess; this assertion guards the SSE
// closure from accidentally inheriting AWS signing.
func TestEmitBAMLHTTPRequestConversion_NilPostProcess_NoBedrockAttach(t *testing.T) {
	f := jen.NewFilePathName("github.com/example/test", "test")
	f.Func().Id("emit").Params().Params(jen.Op("*").Id("Request"), jen.Error()).BlockFunc(func(g *jen.Group) {
		emitBAMLHTTPRequestConversion(g, nil)
	})
	rendered := f.GoString()

	if strings.Contains(rendered, "MaybeAttachBedrockAuth") {
		t.Errorf("nil postProcess (streaming path) must not emit MaybeAttachBedrockAuth; rendered:\n%s", rendered)
	}
	// Sanity: the conversion body itself must still be emitted (the
	// negative assertion above would also pass for an empty function,
	// which would be a different regression).
	if !strings.Contains(rendered, `req := &llmhttp.Request`) {
		t.Errorf("expected the req := &llmhttp.Request literal in rendered output; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "return req, nil") {
		t.Errorf("expected `return req, nil` at the end of the conversion body; got:\n%s", rendered)
	}
}

// TestEmitBAMLHTTPRequestConversion_BedrockPostProcess pins the
// call-branch contract: when emitBAMLHTTPRequestConversion is called
// with emitMaybeAttachBedrockAuth as postProcess (as the non-streaming
// _buildCallRequest emit does), the generated body must contain the
// MaybeAttachBedrockAuth call with the (ctx, req) argument shape and
// must propagate any error returned by it.
func TestEmitBAMLHTTPRequestConversion_BedrockPostProcess(t *testing.T) {
	f := jen.NewFilePathName("github.com/example/test", "test")
	f.Func().Id("emit").Params().Params(jen.Op("*").Id("Request"), jen.Error()).BlockFunc(func(g *jen.Group) {
		emitBAMLHTTPRequestConversion(g, emitMaybeAttachBedrockAuth)
	})
	rendered := f.GoString()

	if !strings.Contains(rendered, "llmhttp.MaybeAttachBedrockAuth(ctx, req)") {
		t.Errorf("call-branch postProcess must emit llmhttp.MaybeAttachBedrockAuth(ctx, req); rendered:\n%s", rendered)
	}
	// The attach must happen AFTER req is built (so MaybeAttachBedrockAuth
	// can read req.URL) and BEFORE the final return (so the attached
	// AWSAuth flows out of the closure). Pin both via positional checks.
	reqAssignIdx := strings.Index(rendered, `req := &llmhttp.Request`)
	attachIdx := strings.Index(rendered, "llmhttp.MaybeAttachBedrockAuth")
	returnIdx := strings.LastIndex(rendered, "return req, nil")
	if reqAssignIdx < 0 || attachIdx < 0 || returnIdx < 0 {
		t.Fatalf("expected req-assign, attach, and return sites in rendered output; got:\n%s", rendered)
	}
	if !(reqAssignIdx < attachIdx && attachIdx < returnIdx) {
		t.Errorf("attach must sit between req-assign and return; positions reqAssign=%d attach=%d return=%d", reqAssignIdx, attachIdx, returnIdx)
	}
	// Error propagation: the attach return must surface the auth error
	// rather than swallowing it. Without this guard, MaybeAttachBedrockAuth
	// failures (missing creds, malformed URL) would silently flow through
	// and surface as opaque SigV4 errors at signing time.
	if !strings.Contains(rendered, "return nil, authErr") {
		t.Errorf("call-branch postProcess must propagate the auth error via `return nil, authErr`; rendered:\n%s", rendered)
	}
}

// bedrockTestSyncFunc is the minimal sync-method signature used by
// the emitBuildCallRequest / emitBuildRequest pinning tests: ctx +
// variadic opts -> (string, error). The methodEmitter machinery reads
// syncFuncType via reflect to derive finalType / argCallParam; the
// shape here exercises emit without any input args, which keeps the
// rendered output focused on the buildRequestFn closure itself rather
// than per-argument plumbing.
func bedrockTestSyncFunc(_ context.Context, _ ...string) (string, error) {
	return "", nil
}

// newBedrockTestMethodEmitter constructs a methodEmitter with the
// minimum field set the BuildRequest emitters read, mirroring the
// pattern in TestEmitRouter_RetryResolutionUsesStrategyAwareHelper.
// The output struct's pool-getter / metadata-constructor / unwrap
// names are populated so the closure-internal references resolve;
// finalType / streamType are set to a plain `string` parsedReflectType
// so the newResultFn type-assertion emits compile.
func newBedrockTestMethodEmitter(t *testing.T) *methodEmitter {
	t.Helper()
	g := &generator{
		opts:               Options{SupportsWithClient: true},
		out:                common.MakeFile(),
		supportsWithClient: true,
	}
	stringType := parseReflectType(reflect.TypeOf(""))
	return &methodEmitter{
		g:                          g,
		methodName:                 "GreetUser",
		args:                       nil,
		syncFuncType:               reflect.TypeOf(bedrockTestSyncFunc),
		inputStructName:            "GreetUserInput",
		outputStructName:           "GreetUserOutput",
		poolVarName:                "greetUserOutputPool",
		getterFuncName:             "getGreetUserOutput",
		errorConstructorName:       "newGreetUserOutputError",
		metadataConstructorName:    "newGreetUserOutputMetadata",
		unwrapStreamFuncName:       "unwrapDynamicGreetUserOutputStream",
		unwrapFinalFuncName:        "unwrapDynamicGreetUserOutputFinal",
		noRawMethodName:            "greetUser_noRaw",
		fullMethodName:             "greetUser_full",
		buildRequestMethodName:     "greetUser_buildRequest",
		buildCallRequestMethodName: "greetUser_buildCallRequest",
		finalType:                  stringType,
		finalTypePtr:               jen.Op("*").Add(stringType.statement.Clone()),
		streamType:                 stringType,
		streamTypePtr:              jen.Op("*").Add(stringType.statement.Clone()),
	}
}

// TestEmitBuildCallRequest_EmitsBedrockAuthDispatch pins the real
// call-branch emission (not just the helper): the generated
// _buildCallRequest closure must invoke the client-aware Bedrock
// dispatch — resolve the selected client name, look up
// introspected.BedrockClientOptionsByName, then call
// llmhttp.AttachBedrockAuthForClient with the resolved endpoint and
// region values. A regression that dropped the postProcess argument
// from emitBuildCallRequest's emitBAMLHTTPRequestConversion call would
// compile cleanly (the parameter is variadic-shaped via nil-safety)
// and silently revert to streaming-style "no attach". This test
// catches that.
//
// Gated on introspected.Request being non-nil because emitBuildCallRequest
// no-ops for adapters without the BAML Request API. The test restores
// the singleton on cleanup so adjacent tests are not perturbed.
func TestEmitBuildCallRequest_EmitsBedrockAuthDispatch(t *testing.T) {
	savedRequest := introspected.Request
	t.Cleanup(func() { introspected.Request = savedRequest })
	introspected.Request = struct{}{}

	me := newBedrockTestMethodEmitter(t)
	me.emitBuildCallRequest()
	rendered := me.g.out.GoString()

	// Selected-client resolve: clientOverride first, then fall back to
	// introspected.FunctionClient[<methodName>] for the default client.
	if !strings.Contains(rendered, "selectedClient := clientOverride") {
		t.Errorf("dispatch must resolve selectedClient from clientOverride; rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, `introspected.FunctionClient["GreetUser"]`) {
		t.Errorf("dispatch must fall back to introspected.FunctionClient[%q] when clientOverride is empty; rendered:\n%s", "GreetUser", rendered)
	}
	// BedrockClientOptionsByName lookup keyed on the resolved selected client.
	if !strings.Contains(rendered, "introspected.BedrockClientOptionsByName[selectedClient]") {
		t.Errorf("dispatch must look up introspected.BedrockClientOptionsByName[selectedClient]; rendered:\n%s", rendered)
	}
	// The dispatch must invoke AttachBedrockAuthForClient with the
	// resolved endpoint and region locals. AttachBedrockAuthForClient
	// internally falls through to MaybeAttachBedrockAuth when both
	// values are empty, so the URL-pattern detection contract holds
	// without re-emitting the helper here.
	if !strings.Contains(rendered, "llmhttp.AttachBedrockAuthForClient(ctx, req, llmhttp.BedrockClientAuthOptions{") {
		t.Errorf("dispatch must call llmhttp.AttachBedrockAuthForClient with a BedrockClientAuthOptions struct; rendered:\n%s", rendered)
	}
	for _, optField := range []string{"ClientName:  selectedClient", "EndpointURL: bedrockEndpointURL", "Region:      bedrockRegion", "Credentials: bedrockCreds"} {
		if !strings.Contains(rendered, optField) {
			t.Errorf("dispatch must populate BedrockClientAuthOptions.%s; rendered:\n%s", optField, rendered)
		}
	}
	// The attach lives inside the buildRequestFn closure, between the
	// httpReq construction and the closure's final return. Pin both
	// neighbours positionally so a refactor that moves the attach
	// outside the closure (e.g. into the orchestrator) fails this
	// test rather than silently changing semantics.
	dispatchIdx := strings.Index(rendered, "llmhttp.AttachBedrockAuthForClient(ctx, req")
	buildFnIdx := strings.Index(rendered, "buildRequestFn :=")
	if buildFnIdx < 0 || dispatchIdx < 0 || !(buildFnIdx < dispatchIdx) {
		t.Errorf("dispatch must sit inside the buildRequestFn closure; positions buildFn=%d dispatch=%d", buildFnIdx, dispatchIdx)
	}
}

// TestEmitBuildRequest_EmitsBedrockStreamingClosure pins the
// streaming-branch enablement: when introspected.Request is non-nil
// (v0.219), emitBuildRequest emits a
// buildBedrockStreamRequestFn closure that calls BAML's non-streaming
// Request.<Method>, mutates the URL to /converse-stream, sets the AWS
// event-stream Accept header, and attaches AWSAuth. The closure is
// wired into StreamConfig.BuildBedrockStreamRequest so the
// orchestrator dispatches to it for aws-bedrock providers.
//
// Inverse test below pins that the bedrock streaming closure is NOT
// emitted when introspected.Request is nil (the v0.204/v0.215 case).
func TestEmitBuildRequest_EmitsBedrockStreamingClosure(t *testing.T) {
	savedStreamRequest := introspected.StreamRequest
	savedRequest := introspected.Request
	t.Cleanup(func() {
		introspected.StreamRequest = savedStreamRequest
		introspected.Request = savedRequest
	})
	introspected.StreamRequest = struct{}{}
	introspected.Request = struct{}{}

	me := newBedrockTestMethodEmitter(t)
	me.emitBuildRequest()
	rendered := me.g.out.GoString()

	// Closure exists and is wired into StreamConfig.
	if !strings.Contains(rendered, "buildBedrockStreamRequestFn :=") {
		t.Errorf("emitBuildRequest must declare buildBedrockStreamRequestFn; rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "BuildBedrockStreamRequest: buildBedrockStreamRequestFn") {
		t.Errorf("StreamConfig must wire BuildBedrockStreamRequest: buildBedrockStreamRequestFn; rendered:\n%s", rendered)
	}
	// The closure calls BAML's non-streaming Request.<Method>, not
	// StreamRequest — BAML's StreamRequest errors for aws-bedrock,
	// which is the upstream gate baml-rest works around here.
	// (jen renders the import as `bamlclient` — the imported package
	// alias from GeneratedClientPkg.)
	if !strings.Contains(rendered, "bamlclient.Request.GreetUser") {
		t.Errorf("bedrock streaming closure must call bamlclient.Request.<Method>; rendered:\n%s", rendered)
	}
	// URL mutation: /converse → /converse-stream via strings.Replace.
	if !strings.Contains(rendered, `strings.Replace(req.URL, "/converse", "/converse-stream", 1)`) {
		t.Errorf("bedrock streaming closure must mutate /converse → /converse-stream; rendered:\n%s", rendered)
	}
	// Accept header set to the AWS event-stream content type.
	if !strings.Contains(rendered, `req.Headers["Accept"] = llmhttp.AWSStreamContentType`) {
		t.Errorf("bedrock streaming closure must set req.Headers[\"Accept\"] = llmhttp.AWSStreamContentType; rendered:\n%s", rendered)
	}
	// Auth attach uses the client-aware dispatch (which internally
	// falls through to MaybeAttachBedrockAuth for the default-endpoint
	// case) — the streaming closure picks up the same per-client
	// endpoint_url + region overrides as the call closure.
	if !strings.Contains(rendered, "llmhttp.AttachBedrockAuthForClient(ctx, req, llmhttp.BedrockClientAuthOptions{") {
		t.Errorf("bedrock streaming closure must call llmhttp.AttachBedrockAuthForClient with a BedrockClientAuthOptions struct; rendered:\n%s", rendered)
	}
	for _, optField := range []string{"ClientName:  selectedClient", "EndpointURL: bedrockEndpointURL", "Region:      bedrockRegion", "Credentials: bedrockCreds"} {
		if !strings.Contains(rendered, optField) {
			t.Errorf("bedrock streaming closure must populate BedrockClientAuthOptions.%s; rendered:\n%s", optField, rendered)
		}
	}
	if !strings.Contains(rendered, "introspected.BedrockClientOptionsByName[selectedClient]") {
		t.Errorf("bedrock streaming closure must look up introspected.BedrockClientOptionsByName[selectedClient]; rendered:\n%s", rendered)
	}
	// Order invariant: URL mutation /converse → /converse-stream must
	// emit BEFORE the dispatch so AttachBedrockAuthForClient (and any
	// endpoint_url override it routes to) joins on the streaming path.
	mutationIdx := strings.Index(rendered, `strings.Replace(req.URL, "/converse", "/converse-stream", 1)`)
	dispatchIdx := strings.Index(rendered, "llmhttp.AttachBedrockAuthForClient(ctx, req")
	if mutationIdx < 0 || dispatchIdx < 0 || !(mutationIdx < dispatchIdx) {
		t.Errorf("URL mutation must precede the bedrock dispatch in the stream closure; positions mutation=%d dispatch=%d", mutationIdx, dispatchIdx)
	}
}

// TestEmitBedrockAuthDispatchFor_Shape pins the standalone shape of
// the dispatch helper so any future tweak to the resolve-name +
// look-up + dispatch sequence surfaces immediately rather than only
// via the closure-emission tests above. The shape is the load-bearing
// contract that makes per-client `endpoint_url` parity work without
// reading from BAML's HTTPRequest surface.
func TestEmitBedrockAuthDispatchFor_Shape(t *testing.T) {
	f := jen.NewFilePathName("github.com/example/test", "test")
	f.Func().Id("emit").Params(jen.Id("clientOverride").String()).Params(jen.Op("*").Id("Request"), jen.Error()).BlockFunc(func(g *jen.Group) {
		emitBedrockAuthDispatchFor("MyMethod")(g)
	})
	rendered := f.GoString()

	wantSubstrings := []string{
		"selectedClient := clientOverride",
		`introspected.FunctionClient["MyMethod"]`,
		"introspected.BedrockClientOptionsByName[selectedClient]",
		"bedrockOpts.EndpointURL.Resolve()",
		"bedrockOpts.Region.Resolve()",
		"bedrockOpts.Credentials.AccessKeyID.Resolve()",
		"bedrockOpts.Credentials.SecretAccessKey.Resolve()",
		"bedrockOpts.Credentials.SessionToken.Resolve()",
		"bedrockOpts.Credentials.Profile.Resolve()",
		"llmhttp.AttachBedrockAuthForClient(ctx, req, llmhttp.BedrockClientAuthOptions{",
		"ClientName:  selectedClient",
		"EndpointURL: bedrockEndpointURL",
		"Region:      bedrockRegion",
		"Credentials: bedrockCreds",
		"return nil, authErr",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(rendered, want) {
			t.Errorf("dispatch emission missing %q; rendered:\n%s", want, rendered)
		}
	}
}

// TestEmitBuildRequest_NoBedrockClosureWithoutRequest pins the
// v0.204/v0.215 gate: emitBuildRequest must NOT emit the bedrock
// streaming closure when introspected.Request is nil — there's no
// baml_client.Request symbol to call. StreamConfig's
// BuildBedrockStreamRequest field stays nil; the orchestrator's
// up-front validation then rejects aws-bedrock provider
// configurations on those adapters.
func TestEmitBuildRequest_NoBedrockClosureWithoutRequest(t *testing.T) {
	savedStreamRequest := introspected.StreamRequest
	savedRequest := introspected.Request
	t.Cleanup(func() {
		introspected.StreamRequest = savedStreamRequest
		introspected.Request = savedRequest
	})
	introspected.StreamRequest = struct{}{}
	introspected.Request = nil

	me := newBedrockTestMethodEmitter(t)
	me.emitBuildRequest()
	rendered := me.g.out.GoString()

	if strings.Contains(rendered, "buildBedrockStreamRequestFn") {
		t.Errorf("emitBuildRequest must not declare buildBedrockStreamRequestFn when introspected.Request is nil; rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "BuildBedrockStreamRequest: nil") {
		t.Errorf("StreamConfig must wire BuildBedrockStreamRequest: nil when introspected.Request is nil; rendered:\n%s", rendered)
	}
}
