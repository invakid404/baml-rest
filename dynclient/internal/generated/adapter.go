package generated

import (
	"context"
	"fmt"
	goconcurrentqueue "github.com/enriquebris/goconcurrentqueue"
	gorecovery "github.com/gregwebs/go-recovery"
	bamlutils "github.com/invakid404/baml-rest/bamlutils"
	buildrequest "github.com/invakid404/baml-rest/bamlutils/buildrequest"
	llmhttp "github.com/invakid404/baml-rest/bamlutils/llmhttp"
	retry "github.com/invakid404/baml-rest/bamlutils/retry"
	sse "github.com/invakid404/baml-rest/bamlutils/sse"
	pkg "github.com/invakid404/baml-rest/dynclient/baml-patched/engine/language_client_go/pkg"
	adapter "github.com/invakid404/baml-rest/dynclient/internal/generated/adapter"
	bamlclient "github.com/invakid404/baml-rest/dynclient/internal/generated/baml_client"
	streamtypes "github.com/invakid404/baml-rest/dynclient/internal/generated/baml_client/stream_types"
	types "github.com/invakid404/baml-rest/dynclient/internal/generated/baml_client/types"
	introspected "github.com/invakid404/baml-rest/dynclient/internal/generated/introspected"
	utils "github.com/invakid404/baml-rest/dynclient/internal/generated/utils"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Baml_Rest_ContentPartMediaInput struct {
	Text          *string               `json:"text"`
	Img           *bamlutils.MediaInput `json:"img"`
	Aud           *bamlutils.MediaInput `json:"aud"`
	Doc           *bamlutils.MediaInput `json:"doc"`
	Vid           *bamlutils.MediaInput `json:"vid"`
	Output_format *bool                 `json:"output_format"`
}

func convertBaml_Rest_ContentPartMediaInput(adapter bamlutils.Adapter, input *Baml_Rest_ContentPartMediaInput) (types.Baml_Rest_ContentPart, error) {
	var result types.Baml_Rest_ContentPart
	result.Text = input.Text
	if input.Img != nil {
		__raw, __err := bamlutils.ConvertMedia(adapter, bamlutils.MediaKindImage, input.Img)
		if __err != nil {
			return result, fmt.Errorf("Img: %w", __err)
		}
		__typed := __raw.(types.Image)
		result.Img = &__typed
	}
	if input.Aud != nil {
		__raw, __err := bamlutils.ConvertMedia(adapter, bamlutils.MediaKindAudio, input.Aud)
		if __err != nil {
			return result, fmt.Errorf("Aud: %w", __err)
		}
		__typed := __raw.(types.Audio)
		result.Aud = &__typed
	}
	if input.Doc != nil {
		__raw, __err := bamlutils.ConvertMedia(adapter, bamlutils.MediaKindPDF, input.Doc)
		if __err != nil {
			return result, fmt.Errorf("Doc: %w", __err)
		}
		__typed := __raw.(types.PDF)
		result.Doc = &__typed
	}
	if input.Vid != nil {
		__raw, __err := bamlutils.ConvertMedia(adapter, bamlutils.MediaKindVideo, input.Vid)
		if __err != nil {
			return result, fmt.Errorf("Vid: %w", __err)
		}
		__typed := __raw.(types.Video)
		result.Vid = &__typed
	}
	result.Output_format = input.Output_format
	return result, nil
}

type Baml_Rest_MessageMediaInput struct {
	Role     string                             `json:"role"`
	Content  *string                            `json:"content"`
	Parts    *[]Baml_Rest_ContentPartMediaInput `json:"parts"`
	Metadata *types.Baml_Rest_MessageMetadata   `json:"metadata"`
}

var baml_Rest_ContentPartSlicePool sync.Pool

func getbaml_Rest_ContentPartSlice(n int) *[]types.Baml_Rest_ContentPart {
	if v := baml_Rest_ContentPartSlicePool.Get(); v != nil {
		sp := v.(*[]types.Baml_Rest_ContentPart)
		if cap(*sp) >= n {
			*sp = (*sp)[:0]
			return sp
		}
	}
	s := make([]types.Baml_Rest_ContentPart, 0, n)
	return &s
}
func putbaml_Rest_ContentPartSlice(sp *[]types.Baml_Rest_ContentPart) {
	if sp == nil {
		return
	}
	if cap(*sp) > 256 {
		return
	}
	used := (*sp)[0:len(*sp):cap(*sp)]
	for i := range used {
		used[i] = types.Baml_Rest_ContentPart{}
	}
	*sp = (*sp)[:0]
	baml_Rest_ContentPartSlicePool.Put(sp)
}
func convertBaml_Rest_MessageMediaInput(adapter bamlutils.Adapter, input *Baml_Rest_MessageMediaInput, ownedNested *[]func()) (types.Baml_Rest_Message, error) {
	var result types.Baml_Rest_Message
	result.Role = input.Role
	result.Content = input.Content
	if input.Parts != nil {
		__partsPtr := getbaml_Rest_ContentPartSlice(len(*input.Parts))
		*ownedNested = append(*ownedNested, func() {
			putbaml_Rest_ContentPartSlice(__partsPtr)
		})
		*__partsPtr = (*__partsPtr)[:len(*input.Parts)]
		__ptrSlice := *__partsPtr
		for __i, __v := range *input.Parts {
			__converted, __err := convertBaml_Rest_ContentPartMediaInput(adapter, &__v)
			if __err != nil {
				return result, fmt.Errorf("Parts[%d]: %w", __i, __err)
			}
			__ptrSlice[__i] = __converted
		}
		result.Parts = &__ptrSlice
	}
	result.Metadata = input.Metadata
	return result, nil
}

type BamlRestDynamicInput struct {
	Messages []Baml_Rest_MessageMediaInput `json:"messages"`
}
type BamlRestDynamicOutput struct {
	kind         bamlutils.StreamResultKind
	raw          string
	reasoning    string
	streamParsed *streamtypes.Baml_Rest_DynamicOutput
	finalParsed  *types.Baml_Rest_DynamicOutput
	err          error
	reset        bool
	metadata     *bamlutils.Metadata
}

func (v *BamlRestDynamicOutput) Kind() bamlutils.StreamResultKind {
	return v.kind
}
func unwrapDynamicBamlRestDynamicOutputStream(val *streamtypes.Baml_Rest_DynamicOutput) {
	if val == nil {
		return
	}
	if val.DynamicProperties.Len() == 0 {
		return
	}
	val.DynamicProperties.Range(func(key string, value any) bool {
		var unwrapped any
		if reflectValue, ok := value.(reflect.Value); ok {
			unwrapped = utils.UnwrapDynamicValue(reflectValue.Interface())
		} else {
			unwrapped = utils.UnwrapDynamicValue(value)
		}
		_ = val.DynamicProperties.Replace(key, unwrapped)
		return true
	})
}
func unwrapDynamicBamlRestDynamicOutputFinal(val *types.Baml_Rest_DynamicOutput) {
	if val == nil {
		return
	}
	if val.DynamicProperties.Len() == 0 {
		return
	}
	val.DynamicProperties.Range(func(key string, value any) bool {
		var unwrapped any
		if reflectValue, ok := value.(reflect.Value); ok {
			unwrapped = utils.UnwrapDynamicValue(reflectValue.Interface())
		} else {
			unwrapped = utils.UnwrapDynamicValue(value)
		}
		_ = val.DynamicProperties.Replace(key, unwrapped)
		return true
	})
}
func (v *BamlRestDynamicOutput) Stream() any {
	return v.streamParsed
}
func (v *BamlRestDynamicOutput) Final() any {
	return v.finalParsed
}
func (v *BamlRestDynamicOutput) Error() error {
	return v.err
}
func (v *BamlRestDynamicOutput) Raw() string {
	return v.raw
}
func (v *BamlRestDynamicOutput) Reasoning() string {
	return v.reasoning
}
func (v *BamlRestDynamicOutput) Reset() bool {
	return v.reset
}
func (v *BamlRestDynamicOutput) Metadata() *bamlutils.Metadata {
	return v.metadata
}

var bamlRestDynamicOutputPool = bamlutils.NewPool(func() *BamlRestDynamicOutput {
	return &BamlRestDynamicOutput{}
})

func (v *BamlRestDynamicOutput) Release() {
	if v == nil {
		return
	}
	*v = BamlRestDynamicOutput{}
	bamlRestDynamicOutputPool.Put(v)
}
func getBamlRestDynamicOutput() *BamlRestDynamicOutput {
	return bamlRestDynamicOutputPool.Get()
}
func newBamlRestDynamicOutputError(err error) *BamlRestDynamicOutput {
	r := getBamlRestDynamicOutput()
	r.kind = bamlutils.StreamResultKindError
	r.err = err
	return r
}
func newBamlRestDynamicOutputMetadata(md *bamlutils.Metadata) *BamlRestDynamicOutput {
	r := getBamlRestDynamicOutput()
	r.kind = bamlutils.StreamResultKindMetadata
	r.metadata = md
	return r
}

var baml_Rest_MessageSlicePool sync.Pool

func getbaml_Rest_MessageSlice(n int) *[]types.Baml_Rest_Message {
	if v := baml_Rest_MessageSlicePool.Get(); v != nil {
		sp := v.(*[]types.Baml_Rest_Message)
		if cap(*sp) >= n {
			*sp = (*sp)[:0]
			return sp
		}
	}
	s := make([]types.Baml_Rest_Message, 0, n)
	return &s
}
func putbaml_Rest_MessageSlice(sp *[]types.Baml_Rest_Message) {
	if sp == nil {
		return
	}
	if cap(*sp) > 1024 {
		return
	}
	used := (*sp)[0:len(*sp):cap(*sp)]
	for i := range used {
		used[i] = types.Baml_Rest_Message{}
	}
	*sp = (*sp)[:0]
	baml_Rest_MessageSlicePool.Put(sp)
}
func bamlRestDynamicNoRaw(adapter bamlutils.Adapter, rawInput any, out chan bamlutils.StreamResult, skipPartials bool, plannedMetadata *bamlutils.Metadata, clientOverride string) error {
	options, err := makeLegacyStreamOptionsFromAdapter(adapter, clientOverride)
	if err != nil {
		return err
	}
	input, ok := rawInput.(*BamlRestDynamicInput)
	if !ok {
		return fmt.Errorf("invalid input type: expected *%s, got %T", "BamlRestDynamicInput", rawInput)
	}
	__messagesPtr_messages := getbaml_Rest_MessageSlice(len(input.Messages))
	*__messagesPtr_messages = (*__messagesPtr_messages)[:len(input.Messages)]
	__struct_messages := *__messagesPtr_messages
	var __ownedNested_messages []func()
	__releaseConverted := func() {
		for _, __release := range __ownedNested_messages {
			__release()
		}
		putbaml_Rest_MessageSlice(__messagesPtr_messages)
	}
	for __i, __v := range input.Messages {
		__converted, __err___struct_messages := convertBaml_Rest_MessageMediaInput(adapter, &__v, &__ownedNested_messages)
		if __err___struct_messages != nil {
			__releaseConverted()
			return fmt.Errorf("messages[%d]: %w", __i, __err___struct_messages)
		}
		__struct_messages[__i] = __converted
	}
	return runNoRawOrchestration(adapter, out, func() bamlutils.StreamResult {
		__r := getBamlRestDynamicOutput()
		__r.kind = bamlutils.StreamResultKindHeartbeat
		return __r
	}, func(err error) bamlutils.StreamResult {
		return newBamlRestDynamicOutputError(err)
	}, func(__r bamlutils.StreamResult) {
		__r.Release()
	}, plannedMetadata, func(md *bamlutils.Metadata) bamlutils.StreamResult {
		return newBamlRestDynamicOutputMetadata(md)
	}, func(beforeFinal func(), onTick func(context.Context, pkg.TickReason, pkg.FunctionLog) pkg.FunctionSignal) error {
		defer __releaseConverted()
		streamOpts := append(options, bamlclient.WithOnTick(onTick))
		if clientOverride != "" {
			streamOpts = append(slices.Clone(streamOpts), bamlclient.WithClient(clientOverride))
		}
		stream, streamErr := bamlclient.Stream.Baml_Rest_Dynamic(adapter, __struct_messages, streamOpts...)
		if streamErr != nil {
			__errR := newBamlRestDynamicOutputError(streamErr)
			select {
			case out <- __errR:
			case <-adapter.Done():
				__errR.Release()
			}
			return nil
		}
		for streamVal := range stream {
			select {
			case <-adapter.Done():
				return nil
			default:
			}
			if streamVal.IsError {
				__errR := newBamlRestDynamicOutputError(streamVal.Error)
				select {
				case out <- __errR:
				case <-adapter.Done():
					__errR.Release()
					return nil
				}
				continue
			}
			if streamVal.IsFinal {
				__r := getBamlRestDynamicOutput()
				__r.kind = bamlutils.StreamResultKindFinal
				__r.finalParsed = streamVal.Final()
				unwrapDynamicBamlRestDynamicOutputFinal(__r.finalParsed)
				beforeFinal()
				select {
				case out <- __r:
				case <-adapter.Done():
					__r.Release()
					return nil
				}
				continue
			}
			if !skipPartials {
				if __partial := streamVal.Stream(); __partial != nil {
					unwrapDynamicBamlRestDynamicOutputStream(__partial)
					__r := getBamlRestDynamicOutput()
					__r.kind = bamlutils.StreamResultKindStream
					__r.streamParsed = __partial
					select {
					case out <- __r:
					case <-adapter.Done():
						__r.Release()
						return nil
					default:
						__r.Release()
					}
				}
			}
		}
		return nil
	})
}
func bamlRestDynamicFull(adapter bamlutils.Adapter, rawInput any, out chan bamlutils.StreamResult, skipIntermediateParsing bool, plannedMetadata *bamlutils.Metadata, clientOverride string) error {
	options, err := makeLegacyStreamOptionsFromAdapter(adapter, clientOverride)
	if err != nil {
		return err
	}
	input, ok := rawInput.(*BamlRestDynamicInput)
	if !ok {
		return fmt.Errorf("invalid input type: expected *%s, got %T", "BamlRestDynamicInput", rawInput)
	}
	__messagesPtr_messages := getbaml_Rest_MessageSlice(len(input.Messages))
	*__messagesPtr_messages = (*__messagesPtr_messages)[:len(input.Messages)]
	__struct_messages := *__messagesPtr_messages
	var __ownedNested_messages []func()
	__releaseConverted := func() {
		for _, __release := range __ownedNested_messages {
			__release()
		}
		putbaml_Rest_MessageSlice(__messagesPtr_messages)
	}
	for __i, __v := range input.Messages {
		__converted, __err___struct_messages := convertBaml_Rest_MessageMediaInput(adapter, &__v, &__ownedNested_messages)
		if __err___struct_messages != nil {
			__releaseConverted()
			return fmt.Errorf("messages[%d]: %w", __i, __err___struct_messages)
		}
		__struct_messages[__i] = __converted
	}
	return runFullOrchestration(adapter, out, options, func() bamlutils.StreamResult {
		__r := getBamlRestDynamicOutput()
		__r.kind = bamlutils.StreamResultKindHeartbeat
		return __r
	}, func(err error, raw string) bamlutils.StreamResult {
		__r := newBamlRestDynamicOutputError(err)
		__r.raw = raw
		return __r
	}, func(__r bamlutils.StreamResult) {
		__r.Release()
	}, plannedMetadata, func(md *bamlutils.Metadata) bamlutils.StreamResult {
		return newBamlRestDynamicOutputMetadata(md)
	}, func(funcLog pkg.FunctionLog, extractor *sse.IncrementalExtractor, extractorMu *sync.Mutex) error {
		calls, callsErr := funcLog.Calls()
		if callsErr != nil {
			return nil
		}
		callCount := len(calls)
		if callCount == 0 {
			return nil
		}
		lastCall := calls[callCount-1]
		streamCall, ok := lastCall.(pkg.LLMStreamCall)
		if !ok {
			return nil
		}
		provider, provErr := streamCall.Provider()
		if provErr != nil {
			return nil
		}
		if !sse.IsDeltaProviderSupported(provider) {
			resolved := false
			for i := callCount - 1; i >= 0 && !resolved; i-- {
				if sc, scOk := calls[i].(pkg.LLMStreamCall); scOk {
					if cp, cpErr := sc.Provider(); cpErr == nil && sse.IsDeltaProviderSupported(cp) {
						provider = cp
						resolved = true
					}
				}
			}
			if !resolved {
				if clientName, cnErr := streamCall.ClientName(); cnErr == nil && clientName != "" {
					if reg := adapter.OriginalClientRegistry(); reg != nil {
						for _, rc := range reg.Clients {
							if rc != nil && rc.Name == clientName && rc.Provider != "" && sse.IsDeltaProviderSupported(rc.Provider) {
								provider = rc.Provider
								resolved = true
								break
							}
						}
					}
					if !resolved {
						if sp, spOk := introspected.ClientProvider[clientName]; spOk && sse.IsDeltaProviderSupported(sp) {
							provider = sp
						}
					}
				}
			}
		}
		chunks, chunksErr := streamCall.SSEChunks()
		if chunksErr != nil {
			return nil
		}
		extractorMu.Lock()
		defer extractorMu.Unlock()
		extractResult := sse.ExtractFrom(extractor, callCount, provider, chunks)
		if skipIntermediateParsing {
			return nil
		}
		if extractResult.ParseableDelta == "" && extractResult.RawDelta == "" && extractResult.ReasoningDelta == "" && !extractResult.Reset {
			return nil
		}
		parseable := extractResult.ParseableFull
		parseableDelta := extractResult.ParseableDelta
		rawDelta := extractResult.RawDelta
		reasoningDelta := extractResult.ReasoningDelta
		if parseable == "" || parseableDelta == "" {
			select {
			case <-adapter.Done():
				return nil
			default:
			}
			__r := getBamlRestDynamicOutput()
			__r.kind = bamlutils.StreamResultKindStream
			__r.raw = rawDelta
			__r.reasoning = reasoningDelta
			__r.reset = extractResult.Reset
			if extractResult.Reset {
				select {
				case out <- __r:
				case <-adapter.Done():
					__r.Release()
				}
			} else {
				select {
				case out <- __r:
				default:
					__r.Release()
				}
			}
			return nil
		}
		parsed, parseErr := bamlclient.ParseStream.Baml_Rest_Dynamic(adapter, parseable, options...)
		if parseErr == nil {
			select {
			case <-adapter.Done():
				return nil
			default:
			}
			parsedPtr := &parsed
			unwrapDynamicBamlRestDynamicOutputStream(parsedPtr)
			__r := getBamlRestDynamicOutput()
			__r.kind = bamlutils.StreamResultKindStream
			__r.raw = rawDelta
			__r.reasoning = reasoningDelta
			__r.streamParsed = parsedPtr
			__r.reset = extractResult.Reset
			if extractResult.Reset {
				select {
				case out <- __r:
				case <-adapter.Done():
					__r.Release()
				}
			} else {
				select {
				case out <- __r:
				default:
					__r.Release()
				}
			}
		} else if extractResult.Reset {
			select {
			case <-adapter.Done():
				return nil
			default:
			}
			__r := getBamlRestDynamicOutput()
			__r.kind = bamlutils.StreamResultKindStream
			__r.raw = rawDelta
			__r.reasoning = reasoningDelta
			__r.reset = true
			select {
			case out <- __r:
			case <-adapter.Done():
				__r.Release()
			}
		}
		return nil
	}, func(opts []bamlclient.CallOptionFunc) (any, error) {
		defer __releaseConverted()
		driveOpts := opts
		if clientOverride != "" {
			driveOpts = append(slices.Clone(opts), bamlclient.WithClient(clientOverride))
		}
		stream, streamErr := bamlclient.Stream.Baml_Rest_Dynamic(adapter, __struct_messages, driveOpts...)
		if streamErr != nil {
			return nil, streamErr
		}
		var result any
		var lastErr error
		for streamVal := range stream {
			if streamVal.IsError {
				lastErr = streamVal.Error
				continue
			}
			if streamVal.IsFinal {
				result = streamVal.Final()
			}
		}
		return result, lastErr
	}, func(result any, raw string, reasoning string) bamlutils.StreamResult {
		__r := getBamlRestDynamicOutput()
		__r.kind = bamlutils.StreamResultKindFinal
		__r.raw = raw
		__r.reasoning = reasoning
		if result != nil {
			if ptr, ok := result.(*types.Baml_Rest_DynamicOutput); ok {
				unwrapDynamicBamlRestDynamicOutputFinal(ptr)
				__r.finalParsed = ptr
			} else if val, ok := result.(types.Baml_Rest_DynamicOutput); ok {
				unwrapDynamicBamlRestDynamicOutputFinal(&val)
				__r.finalParsed = &val
			}
		}
		return __r
	})
}
func bamlRestDynamicBuildRequest(adapter bamlutils.Adapter, rawInput any, out chan bamlutils.StreamResult, provider string, retryPolicy *retry.Policy, fallbackChain []string, clientProviders map[string]string, legacyChildren map[string]bool, fallbackTargets map[string]string, fallbackRoundRobin map[string]*bamlutils.RoundRobinInfo, plannedMetadata *bamlutils.Metadata, clientOverride string) error {
	options, err := makeOptionsFromAdapter(adapter)
	if err != nil {
		return err
	}
	input, ok := rawInput.(*BamlRestDynamicInput)
	if !ok {
		return fmt.Errorf("invalid input type: expected *%s, got %T", "BamlRestDynamicInput", rawInput)
	}
	__messagesPtr_messages := getbaml_Rest_MessageSlice(len(input.Messages))
	*__messagesPtr_messages = (*__messagesPtr_messages)[:len(input.Messages)]
	__struct_messages := *__messagesPtr_messages
	var __ownedNested_messages []func()
	__releaseConverted := func() {
		for _, __release := range __ownedNested_messages {
			__release()
		}
		putbaml_Rest_MessageSlice(__messagesPtr_messages)
	}
	for __i, __v := range input.Messages {
		__converted, __err___struct_messages := convertBaml_Rest_MessageMediaInput(adapter, &__v, &__ownedNested_messages)
		if __err___struct_messages != nil {
			__releaseConverted()
			return fmt.Errorf("messages[%d]: %w", __i, __err___struct_messages)
		}
		__struct_messages[__i] = __converted
	}
	__debaml_messages := maybeApplyDeBAMLOutputFormat(adapter, __struct_messages)
	buildRequestFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		callOpts := options
		if clientOverride != "" {
			callOpts = append(slices.Clone(options), bamlclient.WithClient(clientOverride))
		}
		httpReq, err := bamlclient.StreamRequest.Baml_Rest_Dynamic(ctx, __debaml_messages, callOpts...)
		if err != nil {
			return nil, err
		}
		url, urlErr := httpReq.Url()
		if urlErr != nil {
			return nil, fmt.Errorf("failed to get URL: %w", urlErr)
		}
		method, methodErr := httpReq.Method()
		if methodErr != nil {
			return nil, fmt.Errorf("failed to get method: %w", methodErr)
		}
		headers, headersErr := httpReq.Headers()
		if headersErr != nil {
			return nil, fmt.Errorf("failed to get headers: %w", headersErr)
		}
		body, bodyErr := httpReq.Body()
		if bodyErr != nil {
			return nil, fmt.Errorf("failed to get body: %w", bodyErr)
		}
		bodyText, bodyTextErr := body.Text()
		if bodyTextErr != nil {
			return nil, fmt.Errorf("failed to get body text: %w", bodyTextErr)
		}
		req := &llmhttp.Request{
			Body:    bodyText,
			Headers: headers,
			Method:  method,
			URL:     url,
		}
		return req, nil
	}
	buildBedrockStreamRequestFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		callOpts := options
		if clientOverride != "" {
			callOpts = append(slices.Clone(options), bamlclient.WithClient(clientOverride))
		}
		httpReq, err := bamlclient.Request.Baml_Rest_Dynamic(ctx, __debaml_messages, callOpts...)
		if err != nil {
			return nil, err
		}
		url, urlErr := httpReq.Url()
		if urlErr != nil {
			return nil, fmt.Errorf("failed to get URL: %w", urlErr)
		}
		method, methodErr := httpReq.Method()
		if methodErr != nil {
			return nil, fmt.Errorf("failed to get method: %w", methodErr)
		}
		headers, headersErr := httpReq.Headers()
		if headersErr != nil {
			return nil, fmt.Errorf("failed to get headers: %w", headersErr)
		}
		body, bodyErr := httpReq.Body()
		if bodyErr != nil {
			return nil, fmt.Errorf("failed to get body: %w", bodyErr)
		}
		bodyText, bodyTextErr := body.Text()
		if bodyTextErr != nil {
			return nil, fmt.Errorf("failed to get body text: %w", bodyTextErr)
		}
		req := &llmhttp.Request{
			Body:    bodyText,
			Headers: headers,
			Method:  method,
			URL:     url,
		}
		req.URL = strings.Replace(req.URL, "/converse", "/converse-stream", 1)
		if req.Headers == nil {
			req.Headers = make(map[string]string)
		}
		req.Headers["Accept"] = llmhttp.AWSStreamContentType
		selectedClient := clientOverride
		if selectedClient == "" {
			selectedClient = introspected.FunctionClient["Baml_Rest_Dynamic"]
		}
		var (
			bedrockEndpointURL        string
			bedrockEndpointURLPresent bool
			bedrockRegion             string
			bedrockRegionPresent      bool
			bedrockCreds              llmhttp.BedrockCredentialSelector
		)
		if bedrockOpts, ok := introspected.BedrockClientOptionsByName[selectedClient]; ok {
			bedrockEndpointURL, _ = bedrockOpts.EndpointURL.Resolve()
			bedrockEndpointURLPresent = bedrockOpts.EndpointURL.IsSet()
			bedrockRegion, _ = bedrockOpts.Region.Resolve()
			bedrockRegionPresent = bedrockOpts.Region.IsSet()
			bedrockCreds.AccessKeyID, _ = bedrockOpts.Credentials.AccessKeyID.Resolve()
			bedrockCreds.AccessKeyIDPresent = bedrockOpts.Credentials.AccessKeyID.IsSet()
			bedrockCreds.SecretAccessKey, _ = bedrockOpts.Credentials.SecretAccessKey.Resolve()
			bedrockCreds.SecretAccessKeyPresent = bedrockOpts.Credentials.SecretAccessKey.IsSet()
			bedrockCreds.SessionToken, _ = bedrockOpts.Credentials.SessionToken.Resolve()
			bedrockCreds.SessionTokenPresent = bedrockOpts.Credentials.SessionToken.IsSet()
			bedrockCreds.Profile, _ = bedrockOpts.Credentials.Profile.Resolve()
			bedrockCreds.ProfilePresent = bedrockOpts.Credentials.Profile.IsSet()
		}
		if authErr := llmhttp.AttachBedrockAuthForClient(ctx, req, llmhttp.BedrockClientAuthOptions{
			ClientName:         selectedClient,
			Credentials:        bedrockCreds,
			EndpointURL:        bedrockEndpointURL,
			EndpointURLPresent: bedrockEndpointURLPresent,
			Region:             bedrockRegion,
			RegionPresent:      bedrockRegionPresent,
		}); authErr != nil {
			return nil, authErr
		}
		return req, nil
	}
	parseStreamFn := func(ctx context.Context, accumulated string) (any, error) {
		// Native de-BAML stream parse first; ok==false && err==nil means
		// declined/unsupported, so fall through to BAML parse-stream. The
		// fallback is SILENT (runs per accumulated prefix); a CLAIMED native
		// error propagates and the orchestrator drops it like a BAML parse-
		// stream error (non-terminal for partial emission).
		if result, ok, err := maybeParseDeBAMLStream(ctx, adapter, accumulated); ok || err != nil {
			return result, err
		}
		return bamlclient.ParseStream.Baml_Rest_Dynamic(ctx, accumulated, options...)
	}
	parseFinalFn := func(ctx context.Context, accumulated string) (any, error) {
		// Native de-BAML final parse first; ok==false && err==nil means
		// declined/unsupported, so fall through to BAML-as-today.
		if result, ok, err := maybeParseDeBAMLFinal(ctx, adapter, accumulated, "final"); ok || err != nil {
			return result, err
		}
		return bamlclient.Parse.Baml_Rest_Dynamic(ctx, accumulated, options...)
	}
	newResultFn := func(kind bamlutils.StreamResultKind, stream any, final any, raw string, reasoning string, err error, reset bool) bamlutils.StreamResult {
		r := getBamlRestDynamicOutput()
		r.kind = kind
		r.raw = raw
		r.reasoning = reasoning
		r.err = err
		r.reset = reset
		if stream != nil {
			if v, ok := stream.(*streamtypes.Baml_Rest_DynamicOutput); ok {
				unwrapDynamicBamlRestDynamicOutputStream(v)
				r.streamParsed = v
			} else if v, ok := stream.(streamtypes.Baml_Rest_DynamicOutput); ok {
				unwrapDynamicBamlRestDynamicOutputStream(&v)
				r.streamParsed = &v
			}
		}
		if final != nil {
			if v, ok := final.(*types.Baml_Rest_DynamicOutput); ok {
				unwrapDynamicBamlRestDynamicOutputFinal(v)
				r.finalParsed = v
			} else if v, ok := final.(types.Baml_Rest_DynamicOutput); ok {
				unwrapDynamicBamlRestDynamicOutputFinal(&v)
				r.finalParsed = &v
			}
		}
		return r
	}
	legacyStreamChildFn := func(ctx context.Context, clientOverride string, _ string, needsRaw bool, sendHeartbeat func()) (any, string, string, error) {
		callOpts, childOptsErr := makeLegacyChildOptionsFromAdapter(adapter, clientOverride)
		if childOptsErr != nil {
			return nil, "", "", childOptsErr
		}
		if clientOverride != "" {
			callOpts = append(slices.Clone(callOpts), bamlclient.WithClient(clientOverride))
		}
		return runLegacyChildStream(ctx, needsRaw, sendHeartbeat, func(onTick func(context.Context, pkg.TickReason, pkg.FunctionLog) pkg.FunctionSignal) (any, error) {
			opts := append(callOpts, bamlclient.WithOnTick(onTick))
			stream, streamErr := bamlclient.Stream.Baml_Rest_Dynamic(ctx, __struct_messages, opts...)
			if streamErr != nil {
				return nil, streamErr
			}
			var result any
			var lastErr error
			for streamVal := range stream {
				if streamVal.IsError {
					lastErr = streamVal.Error
					continue
				}
				if streamVal.IsFinal {
					result = streamVal.Final()
				}
			}
			return result, lastErr
		})
	}
	streamConfig := &buildrequest.StreamConfig{
		BuildBedrockStreamRequest: buildBedrockStreamRequestFn,
		ClientOverride:            clientOverride,
		ClientProviders:           clientProviders,
		FallbackChain:             fallbackChain,
		FallbackRoundRobin:        fallbackRoundRobin,
		FallbackTargets:           fallbackTargets,
		IncludeReasoning:          adapter.IncludeReasoning(),
		LegacyChildren:            legacyChildren,
		LegacyStreamChild:         legacyStreamChildFn,
		MetadataPlan:              plannedMetadata,
		NeedsPartials:             adapter.StreamMode().NeedsPartials(),
		NeedsRaw:                  adapter.StreamMode().NeedsRaw(),
		NewMetadataResult: func(md *bamlutils.Metadata) bamlutils.StreamResult {
			return newBamlRestDynamicOutputMetadata(md)
		},
		Provider:    provider,
		RetryPolicy: retryPolicy,
	}
	__httpClient := llmhttp.DefaultClient
	if __c := adapter.HTTPClient(); __c != nil {
		__httpClient = __c
	}
	go func() {
		defer close(out)
		defer __releaseConverted()
		gorecovery.GoHandler(func(err error) {
			__errR := newResultFn(bamlutils.StreamResultKindError, nil, nil, "", "", err, false)
			select {
			case out <- __errR:
			case <-adapter.Done():
				__errR.Release()
			}
		}, func() error {
			return buildrequest.RunStreamOrchestration(adapter, out, streamConfig, __httpClient, buildRequestFn, parseStreamFn, parseFinalFn, newResultFn)
		})
	}()
	return nil
}
func bamlRestDynamicBuildCallRequest(adapter bamlutils.Adapter, rawInput any, out chan bamlutils.StreamResult, provider string, retryPolicy *retry.Policy, fallbackChain []string, clientProviders map[string]string, legacyChildren map[string]bool, fallbackTargets map[string]string, fallbackRoundRobin map[string]*bamlutils.RoundRobinInfo, plannedMetadata *bamlutils.Metadata, clientOverride string) error {
	options, err := makeOptionsFromAdapter(adapter)
	if err != nil {
		return err
	}
	input, ok := rawInput.(*BamlRestDynamicInput)
	if !ok {
		return fmt.Errorf("invalid input type: expected *%s, got %T", "BamlRestDynamicInput", rawInput)
	}
	__messagesPtr_messages := getbaml_Rest_MessageSlice(len(input.Messages))
	*__messagesPtr_messages = (*__messagesPtr_messages)[:len(input.Messages)]
	__struct_messages := *__messagesPtr_messages
	var __ownedNested_messages []func()
	__releaseConverted := func() {
		for _, __release := range __ownedNested_messages {
			__release()
		}
		putbaml_Rest_MessageSlice(__messagesPtr_messages)
	}
	for __i, __v := range input.Messages {
		__converted, __err___struct_messages := convertBaml_Rest_MessageMediaInput(adapter, &__v, &__ownedNested_messages)
		if __err___struct_messages != nil {
			__releaseConverted()
			return fmt.Errorf("messages[%d]: %w", __i, __err___struct_messages)
		}
		__struct_messages[__i] = __converted
	}
	__debaml_messages := maybeApplyDeBAMLOutputFormat(adapter, __struct_messages)
	buildRequestFn := func(ctx context.Context, clientOverride string) (*llmhttp.Request, error) {
		callOpts := options
		if clientOverride != "" {
			callOpts = append(slices.Clone(options), bamlclient.WithClient(clientOverride))
		}
		httpReq, err := bamlclient.Request.Baml_Rest_Dynamic(ctx, __debaml_messages, callOpts...)
		if err != nil {
			return nil, err
		}
		url, urlErr := httpReq.Url()
		if urlErr != nil {
			return nil, fmt.Errorf("failed to get URL: %w", urlErr)
		}
		method, methodErr := httpReq.Method()
		if methodErr != nil {
			return nil, fmt.Errorf("failed to get method: %w", methodErr)
		}
		headers, headersErr := httpReq.Headers()
		if headersErr != nil {
			return nil, fmt.Errorf("failed to get headers: %w", headersErr)
		}
		body, bodyErr := httpReq.Body()
		if bodyErr != nil {
			return nil, fmt.Errorf("failed to get body: %w", bodyErr)
		}
		bodyText, bodyTextErr := body.Text()
		if bodyTextErr != nil {
			return nil, fmt.Errorf("failed to get body text: %w", bodyTextErr)
		}
		req := &llmhttp.Request{
			Body:    bodyText,
			Headers: headers,
			Method:  method,
			URL:     url,
		}
		selectedClient := clientOverride
		if selectedClient == "" {
			selectedClient = introspected.FunctionClient["Baml_Rest_Dynamic"]
		}
		var (
			bedrockEndpointURL        string
			bedrockEndpointURLPresent bool
			bedrockRegion             string
			bedrockRegionPresent      bool
			bedrockCreds              llmhttp.BedrockCredentialSelector
		)
		if bedrockOpts, ok := introspected.BedrockClientOptionsByName[selectedClient]; ok {
			bedrockEndpointURL, _ = bedrockOpts.EndpointURL.Resolve()
			bedrockEndpointURLPresent = bedrockOpts.EndpointURL.IsSet()
			bedrockRegion, _ = bedrockOpts.Region.Resolve()
			bedrockRegionPresent = bedrockOpts.Region.IsSet()
			bedrockCreds.AccessKeyID, _ = bedrockOpts.Credentials.AccessKeyID.Resolve()
			bedrockCreds.AccessKeyIDPresent = bedrockOpts.Credentials.AccessKeyID.IsSet()
			bedrockCreds.SecretAccessKey, _ = bedrockOpts.Credentials.SecretAccessKey.Resolve()
			bedrockCreds.SecretAccessKeyPresent = bedrockOpts.Credentials.SecretAccessKey.IsSet()
			bedrockCreds.SessionToken, _ = bedrockOpts.Credentials.SessionToken.Resolve()
			bedrockCreds.SessionTokenPresent = bedrockOpts.Credentials.SessionToken.IsSet()
			bedrockCreds.Profile, _ = bedrockOpts.Credentials.Profile.Resolve()
			bedrockCreds.ProfilePresent = bedrockOpts.Credentials.Profile.IsSet()
		}
		if authErr := llmhttp.AttachBedrockAuthForClient(ctx, req, llmhttp.BedrockClientAuthOptions{
			ClientName:         selectedClient,
			Credentials:        bedrockCreds,
			EndpointURL:        bedrockEndpointURL,
			EndpointURLPresent: bedrockEndpointURLPresent,
			Region:             bedrockRegion,
			RegionPresent:      bedrockRegionPresent,
		}); authErr != nil {
			return nil, authErr
		}
		return req, nil
	}
	parseFinalFn := func(ctx context.Context, text string) (any, error) {
		// Native de-BAML final parse first; ok==false && err==nil means
		// declined/unsupported, so fall through to BAML-as-today.
		if result, ok, err := maybeParseDeBAMLFinal(ctx, adapter, text, "final"); ok || err != nil {
			return result, err
		}
		return bamlclient.Parse.Baml_Rest_Dynamic(ctx, text, options...)
	}
	newResultFn := func(kind bamlutils.StreamResultKind, stream any, final any, raw string, reasoning string, err error, reset bool) bamlutils.StreamResult {
		r := getBamlRestDynamicOutput()
		r.kind = kind
		r.raw = raw
		r.reasoning = reasoning
		r.err = err
		r.reset = reset
		if final != nil {
			if v, ok := final.(*types.Baml_Rest_DynamicOutput); ok {
				unwrapDynamicBamlRestDynamicOutputFinal(v)
				r.finalParsed = v
			} else if v, ok := final.(types.Baml_Rest_DynamicOutput); ok {
				unwrapDynamicBamlRestDynamicOutputFinal(&v)
				r.finalParsed = &v
			}
		}
		return r
	}
	legacyCallChildFn := func(ctx context.Context, clientOverride string, _ string, needsRaw bool, sendHeartbeat func()) (any, string, string, error) {
		callOpts, childOptsErr := makeLegacyChildOptionsFromAdapter(adapter, clientOverride)
		if childOptsErr != nil {
			return nil, "", "", childOptsErr
		}
		if clientOverride != "" {
			callOpts = append(slices.Clone(callOpts), bamlclient.WithClient(clientOverride))
		}
		return runLegacyChildStream(ctx, needsRaw, sendHeartbeat, func(onTick func(context.Context, pkg.TickReason, pkg.FunctionLog) pkg.FunctionSignal) (any, error) {
			opts := append(callOpts, bamlclient.WithOnTick(onTick))
			stream, streamErr := bamlclient.Stream.Baml_Rest_Dynamic(ctx, __struct_messages, opts...)
			if streamErr != nil {
				return nil, streamErr
			}
			var result any
			var lastErr error
			for streamVal := range stream {
				if streamVal.IsError {
					lastErr = streamVal.Error
					continue
				}
				if streamVal.IsFinal {
					result = streamVal.Final()
				}
			}
			return result, lastErr
		})
	}
	callConfig := &buildrequest.CallConfig{
		ClientOverride:     clientOverride,
		ClientProviders:    clientProviders,
		FallbackChain:      fallbackChain,
		FallbackRoundRobin: fallbackRoundRobin,
		FallbackTargets:    fallbackTargets,
		IncludeReasoning:   adapter.IncludeReasoning(),
		LegacyCallChild:    legacyCallChildFn,
		LegacyChildren:     legacyChildren,
		MetadataPlan:       plannedMetadata,
		NeedsRaw:           adapter.StreamMode().NeedsRaw(),
		NewMetadataResult: func(md *bamlutils.Metadata) bamlutils.StreamResult {
			return newBamlRestDynamicOutputMetadata(md)
		},
		Provider:    provider,
		RetryPolicy: retryPolicy,
	}
	__httpClient := llmhttp.DefaultClient
	if __c := adapter.HTTPClient(); __c != nil {
		__httpClient = __c
	}
	maybeInstallNativeCall(adapter, callConfig, __struct_messages, len(fallbackChain) == 0, len(fallbackChain) > 0, plannedMetadata != nil && plannedMetadata.RoundRobin != nil, __httpClient.WouldRewriteOrProxy)
	go func() {
		defer close(out)
		defer __releaseConverted()
		gorecovery.GoHandler(func(err error) {
			__errR := newResultFn(bamlutils.StreamResultKindError, nil, nil, "", "", err, false)
			select {
			case out <- __errR:
			case <-adapter.Done():
				__errR.Release()
			}
		}, func() error {
			return buildrequest.RunCallOrchestration(adapter, out, callConfig, __httpClient, buildRequestFn, parseFinalFn, buildrequest.ExtractResponseContent, buildrequest.ExtractResponseContentBytes, buildrequest.ExtractResponseContentBorrowed, newResultFn)
		})
	}()
	return nil
}
func bamlRestDynamicDirectLegacyCall(adapter bamlutils.Adapter, rawInput any, out chan bamlutils.StreamResult, provider string, hasRequestRetryOverride bool, plannedMetadata *bamlutils.Metadata, clientOverride string) error {
	options, err := makeLegacyStreamOptionsFromAdapter(adapter, clientOverride)
	if err != nil {
		return err
	}
	input, ok := rawInput.(*BamlRestDynamicInput)
	if !ok {
		return fmt.Errorf("invalid input type: expected *%s, got %T", "BamlRestDynamicInput", rawInput)
	}
	__messagesPtr_messages := getbaml_Rest_MessageSlice(len(input.Messages))
	*__messagesPtr_messages = (*__messagesPtr_messages)[:len(input.Messages)]
	__struct_messages := *__messagesPtr_messages
	var __ownedNested_messages []func()
	__releaseConverted := func() {
		for _, __release := range __ownedNested_messages {
			__release()
		}
		putbaml_Rest_MessageSlice(__messagesPtr_messages)
	}
	for __i, __v := range input.Messages {
		__converted, __err___struct_messages := convertBaml_Rest_MessageMediaInput(adapter, &__v, &__ownedNested_messages)
		if __err___struct_messages != nil {
			__releaseConverted()
			return fmt.Errorf("messages[%d]: %w", __i, __err___struct_messages)
		}
		__struct_messages[__i] = __converted
	}
	skipPartials := true
	__httpClient := llmhttp.DefaultClient
	if __c := adapter.HTTPClient(); __c != nil {
		__httpClient = __c
	}
	var serve bamlutils.NativeServeFunc
	if __g, __gok := adapter.(nativeServeGetter); __gok {
		serve = __g.NativeServeComparator()
	}
	return runNoRawOrchestration(adapter, out, func() bamlutils.StreamResult {
		__r := getBamlRestDynamicOutput()
		__r.kind = bamlutils.StreamResultKindHeartbeat
		return __r
	}, func(err error) bamlutils.StreamResult {
		return newBamlRestDynamicOutputError(err)
	}, func(__r bamlutils.StreamResult) {
		__r.Release()
	}, plannedMetadata, func(md *bamlutils.Metadata) bamlutils.StreamResult {
		if md != nil && md.Phase == bamlutils.MetadataPhaseOutcome {
			md.PlannedEngine = "native"
		}
		return newBamlRestDynamicOutputMetadata(md)
	}, func(beforeFinal func(), onTick func(context.Context, pkg.TickReason, pkg.FunctionLog) pkg.FunctionSignal) error {
		defer __releaseConverted()
		if serve != nil {
			var __probeHB atomic.Bool
			__sendHeartbeat := func() {
				if __probeHB.CompareAndSwap(false, true) {
					__hb := getBamlRestDynamicOutput()
					__hb.kind = bamlutils.StreamResultKindHeartbeat
					select {
					case out <- __hb:
					default:
						__hb.Release()
					}
				}
			}
			__res := serve(adapter, bamlutils.NativeServeRequest{
				BuildBAMLRequest:        nil,
				ClientOverride:          clientOverride,
				HasFallbackChain:        false,
				HasRequestRetryOverride: hasRequestRetryOverride,
				HasRoundRobin:           false,
				IncludeReasoning:        adapter.IncludeReasoning(),
				Messages:                nativeShadowMessages(__struct_messages),
				Mode:                    bamlutils.NativeServeModeCall,
				OutputSchema:            adapter.DeBAMLOutputSchema(),
				Provider:                provider,
				Registry:                adapter.OriginalClientRegistry(),
				SendHeartbeat:           __sendHeartbeat,
				SingleLeaf:              true,
				WouldRewriteOrProxy:     __httpClient.WouldRewriteOrProxy,
			})
			switch __res.Disposition {
			case bamlutils.NativeServeSucceeded:
				__wrapped, __werr := wrapDeBAMLDynamicOutput(__res.FinalJSON)
				if __werr != nil {
					__errR := newBamlRestDynamicOutputError(&buildrequest.OutputParseError{Err: __werr})
					__errR.raw = __res.Raw
					select {
					case out <- __errR:
					case <-adapter.Done():
						__errR.Release()
					}
					return nil
				}
				if plannedMetadata != nil {
					__outcome := bamlutils.BuildLegacyOutcome(plannedMetadata, 0, plannedMetadata.Client, plannedMetadata.Provider, nil)
					if __outcome != nil {
						__outcome.WinnerEngine = __res.WinnerEngine
						__outcome.PlannedEngine = "native"
						__om := newBamlRestDynamicOutputMetadata(__outcome)
						select {
						case out <- __om:
						case <-adapter.Done():
							__om.Release()
						}
					}
				}
				__r := getBamlRestDynamicOutput()
				__r.kind = bamlutils.StreamResultKindFinal
				__r.finalParsed = &__wrapped
				unwrapDynamicBamlRestDynamicOutputFinal(__r.finalParsed)
				select {
				case out <- __r:
				case <-adapter.Done():
					__r.Release()
				}
				return nil
			case bamlutils.NativeServeFailed:
				__errR := newBamlRestDynamicOutputError(__res.Err)
				__errR.raw = __res.RawDiagnostic
				select {
				case out <- __errR:
				case <-adapter.Done():
					__errR.Release()
				}
				return nil
			case bamlutils.NativeServeDeclined:
			// no-op: fall through to the ordinary legacy stream below
			default:
				__errR := newBamlRestDynamicOutputError(fmt.Errorf("native serve returned unknown disposition %d", __res.Disposition))
				select {
				case out <- __errR:
				case <-adapter.Done():
					__errR.Release()
				}
				return nil
			}
		}
		streamOpts := append(options, bamlclient.WithOnTick(onTick))
		if clientOverride != "" {
			streamOpts = append(slices.Clone(streamOpts), bamlclient.WithClient(clientOverride))
		}
		stream, streamErr := bamlclient.Stream.Baml_Rest_Dynamic(adapter, __struct_messages, streamOpts...)
		if streamErr != nil {
			__errR := newBamlRestDynamicOutputError(streamErr)
			select {
			case out <- __errR:
			case <-adapter.Done():
				__errR.Release()
			}
			return nil
		}
		for streamVal := range stream {
			select {
			case <-adapter.Done():
				return nil
			default:
			}
			if streamVal.IsError {
				__errR := newBamlRestDynamicOutputError(streamVal.Error)
				select {
				case out <- __errR:
				case <-adapter.Done():
					__errR.Release()
					return nil
				}
				continue
			}
			if streamVal.IsFinal {
				__r := getBamlRestDynamicOutput()
				__r.kind = bamlutils.StreamResultKindFinal
				__r.finalParsed = streamVal.Final()
				unwrapDynamicBamlRestDynamicOutputFinal(__r.finalParsed)
				beforeFinal()
				select {
				case out <- __r:
				case <-adapter.Done():
					__r.Release()
					return nil
				}
				continue
			}
			if !skipPartials {
				if __partial := streamVal.Stream(); __partial != nil {
					unwrapDynamicBamlRestDynamicOutputStream(__partial)
					__r := getBamlRestDynamicOutput()
					__r.kind = bamlutils.StreamResultKindStream
					__r.streamParsed = __partial
					select {
					case out <- __r:
					case <-adapter.Done():
						__r.Release()
						return nil
					default:
						__r.Release()
					}
				}
			}
		}
		return nil
	})
}
func Baml_Rest_Dynamic(adapter bamlutils.Adapter, rawInput any) (<-chan bamlutils.StreamResult, error) {
	out := make(chan bamlutils.StreamResult, 100)
	var err error
	mode := adapter.StreamMode()
	__retryClient := buildrequest.ResolvePrimaryClient(adapter, introspected.FunctionClient["Baml_Rest_Dynamic"])
	__effective := __retryClient
	var __rrInfo *bamlutils.RoundRobinInfo
	__rrEffective, __rrInfoUpgrade, __rrErr := buildrequest.ResolveEffectiveClient(adapter, introspected.FunctionClient["Baml_Rest_Dynamic"], introspected.FallbackChains, introspected.ClientProvider, introspected.RoundRobinCoordinator)
	if __rrErr != nil {
		return nil, __rrErr
	}
	__effective = __rrEffective
	__rrInfo = __rrInfoUpgrade
	__reg := adapter.OriginalClientRegistry()
	// Try non-streaming BuildRequest path for /call and /call-with-raw
	if introspected.Request != nil && (mode == bamlutils.StreamModeCall || mode == bamlutils.StreamModeCallWithRaw) {
		provider := buildrequest.ResolveClientProvider(__reg, __effective, introspected.ClientProvider)
		if provider != "" && buildrequest.IsCallProviderSupported(provider) {
			retryPolicy := buildrequest.ResolveStrategyAwareRetryPolicy(adapter, __retryClient, __effective, introspected.ClientRetryPolicy[__retryClient], introspected.ClientRetryPolicy[__effective], introspected.RetryPolicies)
			__planned := buildrequest.BuildSingleProviderPlanForClient(__effective, provider, retryPolicy, buildrequest.BuildRequestAPIRequest)
			__planned.RoundRobin = __rrInfo
			err = bamlRestDynamicBuildCallRequest(adapter, rawInput, out, provider, retryPolicy, nil, nil, nil, nil, nil, __planned, __effective)
			if err != nil {
				return nil, err
			}
			return out, nil
		}
	}
	// Try streaming BuildRequest path for /stream and /stream-with-raw
	if introspected.StreamRequest != nil && (mode == bamlutils.StreamModeStream || mode == bamlutils.StreamModeStreamWithRaw) {
		provider := buildrequest.ResolveClientProvider(__reg, __effective, introspected.ClientProvider)
		if provider != "" && buildrequest.IsProviderSupported(provider) {
			retryPolicy := buildrequest.ResolveStrategyAwareRetryPolicy(adapter, __retryClient, __effective, introspected.ClientRetryPolicy[__retryClient], introspected.ClientRetryPolicy[__effective], introspected.RetryPolicies)
			__planned := buildrequest.BuildSingleProviderPlanForClient(__effective, provider, retryPolicy, buildrequest.BuildRequestAPIStreamRequest)
			__planned.RoundRobin = __rrInfo
			err = bamlRestDynamicBuildRequest(adapter, rawInput, out, provider, retryPolicy, nil, nil, nil, nil, nil, __planned, __effective)
			if err != nil {
				return nil, err
			}
			return out, nil
		}
		__resolution, __fbErr := buildrequest.ResolveFallbackChainPlanForClient(__reg, __effective, introspected.FallbackChains, introspected.ClientProvider, buildrequest.IsProviderSupported, buildrequest.PreferAdvancer(adapter, introspected.RoundRobinCoordinator))
		if __fbErr != nil {
			return nil, __fbErr
		}
		if __resolution != nil && len(__resolution.Chain) > 0 {
			retryPolicy := buildrequest.ResolveStrategyAwareRetryPolicy(adapter, __retryClient, __effective, introspected.ClientRetryPolicy[__retryClient], introspected.ClientRetryPolicy[__effective], introspected.RetryPolicies)
			__planned := buildrequest.BuildFallbackChainPlanFromResolution(__effective, __resolution, retryPolicy, buildrequest.BuildRequestAPIStreamRequest)
			__planned.RoundRobin = __rrInfo
			err = bamlRestDynamicBuildRequest(adapter, rawInput, out, "", retryPolicy, __resolution.Chain, __resolution.Providers, __resolution.LegacyChildren, __resolution.Targets, __resolution.NestedRoundRobin, __planned, "")
			if err != nil {
				return nil, err
			}
			return out, nil
		}
	}
	// Bridge: /call and /call-with-raw via StreamRequest when Request is unavailable
	if introspected.StreamRequest != nil && (mode == bamlutils.StreamModeCall || mode == bamlutils.StreamModeCallWithRaw) {
		provider := buildrequest.ResolveClientProvider(__reg, __effective, introspected.ClientProvider)
		if provider != "" && buildrequest.IsProviderSupported(provider) {
			retryPolicy := buildrequest.ResolveStrategyAwareRetryPolicy(adapter, __retryClient, __effective, introspected.ClientRetryPolicy[__retryClient], introspected.ClientRetryPolicy[__effective], introspected.RetryPolicies)
			__planned := buildrequest.BuildSingleProviderPlanForClient(__effective, provider, retryPolicy, buildrequest.BuildRequestAPIStreamRequest)
			__planned.RoundRobin = __rrInfo
			err = bamlRestDynamicBuildRequest(adapter, rawInput, out, provider, retryPolicy, nil, nil, nil, nil, nil, __planned, __effective)
			if err != nil {
				return nil, err
			}
			return out, nil
		}
		__resolution, __fbErr := buildrequest.ResolveFallbackChainPlanForClient(__reg, __effective, introspected.FallbackChains, introspected.ClientProvider, buildrequest.IsProviderSupported, buildrequest.PreferAdvancer(adapter, introspected.RoundRobinCoordinator))
		if __fbErr != nil {
			return nil, __fbErr
		}
		if __resolution != nil && len(__resolution.Chain) > 0 {
			retryPolicy := buildrequest.ResolveStrategyAwareRetryPolicy(adapter, __retryClient, __effective, introspected.ClientRetryPolicy[__retryClient], introspected.ClientRetryPolicy[__effective], introspected.RetryPolicies)
			__callChainSupported := len(__resolution.LegacyChildren) == 0
			if __callChainSupported {
				for _, __provider := range __resolution.Providers {
					if !buildrequest.IsCallProviderSupported(__provider) {
						__callChainSupported = false
						break
					}
				}
			}
			if __callChainSupported {
				__planned := buildrequest.BuildFallbackChainPlanFromResolution(__effective, __resolution, retryPolicy, buildrequest.BuildRequestAPIRequest)
				__planned.RoundRobin = __rrInfo
				err = bamlRestDynamicBuildCallRequest(adapter, rawInput, out, "", retryPolicy, __resolution.Chain, __resolution.Providers, __resolution.LegacyChildren, __resolution.Targets, __resolution.NestedRoundRobin, __planned, "")
				if err != nil {
					return nil, err
				}
				return out, nil
			}
			__planned := buildrequest.BuildFallbackChainPlanFromResolution(__effective, __resolution, retryPolicy, buildrequest.BuildRequestAPIStreamRequest)
			__planned.RoundRobin = __rrInfo
			err = bamlRestDynamicBuildRequest(adapter, rawInput, out, "", retryPolicy, __resolution.Chain, __resolution.Providers, __resolution.LegacyChildren, __resolution.Targets, __resolution.NestedRoundRobin, __planned, "")
			if err != nil {
				return nil, err
			}
			return out, nil
		}
	}
	// Legacy path: CallStream + OnTick (for unsupported/empty providers or BAML versions without a BuildRequest surface)
	__legacyRetryPolicy := buildrequest.ResolveStrategyAwareRetryPolicy(adapter, __retryClient, __effective, introspected.ClientRetryPolicy[__retryClient], introspected.ClientRetryPolicy[__effective], introspected.RetryPolicies)
	__legacyPredicate := buildrequest.IsProviderSupported
	__plannedLegacy := buildrequest.BuildLegacyMetadataPlanForClient(__reg, __effective, introspected.ClientProvider[__effective], introspected.FallbackChains, introspected.ClientProvider, __legacyPredicate, __legacyRetryPolicy)
	__plannedLegacy.RoundRobin = __rrInfo
	__legacyClientOverride := __effective
	buildrequest.LogLegacyClassification(adapter, "Baml_Rest_Dynamic", __plannedLegacy)
	if adapter.DeBAMLConfig().Enabled && mode == bamlutils.StreamModeCall && hasNativeServe(adapter) && __plannedLegacy.Strategy == "" && __plannedLegacy.Chain == nil && __plannedLegacy.RoundRobin == nil {
		__directLegacyProvider := buildrequest.ResolveClientProvider(__reg, __effective, introspected.ClientProvider)
		if __directLegacyProvider != "" && !buildrequest.IsCallProviderSupported(__directLegacyProvider) {
			err = bamlRestDynamicDirectLegacyCall(adapter, rawInput, out, __directLegacyProvider, __legacyRetryPolicy != nil, __plannedLegacy, __legacyClientOverride)
			if err != nil {
				return nil, err
			}
			return out, nil
		}
	}
	switch mode {
	case bamlutils.StreamModeCall:
		err = bamlRestDynamicNoRaw(adapter, rawInput, out, true, __plannedLegacy, __legacyClientOverride)
	case bamlutils.StreamModeStream:
		err = bamlRestDynamicNoRaw(adapter, rawInput, out, false, __plannedLegacy, __legacyClientOverride)
	case bamlutils.StreamModeCallWithRaw:
		err = bamlRestDynamicFull(adapter, rawInput, out, true, __plannedLegacy, __legacyClientOverride)
	case bamlutils.StreamModeStreamWithRaw:
		err = bamlRestDynamicFull(adapter, rawInput, out, false, __plannedLegacy, __legacyClientOverride)
	default:
		err = fmt.Errorf("unknown StreamMode: %d", mode)
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}

var Methods = map[string]bamlutils.StreamingMethod{"Baml_Rest_Dynamic": {
	Impl: Baml_Rest_Dynamic,
	MakeInput: func() any {
		return new(BamlRestDynamicInput)
	},
	MakeOutput: func() any {
		return new(types.Baml_Rest_DynamicOutput)
	},
	MakeStreamOutput: func() any {
		return new(streamtypes.Baml_Rest_DynamicOutput)
	},
}}

func parseBamlRestDynamic(adapter bamlutils.Adapter, raw string) (any, error) {
	if result, ok, err := maybeParseDeBAMLFinal(adapter, adapter, raw, "parse_only"); ok || err != nil {
		if err != nil {
			return nil, err
		}
		unwrapDynamicBamlRestDynamicOutputFinal(&result)
		return result, nil
	}
	options, err := makeOptionsFromAdapter(adapter)
	if err != nil {
		return nil, err
	}
	result, parseErr := bamlclient.Parse.Baml_Rest_Dynamic(adapter, raw, options...)
	if parseErr != nil {
		return nil, parseErr
	}
	unwrapDynamicBamlRestDynamicOutputFinal(&result)
	return result, nil
}
func parseBamlRestDynamicStream(adapter bamlutils.Adapter, raw string) (any, error) {
	options, err := makeOptionsFromAdapter(adapter)
	if err != nil {
		return nil, err
	}
	result, parseErr := bamlclient.ParseStream.Baml_Rest_Dynamic(adapter, raw, options...)
	if parseErr != nil {
		return nil, parseErr
	}
	unwrapDynamicBamlRestDynamicOutputStream(&result)
	return result, nil
}

var ParseMethods = map[string]bamlutils.ParseMethod{"Baml_Rest_Dynamic": {
	Impl: parseBamlRestDynamic,
	MakeOutput: func() any {
		return new(types.Baml_Rest_DynamicOutput)
	},
	StreamImpl: parseBamlRestDynamicStream,
}}

func applyDynamicTypes(tb *introspected.TypeBuilder, dt *bamlutils.DynamicTypes) error {
	if dt == nil {
		return nil
	}
	if err := dt.Validate(); err != nil {
		return fmt.Errorf("invalid dynamic_types schema: %w", err)
	}
	typeCache := make(map[string]introspected.Type)
	classBuilderCache := make(map[string]introspected.DynamicClassBuilder)
	preserveOrder := dt.PreserveOrder

	for _, name := range schemaKeys(dt.Enums, preserveOrder) {
		enum, _ := dt.Enums.Get(name)
		if err := createEnumShell(tb, name, enum, typeCache); err != nil {
			return fmt.Errorf("enum %q: %w", name, err)
		}
	}

	for _, name := range schemaKeys(dt.Enums, preserveOrder) {
		enum, _ := dt.Enums.Get(name)
		if err := addEnumValues(tb, name, enum, typeCache); err != nil {
			return fmt.Errorf("enum %q values: %w", name, err)
		}
	}

	for _, name := range schemaKeys(dt.Classes, preserveOrder) {
		if err := createClassShell(tb, name, typeCache, classBuilderCache); err != nil {
			return fmt.Errorf("class %q: %w", name, err)
		}
	}

	for _, name := range schemaKeys(dt.Classes, preserveOrder) {
		class, _ := dt.Classes.Get(name)
		if err := addNewClassProperties(tb, name, class, typeCache, classBuilderCache, preserveOrder); err != nil {
			return fmt.Errorf("class %q properties: %w", name, err)
		}
	}

	for _, name := range schemaKeys(dt.Classes, preserveOrder) {
		cb, ok := classBuilderCache[name]
		if !ok {
			continue
		}
		typ, err := cb.Type()
		if err != nil {
			return fmt.Errorf("class %q type: %w", name, err)
		}
		typeCache[name] = typ
	}

	for _, name := range schemaKeys(dt.Classes, preserveOrder) {
		class, _ := dt.Classes.Get(name)
		if err := addExistingClassProperties(tb, name, class, typeCache, classBuilderCache, preserveOrder); err != nil {
			return fmt.Errorf("class %q properties: %w", name, err)
		}
	}
	return nil
}
func schemaKeys[V any](m bamlutils.OrderedMap[V], preserve bool) []string {
	keys := m.Keys()
	if !preserve {
		slices.Sort(keys)
	}
	return keys
}
func createEnumShell(tb *introspected.TypeBuilder, name string, enum *bamlutils.DynamicEnum, typeCache map[string]introspected.Type) error {
	if introspected.EnumExists(name) {
		return nil
	}

	eb, err := tb.AddEnum(name)
	if err != nil {
		return fmt.Errorf("failed to create enum: %w", err)
	}

	for _, v := range enum.Values {
		vb, err := eb.AddValue(v.Name)
		if err != nil {
			continue
		}
		if v.Description != "" {
			_ = vb.SetDescription(v.Description)
		}
		if v.Alias != "" {
			_ = vb.SetAlias(v.Alias)
		}
		if v.Skip {
			_ = vb.SetSkip(true)
		}
	}

	typ, err := eb.Type()
	if err != nil {
		return fmt.Errorf("get type: %w", err)
	}
	typeCache[name] = typ
	return nil
}
func addEnumValues(tb *introspected.TypeBuilder, name string, enum *bamlutils.DynamicEnum, typeCache map[string]introspected.Type) error {
	_ = typeCache
	accessor, ok := introspected.DynamicEnums[name]
	if !ok {
		return nil
	}

	eb, err := accessor(tb)
	if err != nil {
		return fmt.Errorf("get enum builder: %w", err)
	}

	for _, v := range enum.Values {
		vb, err := eb.AddValue(v.Name)
		if err != nil {
			continue
		}
		if v.Description != "" {
			_ = vb.SetDescription(v.Description)
		}
		if v.Alias != "" {
			_ = vb.SetAlias(v.Alias)
		}
		if v.Skip {
			_ = vb.SetSkip(true)
		}
	}
	return nil
}
func createClassShell(tb *introspected.TypeBuilder, name string, typeCache map[string]introspected.Type, classBuilderCache map[string]introspected.DynamicClassBuilder) error {
	_ = typeCache
	if introspected.ClassExists(name) {
		return nil
	}

	cb, err := tb.AddClass(name)
	if err != nil {
		return fmt.Errorf("failed to create class: %w", err)
	}

	classBuilderCache[name] = cb
	return nil
}
func addNewClassProperties(tb *introspected.TypeBuilder, name string, class *bamlutils.DynamicClass, typeCache map[string]introspected.Type, classBuilderCache map[string]introspected.DynamicClassBuilder, preserveOrder bool) error {
	cb, ok := classBuilderCache[name]
	if !ok {
		return nil
	}

	for _, propName := range schemaKeys(class.Properties, preserveOrder) {
		prop, _ := class.Properties.Get(propName)
		typ, err := resolvePropertyType(tb, prop, typeCache, classBuilderCache)
		if err != nil {
			if strings.Contains(err.Error(), "unresolved reference") {
				continue
			}
			return fmt.Errorf("property %q type: %w", propName, err)
		}

		_, err = cb.AddProperty(propName, typ)
		if err != nil {
			return fmt.Errorf("property %q: %w", propName, err)
		}
	}
	return nil
}
func addExistingClassProperties(tb *introspected.TypeBuilder, name string, class *bamlutils.DynamicClass, typeCache map[string]introspected.Type, classBuilderCache map[string]introspected.DynamicClassBuilder, preserveOrder bool) error {
	if _, ok := classBuilderCache[name]; ok {
		return nil
	}

	accessor, ok := introspected.DynamicClasses[name]
	if !ok {
		return nil
	}

	cb, err := accessor(tb)
	if err != nil {
		return fmt.Errorf("get class builder: %w", err)
	}

	for _, propName := range schemaKeys(class.Properties, preserveOrder) {
		prop, _ := class.Properties.Get(propName)
		typ, err := resolvePropertyType(tb, prop, typeCache, classBuilderCache)
		if err != nil {
			if strings.Contains(err.Error(), "unresolved reference") {
				continue
			}
			return fmt.Errorf("property %q type: %w", propName, err)
		}

		_, err = cb.AddProperty(propName, typ)
		if err != nil {
			return fmt.Errorf("property %q: %w", propName, err)
		}
	}
	return nil
}
func resolvePropertyType(tb *introspected.TypeBuilder, prop *bamlutils.DynamicProperty, typeCache map[string]introspected.Type, classBuilderCache map[string]introspected.DynamicClassBuilder) (introspected.Type, error) {
	if prop.Ref != "" {
		return resolveRef(tb, prop.Ref, typeCache, classBuilderCache)
	}

	return resolveTypeRef(tb, &bamlutils.DynamicTypeSpec{
		Inner:  prop.Inner,
		Items:  prop.Items,
		Keys:   prop.Keys,
		OneOf:  prop.OneOf,
		Type:   prop.Type,
		Value:  prop.Value,
		Values: prop.Values,
	}, typeCache, classBuilderCache)
}
func resolveTypeRef(tb *introspected.TypeBuilder, ref *bamlutils.DynamicTypeSpec, typeCache map[string]introspected.Type, classBuilderCache map[string]introspected.DynamicClassBuilder) (introspected.Type, error) {
	if ref == nil {
		return nil, fmt.Errorf("nil type reference")
	}

	if ref.Ref != "" {
		return resolveRef(tb, ref.Ref, typeCache, classBuilderCache)
	}

	switch ref.Type {
	case "string":
		return tb.String()
	case "int":
		return tb.Int()
	case "float":
		return tb.Float()
	case "bool":
		return tb.Bool()
	case "null":
		return tb.Null()
	case "literal_string":
		str, ok := ref.Value.(string)
		if !ok {
			return nil, fmt.Errorf("literal_string value must be a string, got %T", ref.Value)
		}
		return tb.LiteralString(str)
	case "literal_int":
		switch v := ref.Value.(type) {
		case float64:
			return tb.LiteralInt(int64(v))
		case int64:
			return tb.LiteralInt(v)
		case int:
			return tb.LiteralInt(int64(v))
		default:
			return nil, fmt.Errorf("literal_int value must be a number, got %T", ref.Value)
		}
	case "literal_bool":
		b, ok := ref.Value.(bool)
		if !ok {
			return nil, fmt.Errorf("literal_bool value must be a boolean, got %T", ref.Value)
		}
		return tb.LiteralBool(b)
	case "list":
		if ref.Items == nil {
			return nil, fmt.Errorf("list type requires 'items' field")
		}
		inner, err := resolveTypeRef(tb, ref.Items, typeCache, classBuilderCache)
		if err != nil {
			return nil, fmt.Errorf("list items: %w", err)
		}
		return tb.List(inner)
	case "optional":
		if ref.Inner == nil {
			return nil, fmt.Errorf("optional type requires 'inner' field")
		}
		inner, err := resolveTypeRef(tb, ref.Inner, typeCache, classBuilderCache)
		if err != nil {
			return nil, fmt.Errorf("optional inner: %w", err)
		}
		return tb.Optional(inner)
	case "map":
		if ref.Keys == nil {
			return nil, fmt.Errorf("map type requires 'keys' field")
		}
		if ref.Values == nil {
			return nil, fmt.Errorf("map type requires 'values' field")
		}
		keys, err := resolveTypeRef(tb, ref.Keys, typeCache, classBuilderCache)
		if err != nil {
			return nil, fmt.Errorf("map keys: %w", err)
		}
		values, err := resolveTypeRef(tb, ref.Values, typeCache, classBuilderCache)
		if err != nil {
			return nil, fmt.Errorf("map values: %w", err)
		}
		return tb.Map(keys, values)
	case "union":
		if len(ref.OneOf) == 0 {
			return nil, fmt.Errorf("union type requires 'oneOf' field with at least one type")
		}
		types := make([]introspected.Type, 0, len(ref.OneOf))
		for i, item := range ref.OneOf {
			typ, err := resolveTypeRef(tb, item, typeCache, classBuilderCache)
			if err != nil {
				return nil, fmt.Errorf("union oneOf[%d]: %w", i, err)
			}
			types = append(types, typ)
		}
		return tb.Union(types)
	case "":
		return nil, fmt.Errorf("type field is required")
	default:
		return nil, fmt.Errorf("unknown type: %q", ref.Type)
	}
}
func resolveRef(tb *introspected.TypeBuilder, name string, typeCache map[string]introspected.Type, classBuilderCache map[string]introspected.DynamicClassBuilder) (introspected.Type, error) {
	if typ, ok := typeCache[name]; ok {
		return typ, nil
	}

	if cb, ok := classBuilderCache[name]; ok {
		typ, err := cb.Type()
		if err != nil {
			return nil, fmt.Errorf("get type for class %q: %w", name, err)
		}
		typeCache[name] = typ
		return typ, nil
	}

	typ, err := introspected.GetClassType(tb, name)
	if err == nil && typ != nil {
		typeCache[name] = typ
		return typ, nil
	}

	typ, err = introspected.GetEnumType(tb, name)
	if err == nil && typ != nil {
		typeCache[name] = typ
		return typ, nil
	}

	return nil, fmt.Errorf("unresolved reference: %q", name)
}
func createTypeBuilder(config *bamlutils.TypeBuilder) (*introspected.TypeBuilder, error) {
	tb, err := introspected.NewTypeBuilder()
	if err != nil {
		return nil, err
	}
	if config == nil {
		return tb, nil
	}
	if config.DynamicTypes != nil {
		if err := applyDynamicTypes(tb, config.DynamicTypes); err != nil {
			return nil, fmt.Errorf("failed to apply dynamic types: %w", err)
		}
	}
	for idx, input := range config.BamlSnippets {
		if err := tb.AddBaml(input); err != nil {
			return nil, fmt.Errorf("baml_snippets[%d]: %w", idx, err)
		}
	}
	return tb, nil
}
func MakeAdapter(ctx context.Context) bamlutils.Adapter {
	return &adapter.BamlAdapter{
		Context:                    ctx,
		IntrospectedClientProvider: introspected.ClientProvider,
		MediaFactory:               createMedia,
		TypeBuilderFactory: func(config *bamlutils.TypeBuilder) (*introspected.TypeBuilder, error) {
			return createTypeBuilder(config)
		},
	}
}
func createMedia(kind bamlutils.MediaKind, url *string, base64 *string, mimeType *string) (any, error) {
	if url != nil {
		switch kind {
		case bamlutils.MediaKindImage:
			return bamlclient.NewImageFromUrl(*url, mimeType)
		case bamlutils.MediaKindAudio:
			return bamlclient.NewAudioFromUrl(*url, mimeType)
		case bamlutils.MediaKindPDF:
			return bamlclient.NewPDFFromUrl(*url, mimeType)
		case bamlutils.MediaKindVideo:
			return bamlclient.NewVideoFromUrl(*url, mimeType)
		}
	}
	if base64 != nil {
		switch kind {
		case bamlutils.MediaKindImage:
			return bamlclient.NewImageFromBase64(*base64, mimeType)
		case bamlutils.MediaKindAudio:
			return bamlclient.NewAudioFromBase64(*base64, mimeType)
		case bamlutils.MediaKindPDF:
			return bamlclient.NewPDFFromBase64(*base64, mimeType)
		case bamlutils.MediaKindVideo:
			return bamlclient.NewVideoFromBase64(*base64, mimeType)
		}
	}
	return nil, fmt.Errorf("unsupported media kind: %v", kind)
}
func makeOptionsFromAdapterInternal(adapterIn bamlutils.Adapter, legacy bool) ([]bamlclient.CallOptionFunc, error) {
	adapter, ok := adapterIn.(*adapter.BamlAdapter)
	if !ok {
		return nil, fmt.Errorf("invalid adapter type: expected *BamlAdapter, got %T", adapterIn)
	}
	registry := adapter.ClientRegistry
	if legacy {
		registry = adapter.LegacyClientRegistry
	}
	result := make([]bamlclient.CallOptionFunc, 0, 3)
	if registry != nil {
		result = append(result, bamlclient.WithClientRegistry(registry))
	}
	if adapter.TypeBuilder != nil {
		result = append(result, bamlclient.WithTypeBuilder(adapter.TypeBuilder))
	}
	return result, nil
}
func makeOptionsFromAdapter(adapterIn bamlutils.Adapter) ([]bamlclient.CallOptionFunc, error) {
	return makeOptionsFromAdapterInternal(adapterIn, false)
}
func makeLegacyOptionsFromAdapter(adapterIn bamlutils.Adapter) ([]bamlclient.CallOptionFunc, error) {
	return makeOptionsFromAdapterInternal(adapterIn, true)
}
func makeLegacyChildOptionsFromAdapter(adapterIn bamlutils.Adapter, clientOverride string) ([]bamlclient.CallOptionFunc, error) {
	adapter, ok := adapterIn.(*adapter.BamlAdapter)
	if !ok {
		return nil, fmt.Errorf("invalid adapter type: expected *BamlAdapter, got %T", adapterIn)
	}
	result := make([]bamlclient.CallOptionFunc, 0, 3)
	original := adapter.OriginalClientRegistry()
	if original != nil || clientOverride != "" {
		registry := pkg.NewClientRegistry()
		if original != nil {
			entries := buildrequest.BuildLegacyChildRegistryEntries(original, clientOverride, adapter.IntrospectedClientProvider, introspected.FallbackChains)
			for _, e := range entries {
				registry.AddLlmClient(e.Name, e.Provider, e.Options)
			}
		}
		if clientOverride != "" {
			registry.SetPrimaryClient(clientOverride)
		}
		result = append(result, bamlclient.WithClientRegistry(registry))
	}
	if adapter.TypeBuilder != nil {
		result = append(result, bamlclient.WithTypeBuilder(adapter.TypeBuilder))
	}
	return result, nil
}
func makeLegacyStreamOptionsFromAdapter(adapterIn bamlutils.Adapter, clientOverride string) ([]bamlclient.CallOptionFunc, error) {
	adapter, ok := adapterIn.(*adapter.BamlAdapter)
	if !ok {
		return nil, fmt.Errorf("invalid adapter type: expected *BamlAdapter, got %T", adapterIn)
	}
	result := make([]bamlclient.CallOptionFunc, 0, 3)
	registry := adapter.LegacyClientRegistry
	if registry != nil || clientOverride != "" {
		if registry == nil {
			registry = pkg.NewClientRegistry()
		}
		if clientOverride != "" {
			registry.SetPrimaryClient(clientOverride)
		}
		result = append(result, bamlclient.WithClientRegistry(registry))
	}
	if adapter.TypeBuilder != nil {
		result = append(result, bamlclient.WithTypeBuilder(adapter.TypeBuilder))
	}
	return result, nil
}
func InitBamlRuntime() {
	bamlclient.InitRuntime()
}

// runNoRawOrchestration manages the noRaw streaming lifecycle:
// heartbeat tracking, onTick callback, goroutine launch, and panic recovery.
// The per-method body closure handles stream creation and iteration.
func runNoRawOrchestration(adapter bamlutils.Adapter, out chan bamlutils.StreamResult, newHeartbeat func() bamlutils.StreamResult, newError func(error) bamlutils.StreamResult, release func(bamlutils.StreamResult), plannedMetadata *bamlutils.Metadata, newMetadataResult func(*bamlutils.Metadata) bamlutils.StreamResult, body func(func(), func(context.Context, pkg.TickReason, pkg.FunctionLog) pkg.FunctionSignal) error) error {
	startTime := time.Now()
	var heartbeatSent atomic.Bool
	var plannedSent atomic.Bool
	var lastFuncLog atomic.Value
	emitPlanned := func() {
		if plannedMetadata == nil || newMetadataResult == nil {
			return
		}
		if !plannedSent.CompareAndSwap(false, true) {
			return
		}
		__plan := *plannedMetadata
		__plan.Phase = bamlutils.MetadataPhasePlanned
		__m := newMetadataResult(&__plan)
		if __m == nil {
			return
		}
		select {
		case out <- __m:
		case <-adapter.Done():
			release(__m)
		default:
			release(__m)
		}
	}
	onTick := func(_ context.Context, _ pkg.TickReason, funcLog pkg.FunctionLog) pkg.FunctionSignal {
		lastFuncLog.Store(funcLog)
		if heartbeatSent.CompareAndSwap(false, true) {
			select {
			case <-adapter.Done():
				return nil
			default:
			}
			__r := newHeartbeat()
			select {
			case out <- __r:
			default:
				release(__r)
			}
		}
		return nil
	}
	beforeFinal := func() {
		if plannedMetadata == nil || newMetadataResult == nil {
			return
		}
		var __winnerClient string
		var __winnerProvider string
		var __bamlCallCount *int
		if fl, flOk := lastFuncLog.Load().(pkg.FunctionLog); flOk {
			if __sel, __selErr := fl.SelectedCall(); __selErr == nil && __sel != nil {
				if __cn, __cnErr := __sel.ClientName(); __cnErr == nil {
					__winnerClient = __cn
				}
				if __pv, __pvErr := __sel.Provider(); __pvErr == nil {
					__winnerProvider = __pv
				}
			}
			if __calls, __callsErr := fl.Calls(); __callsErr == nil {
				__n := len(__calls) - 1
				if __n < 0 {
					__n = 0
				}
				__bamlCallCount = &__n
			}
		}
		__outcome := bamlutils.BuildLegacyOutcome(plannedMetadata, time.Since(startTime).Milliseconds(), __winnerClient, __winnerProvider, __bamlCallCount)
		if __outcome == nil {
			return
		}
		__om := newMetadataResult(__outcome)
		if __om == nil {
			return
		}
		select {
		case out <- __om:
		case <-adapter.Done():
			release(__om)
		}
	}
	go func() {
		defer close(out)
		emitPlanned()
		gorecovery.GoHandler(func(err error) {
			__errR := newError(err)
			select {
			case out <- __errR:
			case <-adapter.Done():
				release(__errR)
			}
		}, func() error {
			return body(beforeFinal, onTick)
		})
	}()
	return nil
}

// runFullOrchestration manages the full streaming lifecycle:
// queue management, two-phase shutdown, heartbeat, onTick, partials goroutine,
// stream drain goroutine, and panic recovery.
// Per-method code supplies processTick, driveStream, and emitFinal closures.
func runFullOrchestration(adapter bamlutils.Adapter, out chan bamlutils.StreamResult, options []bamlclient.CallOptionFunc, newHeartbeat func() bamlutils.StreamResult, newError func(error, string) bamlutils.StreamResult, release func(bamlutils.StreamResult), plannedMetadata *bamlutils.Metadata, newMetadataResult func(*bamlutils.Metadata) bamlutils.StreamResult, processTick func(pkg.FunctionLog, *sse.IncrementalExtractor, *sync.Mutex) error, driveStream func([]bamlclient.CallOptionFunc) (any, error), emitFinal func(any, string, string) bamlutils.StreamResult) error {
	startTime := time.Now()
	funcLogQueue := goconcurrentqueue.NewFIFO()
	queueCtx, queueCancel := context.WithCancel(context.Background())
	var stopping atomic.Bool
	var inTick atomic.Int64
	var pending atomic.Int64
	ticksDone := make(chan struct{})
	allDone := make(chan struct{})
	var ticksOnce sync.Once
	var allOnce sync.Once
	var shutdownOnce sync.Once
	watcherDone := make(chan struct{})
	var heartbeatSent atomic.Bool
	var plannedSent atomic.Bool
	var fatalMu sync.Mutex
	var fatalErr error
	emitPlanned := func() {
		if plannedMetadata == nil || newMetadataResult == nil {
			return
		}
		if !plannedSent.CompareAndSwap(false, true) {
			return
		}
		__plan := *plannedMetadata
		__plan.Phase = bamlutils.MetadataPhasePlanned
		__m := newMetadataResult(&__plan)
		if __m == nil {
			return
		}
		select {
		case out <- __m:
		case <-adapter.Done():
			release(__m)
		default:
			release(__m)
		}
	}
	extractor := sse.NewIncrementalExtractor(adapter.IncludeReasoning())
	var extractorMu sync.Mutex
	var lastFuncLog atomic.Value
	reconcileRaw := func(finalRaw string, parseableFull string) string {
		if fl, flOk := lastFuncLog.Load().(pkg.FunctionLog); flOk {
			if authRaw, rawErr := fl.RawLLMResponse(); rawErr == nil {
				if len(authRaw) > len(parseableFull) && strings.HasPrefix(authRaw, parseableFull) {
					finalRaw = finalRaw + authRaw[len(parseableFull):]
				} else if len(authRaw) > len(finalRaw) {
					finalRaw = authRaw
				}
			}
		}
		return finalRaw
	}
	computeErrorRaw := func() string {
		extractorMu.Lock()
		__raw := extractor.RawFull()
		__parseable := extractor.ParseableFull()
		extractorMu.Unlock()
		return reconcileRaw(__raw, __parseable)
	}
	doShutdown := func() {
		stopping.Store(true)
		if inTick.Load() == 0 {
			ticksOnce.Do(func() {
				close(ticksDone)
			})
		} else {
			<-ticksDone
		}
		queueCancel()
		if pending.Load() == 0 {
			allOnce.Do(func() {
				close(allDone)
			})
		} else {
			<-allDone
		}
	}
	errHandler := func(err error) {
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
		if stopping.CompareAndSwap(false, true) {
			if inTick.Load() == 0 {
				ticksOnce.Do(func() {
					close(ticksDone)
				})
			}
			go shutdownOnce.Do(doShutdown)
		}
	}
	go func() {
		select {
		case <-adapter.Done():
			shutdownOnce.Do(doShutdown)
		case <-watcherDone:
		}
	}()
	decrementPending := func() {
		if pending.Add(-1) == 0 && stopping.Load() {
			allOnce.Do(func() {
				close(allDone)
			})
		}
	}
	onTick := func(_ context.Context, _ pkg.TickReason, funcLog pkg.FunctionLog) pkg.FunctionSignal {
		if err := gorecovery.Call(func() error {
			inTick.Add(1)
			defer func() {
				if inTick.Add(-1) == 0 && stopping.Load() {
					ticksOnce.Do(func() {
						close(ticksDone)
					})
				}
			}()
			if stopping.Load() {
				return nil
			}
			select {
			case <-adapter.Done():
				return nil
			default:
			}
			if heartbeatSent.CompareAndSwap(false, true) {
				__r := newHeartbeat()
				select {
				case out <- __r:
				default:
					release(__r)
				}
			}
			lastFuncLog.Store(funcLog)
			pending.Add(1)
			if err := funcLogQueue.Enqueue(funcLog); err != nil {
				if pending.Add(-1) == 0 && stopping.Load() {
					allOnce.Do(func() {
						close(allDone)
					})
				}
			}
			return nil
		}); err != nil {
			errHandler(err)
		}
		return nil
	}
	processItem := func(funcLog pkg.FunctionLog) {
		defer decrementPending()
		if err := gorecovery.Call(func() error {
			return processTick(funcLog, extractor, &extractorMu)
		}); err != nil {
			errHandler(err)
		}
	}
	drain := func() {
		for {
			select {
			case <-adapter.Done():
				for {
					_, drainErr := funcLogQueue.Dequeue()
					if drainErr != nil {
						return
					}
					decrementPending()
				}
			default:
			}
			remaining, drainErr := funcLogQueue.Dequeue()
			if drainErr != nil {
				return
			}
			funcLog, ok := remaining.(pkg.FunctionLog)
			if !ok {
				decrementPending()
				continue
			}
			processItem(funcLog)
		}
	}
	go func() {
		gorecovery.GoHandler(errHandler, func() error {
			for {
				shouldExit, err := gorecovery.Call1[bool](func() (bool, error) {
					item, err := funcLogQueue.DequeueOrWaitForNextElementContext(queueCtx)
					if err != nil {
						drain()
						return true, nil
					}
					funcLog, ok := item.(pkg.FunctionLog)
					if !ok {
						decrementPending()
						return false, nil
					}
					processItem(funcLog)
					return false, nil
				})
				if err != nil {
					errHandler(err)
					continue
				}
				if shouldExit {
					return nil
				}
			}
		})
	}()
	go func() {
		defer close(out)
		defer close(watcherDone)
		emitPlanned()
		gorecovery.GoHandler(func(err error) {
			shutdownOnce.Do(doShutdown)
			__errR := newError(err, "")
			select {
			case out <- __errR:
			case <-adapter.Done():
				release(__errR)
			}
		}, func() error {
			opts := append(options, bamlclient.WithOnTick(onTick))
			finalResult, lastErr := driveStream(opts)
			shutdownOnce.Do(doShutdown)
			fatalMu.Lock()
			fatalErrCopy := fatalErr
			fatalMu.Unlock()
			if fatalErrCopy != nil {
				__errR := newError(fatalErrCopy, computeErrorRaw())
				select {
				case out <- __errR:
				case <-adapter.Done():
					release(__errR)
				}
				return nil
			}
			if lastErr != nil {
				__errR := newError(lastErr, computeErrorRaw())
				select {
				case out <- __errR:
				case <-adapter.Done():
					release(__errR)
				}
				return nil
			}
			fl, flOk := lastFuncLog.Load().(pkg.FunctionLog)
			if flOk {
				_ = processTick(fl, extractor, &extractorMu)
			}
			extractorMu.Lock()
			finalRaw := extractor.RawFull()
			parseableFull := extractor.ParseableFull()
			finalReasoning := extractor.ReasoningFull()
			extractorMu.Unlock()
			finalRaw = reconcileRaw(finalRaw, parseableFull)
			if plannedMetadata != nil && newMetadataResult != nil {
				var __winnerClient string
				var __winnerProvider string
				var __bamlCallCount *int
				if flOk {
					if __sel, __selErr := fl.SelectedCall(); __selErr == nil && __sel != nil {
						if __cn, __cnErr := __sel.ClientName(); __cnErr == nil {
							__winnerClient = __cn
						}
						if __pv, __pvErr := __sel.Provider(); __pvErr == nil {
							__winnerProvider = __pv
						}
					}
					if __calls, __callsErr := fl.Calls(); __callsErr == nil {
						__n := len(__calls) - 1
						if __n < 0 {
							__n = 0
						}
						__bamlCallCount = &__n
					}
				}
				__outcome := bamlutils.BuildLegacyOutcome(plannedMetadata, time.Since(startTime).Milliseconds(), __winnerClient, __winnerProvider, __bamlCallCount)
				if __outcome != nil {
					__om := newMetadataResult(__outcome)
					if __om != nil {
						select {
						case out <- __om:
						case <-adapter.Done():
							release(__om)
							return nil
						}
					}
				}
			}
			__r := emitFinal(finalResult, finalRaw, finalReasoning)
			select {
			case out <- __r:
			case <-adapter.Done():
				release(__r)
			}
			return nil
		})
	}()
	return nil
}

// runLegacyChildStream drives a single child of a mixed-mode fallback
// chain through BAML's Stream API. The per-method closure supplies
// driveStream, which appends the WithOnTick option and invokes
// Stream.<Method>. This helper wires the onTick callback that fires
// sendHeartbeat on the first FunctionLog tick (matching the
// BuildRequest path's post-HTTP liveness semantics) and captures the
// last FunctionLog so raw can be read via RawLLMResponse.
func runLegacyChildStream(ctx context.Context, needsRaw bool, sendHeartbeat func(), driveStream func(func(context.Context, pkg.TickReason, pkg.FunctionLog) pkg.FunctionSignal) (any, error)) (any, string, string, error) {
	var heartbeatFired atomic.Bool
	var lastFuncLog atomic.Value
	onTick := func(_ context.Context, _ pkg.TickReason, funcLog pkg.FunctionLog) pkg.FunctionSignal {
		if heartbeatFired.CompareAndSwap(false, true) {
			sendHeartbeat()
		}
		lastFuncLog.Store(funcLog)
		return nil
	}
	finalResult, err := driveStream(onTick)
	if err != nil {
		return nil, "", "", err
	}
	var raw string
	if needsRaw {
		if fl, ok := lastFuncLog.Load().(pkg.FunctionLog); ok {
			if r, rawErr := fl.RawLLMResponse(); rawErr == nil {
				raw = r
			}
		}
	}
	return finalResult, raw, "", nil
}
