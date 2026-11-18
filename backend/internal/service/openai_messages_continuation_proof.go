package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// 只证明本软件实际发送给客户端的历史回显，不承诺 provider 缓存命中。
const compatContinuationObservationMaxBytes = 4 << 20
const compatContinuationObserverContextKey = "openai_compat_continuation_observer"

type compatContinuationReplayContextKey struct{}

func compatContinuationRetryContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, compatContinuationReplayContextKey{}, true)
}

func compatContinuationReplayOnly(ctx context.Context) bool {
	value, _ := ctx.Value(compatContinuationReplayContextKey{}).(bool)
	return value
}

type compatContinuationProof struct {
	Fingerprint [32]byte
	ChainHash   [32]byte
	ReqCount    int
	Count       int
}

func canonicalProofJSON(raw []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func() (any, bool)
	walk = func() (any, bool) {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		delim, structured := token.(json.Delim)
		if !structured {
			return token, true
		}
		switch delim {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, false
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, false
				}
				if _, duplicate := object[key]; duplicate {
					return nil, false
				}
				value, ok := walk()
				if !ok {
					return nil, false
				}
				object[key] = value
			}
			closeToken, err := decoder.Token()
			return object, err == nil && closeToken == json.Delim('}')
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				value, ok := walk()
				if !ok {
					return nil, false
				}
				array = append(array, value)
			}
			closeToken, err := decoder.Token()
			return array, err == nil && closeToken == json.Delim(']')
		default:
			return nil, false
		}
	}
	value, ok := walk()
	if !ok {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, false
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), true
}

func continuationItemHash(item apicompat.ResponsesInputItem) ([32]byte, bool) {
	var zero [32]byte
	wire, err := json.Marshal(item)
	if err != nil {
		return zero, false
	}
	var normalized apicompat.ResponsesInputItem
	if err := json.Unmarshal(wire, &normalized); err != nil {
		return zero, false
	}
	switch normalized.Type {
	case "message":
		var parts []apicompat.ResponsesContentPart
		if err := json.Unmarshal(normalized.Content, &parts); err != nil {
			return zero, false
		}
		for i := range parts {
			parts[i].PromptCacheBreakpoint = nil
		}
		normalized.Content, err = json.Marshal(parts)
		if err != nil {
			return zero, false
		}
	case "function_call":
		if normalized.CallID == "" || normalized.Name == "" {
			return zero, false
		}
		arguments := strings.TrimSpace(normalized.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		canonical, ok := canonicalProofJSON([]byte(arguments))
		if !ok {
			return zero, false
		}
		normalized.Arguments = string(canonical)
	case "reasoning", "function_call_output":
	default:
		return zero, false
	}
	wire, err = json.Marshal(normalized)
	if err != nil {
		return zero, false
	}
	canonical, ok := canonicalProofJSON(wire)
	if !ok {
		return zero, false
	}
	return sha256.Sum256(canonical), true
}

func continuationChain(items []apicompat.ResponsesInputItem, seed [32]byte) ([32]byte, bool) {
	chain := seed
	for _, item := range items {
		hash, ok := continuationItemHash(item)
		if !ok {
			return [32]byte{}, false
		}
		var joined [64]byte
		copy(joined[:32], chain[:])
		copy(joined[32:], hash[:])
		chain = sha256.Sum256(joined[:])
	}
	return chain, true
}

func newContinuationObservation(request *apicompat.ResponsesRequest) (*continuationObservation, []apicompat.ResponsesInputItem) {
	if request == nil || len(request.Input) == 0 {
		return nil, nil
	}
	tools, err := json.Marshal(request.Tools)
	if err != nil {
		return nil, nil
	}
	consumed := len(request.Input) + len(tools) + len(request.Model) + len(request.Instructions)
	if consumed > compatContinuationObservationMaxBytes {
		return nil, nil
	}
	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal(request.Input, &items); err != nil || len(items) == 0 {
		return nil, nil
	}
	fingerprintRaw, err := json.Marshal(struct {
		Model        string          `json:"model"`
		Tools        json.RawMessage `json:"tools"`
		Instructions string          `json:"instructions"`
	}{request.Model, tools, request.Instructions})
	if err != nil {
		return nil, nil
	}
	fingerprint, ok := canonicalProofJSON(fingerprintRaw)
	if !ok {
		return nil, nil
	}
	chain, ok := continuationChain(items, [32]byte{})
	if !ok {
		return nil, nil
	}
	return &continuationObservation{
		request: compatContinuationProof{Fingerprint: sha256.Sum256(fingerprint), ChainHash: chain, ReqCount: len(items), Count: len(items)},
		valid:   true, consumed: consumed, activeIndex: -1,
	}, items
}

func selectContinuationSuffix(binding openAICompatSessionResponseBinding, observation *continuationObservation, items []apicompat.ResponsesInputItem) ([]apicompat.ResponsesInputItem, bool) {
	if observation == nil || !observation.valid || binding.ContinuationDisabled || binding.ResponseID == "" {
		return nil, false
	}
	proof := binding.Proof
	if proof.ReqCount <= 0 || proof.Count <= proof.ReqCount || len(items) <= proof.Count || proof.Fingerprint != observation.request.Fingerprint {
		return nil, false
	}
	chain, ok := continuationChain(items[:proof.Count], [32]byte{})
	if !ok || chain != proof.ChainHash {
		return nil, false
	}
	calls := make(map[string]int)
	for _, item := range items[proof.ReqCount:proof.Count] {
		if item.Type == "function_call" {
			calls[item.CallID]++
		}
	}
	outputs := make(map[string]int)
	userItems := 0
	for _, item := range items[proof.Count:] {
		switch item.Type {
		case "message":
			if item.Role != "user" {
				return nil, false
			}
			var parts []apicompat.ResponsesContentPart
			if err := json.Unmarshal(item.Content, &parts); err != nil || len(parts) == 0 {
				return nil, false
			}
			userItems++
		case "function_call_output":
			if item.CallID == "" {
				return nil, false
			}
			outputs[item.CallID]++
		default:
			return nil, false
		}
	}
	if len(calls) != len(outputs) {
		return nil, false
	}
	for callID, count := range calls {
		if outputs[callID] != count {
			return nil, false
		}
	}
	if len(calls) == 0 && userItems == 0 {
		return nil, false
	}
	return items[proof.Count:], true
}

// 仅本请求暂存已转换的客户端echo；binding最终只接收hash/count。
type continuationObservation struct {
	request       compatContinuationProof
	valid         bool
	started       bool
	complete      bool
	stopReason    string
	consumed      int
	activeIndex   int
	active        apicompat.AnthropicContentBlock
	blocks        []apicompat.AnthropicContentBlock
	text          strings.Builder
	arguments     strings.Builder
	signature     strings.Builder
	argumentsSeen bool
}

func (o *continuationObservation) invalidate() {
	if o == nil {
		return
	}
	o.valid = false
	o.blocks = nil
	o.active = apicompat.AnthropicContentBlock{}
	o.text = strings.Builder{}
	o.arguments = strings.Builder{}
	o.signature = strings.Builder{}
}

func (o *continuationObservation) spend(n int) bool {
	if o == nil || !o.valid {
		return false
	}
	if n < 0 || n > compatContinuationObservationMaxBytes-o.consumed {
		o.invalidate()
		return false
	}
	o.consumed += n
	return true
}

func (o *continuationObservation) observeEvent(evt apicompat.AnthropicStreamEvent) {
	if o == nil || !o.valid {
		return
	}
	if o.complete {
		o.invalidate()
		return
	}
	switch evt.Type {
	case "message_start":
		if o.started || evt.Message == nil {
			o.invalidate()
			return
		}
		o.started = true
	case "content_block_start":
		if !o.started || o.activeIndex != -1 || evt.Index == nil || *evt.Index != len(o.blocks) || evt.ContentBlock == nil {
			o.invalidate()
			return
		}
		block := *evt.ContentBlock
		if !o.spend(256 + len(block.Text) + len(block.Thinking) + len(block.Signature) + len(block.ID) + len(block.Name) + len(block.Input)) {
			return
		}
		switch block.Type {
		case "text":
			o.text.WriteString(block.Text)
		case "thinking":
			o.signature.WriteString(block.Signature)
		case "tool_use":
		default:
			o.invalidate()
			return
		}
		o.activeIndex = *evt.Index
		o.active = block
	case "content_block_delta":
		if o.activeIndex < 0 || evt.Index == nil || *evt.Index != o.activeIndex || evt.Delta == nil {
			o.invalidate()
			return
		}
		delta := evt.Delta
		if !o.spend(len(delta.Text) + len(delta.Thinking) + len(delta.Signature) + len(delta.PartialJSON)) {
			return
		}
		switch delta.Type {
		case "text_delta":
			if o.active.Type != "text" {
				o.invalidate()
				return
			}
			o.text.WriteString(delta.Text)
		case "thinking_delta":
			if o.active.Type != "thinking" {
				o.invalidate()
				return
			}
		case "signature_delta":
			if o.active.Type != "thinking" {
				o.invalidate()
				return
			}
			o.signature.WriteString(delta.Signature)
		case "input_json_delta":
			if o.active.Type != "tool_use" {
				o.invalidate()
				return
			}
			o.argumentsSeen = true
			o.arguments.WriteString(delta.PartialJSON)
		default:
			o.invalidate()
			return
		}
	case "content_block_stop":
		if o.activeIndex < 0 || evt.Index == nil || *evt.Index != o.activeIndex {
			o.invalidate()
			return
		}
		block := o.active
		switch block.Type {
		case "text":
			block.Text = o.text.String()
		case "thinking":
			block.Thinking = ""
			block.Signature = o.signature.String()
		case "tool_use":
			if o.argumentsSeen {
				block.Input = json.RawMessage(o.arguments.String())
			}
			if len(bytes.TrimSpace(block.Input)) == 0 {
				block.Input = json.RawMessage("{}")
			}
			if _, ok := canonicalProofJSON(block.Input); !ok {
				o.invalidate()
				return
			}
		}
		o.blocks = append(o.blocks, block)
		o.active = apicompat.AnthropicContentBlock{}
		o.activeIndex = -1
		o.text = strings.Builder{}
		o.arguments = strings.Builder{}
		o.signature = strings.Builder{}
		o.argumentsSeen = false
	case "message_delta":
		if !o.started || o.activeIndex != -1 || evt.Delta == nil || o.stopReason != "" {
			o.invalidate()
			return
		}
		o.stopReason = evt.Delta.StopReason
		if o.stopReason != "end_turn" && o.stopReason != "tool_use" {
			o.invalidate()
			return
		}
	case "message_stop":
		if !o.started || o.activeIndex != -1 || o.stopReason == "" {
			o.invalidate()
			return
		}
		o.complete = true
	default:
		o.invalidate()
	}
}

func (o *continuationObservation) observeResponse(response *apicompat.AnthropicResponse) {
	if o == nil || !o.valid {
		return
	}
	if response == nil || response.StopReason == nil || (*response.StopReason != "end_turn" && *response.StopReason != "tool_use") || o.started {
		o.invalidate()
		return
	}
	o.started = true
	o.stopReason = *response.StopReason
	for _, block := range response.Content {
		if !o.spend(256 + len(block.Text) + len(block.Thinking) + len(block.Signature) + len(block.ID) + len(block.Name) + len(block.Input)) {
			return
		}
		switch block.Type {
		case "text", "thinking":
		case "tool_use":
			if _, ok := canonicalProofJSON(block.Input); !ok {
				o.invalidate()
				return
			}
		default:
			o.invalidate()
			return
		}
		o.blocks = append(o.blocks, block)
	}
	o.complete = true
}

func (o *continuationObservation) completedProof() (compatContinuationProof, bool) {
	if o == nil || !o.valid || !o.complete || len(o.blocks) == 0 {
		return compatContinuationProof{}, false
	}
	// 必须一次转换完整assistant blocks，复用真实请求侧text/tool/thinking规范化。
	items, err := apicompat.AnthropicAssistantBlocksToResponsesInputItems(o.blocks)
	if err != nil || len(items) == 0 {
		o.invalidate()
		return compatContinuationProof{}, false
	}
	chain, ok := continuationChain(items, o.request.ChainHash)
	if !ok {
		o.invalidate()
		return compatContinuationProof{}, false
	}
	proof := o.request
	proof.ChainHash = chain
	proof.Count += len(items)
	o.blocks = nil
	return proof, true
}

func observeContinuationStreamEvent(c *gin.Context, event apicompat.AnthropicStreamEvent, terminalType string) {
	observer := continuationObserver(c)
	if observer == nil {
		return
	}
	if event.Type == "message_stop" && terminalType != "response.completed" && terminalType != "response.done" {
		observer.invalidate()
		return
	}
	observer.observeEvent(event)
}

func observeBufferedContinuationResponse(c *gin.Context, response *apicompat.AnthropicResponse, before int, upstream *apicompat.ResponsesResponse) {
	observer := continuationObserver(c)
	if observer == nil {
		return
	}
	if !continuationResponseEligible(upstream, true) || c.IsAborted() || len(c.Errors) != 0 || c.Writer.Status() != 200 || !c.Writer.Written() || c.Request.Context().Err() != nil {
		observer.invalidate()
		return
	}
	// 先执行有界观测，超限响应不能为了 bytecount 核对再额外完整编码。
	observer.observeResponse(response)
	if !observer.valid {
		return
	}
	encoded, err := json.Marshal(response)
	if before < 0 {
		before = 0
	}
	if err != nil || c.Writer.Size()-before != len(encoded) {
		observer.invalidate()
	}
}

// 只否决本响应的证明：未知上游事件可能被转换器忽略，不能因此视为完整历史。
// 无 Messages observer（例如 shared buffered reader 的 Chat 调用）立即 no-op。
func vetoContinuationEligibility(c *gin.Context, event *apicompat.ResponsesStreamEvent) {
	observer := continuationObserver(c)
	if observer == nil || !observer.valid {
		return
	}
	if event == nil {
		observer.invalidate()
		return
	}
	valid := true
	switch event.Type {
	case "response.created", "response.queued", "response.in_progress":
		valid = event.Response != nil && continuationResponseEligible(event.Response, false)
	case "response.output_item.added", "response.output_item.done":
		valid = event.Item != nil && continuationOutputEligible(*event.Item)
	case "response.content_part.added", "response.content_part.done":
		valid = event.Part != nil && event.Part.Type == "output_text"
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		valid = event.Part != nil && event.Part.Type == "summary_text"
	case "response.output_text.delta", "response.output_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done", "response.reasoning_text.delta":
		// 已有 converter 与实际 client echo 观测器完整覆盖这些内容。
	case "response.completed", "response.done":
		valid = event.Response != nil && continuationResponseEligible(event.Response, true)
	default:
		valid = false
	}
	if !valid {
		observer.invalidate()
	}
}

func continuationResponseEligible(response *apicompat.ResponsesResponse, terminal bool) bool {
	if response == nil || response.Error != nil || response.IncompleteDetails != nil {
		return false
	}
	if terminal && response.Status != "completed" {
		return false
	}
	if !terminal && response.Status != "" && response.Status != "queued" && response.Status != "in_progress" && response.Status != "completed" {
		return false
	}
	for _, item := range response.Output {
		if !continuationOutputEligible(item) {
			return false
		}
	}
	return true
}

func continuationOutputEligible(item apicompat.ResponsesOutput) bool {
	switch item.Type {
	case "message":
		if item.Role != "" && item.Role != "assistant" {
			return false
		}
		if item.Status != "" && item.Status != "in_progress" && item.Status != "completed" {
			return false
		}
		for _, part := range item.Content {
			if part.Type != "output_text" {
				return false
			}
		}
		return true
	case "reasoning":
		for _, summary := range item.Summary {
			if summary.Type != "summary_text" {
				return false
			}
		}
		return true
	case "function_call":
		if strings.TrimSpace(item.CallID) == "" || strings.TrimSpace(item.Name) == "" || item.Namespace != "" || len(item.EncryptedFunctionArgs) > 0 {
			return false
		}
		return item.Status == "" || item.Status == "in_progress" || item.Status == "completed"
	default:
		return false
	}
}

func continuationObserver(c *gin.Context) *continuationObservation {
	if c == nil {
		return nil
	}
	raw, ok := c.Get(compatContinuationObserverContextKey)
	if !ok {
		return nil
	}
	observer, _ := raw.(*continuationObservation)
	return observer
}
