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
// the streaming-branch contract: when emitBAMLHTTPRequestConversion is
// called with nil postProcess (as the streaming _buildRequest emit
// does), the generated body must NOT contain MaybeAttachBedrockAuth.
// PR1-bedrock breadcrumb (#243).
//
// PR 3 will wire streaming through the same SigV4 hook via a different
// codegen path; until then, the streaming branch must stay untouched
// because supportedProviders["aws-bedrock"] is still false.
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
// must propagate any error returned by it. PR1-bedrock breadcrumb (#243).
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

// TestEmitBuildCallRequest_EmitsMaybeAttachBedrockAuth pins the
// real call-branch emission (not just the helper): the generated
// _buildCallRequest closure must invoke llmhttp.MaybeAttachBedrockAuth
// after BAML's Request.<Method> builds the body. A regression that
// dropped the postProcess argument from emitBuildCallRequest's
// emitBAMLHTTPRequestConversion call would compile cleanly (the
// parameter is variadic-shaped via nil-safety) and silently revert to
// streaming-style "no attach". This test catches that.
//
// Gated on introspected.Request being non-nil because emitBuildCallRequest
// no-ops for adapters without the BAML Request API. The test restores
// the singleton on cleanup so adjacent tests are not perturbed.
func TestEmitBuildCallRequest_EmitsMaybeAttachBedrockAuth(t *testing.T) {
	savedRequest := introspected.Request
	t.Cleanup(func() { introspected.Request = savedRequest })
	introspected.Request = struct{}{}

	me := newBedrockTestMethodEmitter(t)
	me.emitBuildCallRequest()
	rendered := me.g.out.GoString()

	if !strings.Contains(rendered, "llmhttp.MaybeAttachBedrockAuth(ctx, req)") {
		t.Errorf("emitBuildCallRequest must emit llmhttp.MaybeAttachBedrockAuth(ctx, req); rendered:\n%s", rendered)
	}
	// The attach lives inside the buildRequestFn closure, between the
	// httpReq construction and the closure's final return. Pin both
	// neighbours positionally so a refactor that moves the attach
	// outside the closure (e.g. into the orchestrator) fails this
	// test rather than silently changing semantics.
	bedrockIdx := strings.Index(rendered, "llmhttp.MaybeAttachBedrockAuth(ctx, req)")
	buildFnIdx := strings.Index(rendered, "buildRequestFn :=")
	if buildFnIdx < 0 || bedrockIdx < 0 || !(buildFnIdx < bedrockIdx) {
		t.Errorf("attach must sit inside the buildRequestFn closure; positions buildFn=%d attach=%d", buildFnIdx, bedrockIdx)
	}
}

// TestEmitBuildRequest_DoesNotEmitMaybeAttachBedrockAuth pins the
// streaming-branch contract: the streaming codegen path
// (emitBuildRequest) must NOT emit MaybeAttachBedrockAuth.
//
// Bedrock streaming is gated by supportedProviders["aws-bedrock"]
// (still false as of #243 PR 1) and lands in PR 3, where it will go
// through a different codegen path (URL mutation /converse →
// /converse-stream plus AWS event-stream decoding). Emitting the
// attach on the streaming path now would silently sign requests that
// can't yet handle the response.
func TestEmitBuildRequest_DoesNotEmitMaybeAttachBedrockAuth(t *testing.T) {
	savedStreamRequest := introspected.StreamRequest
	t.Cleanup(func() { introspected.StreamRequest = savedStreamRequest })
	introspected.StreamRequest = struct{}{}

	me := newBedrockTestMethodEmitter(t)
	me.emitBuildRequest()
	rendered := me.g.out.GoString()

	if strings.Contains(rendered, "MaybeAttachBedrockAuth") {
		t.Errorf("emitBuildRequest (streaming path) must not emit MaybeAttachBedrockAuth — streaming is PR 3 territory; rendered:\n%s", rendered)
	}
}
