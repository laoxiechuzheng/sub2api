package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// 本文件复现 #8706 的确定性回归：Codex /v1/responses 工具续轮的历史 reasoning
// item 只有 summary（或空 summary + encrypted_content）、缺 DeepSeek thinking mode
// 要求的 content[].type=reasoning_text 时，原生 DeepSeek
// （Credentials api_protocol=responses）会按原文返回 400：
// The `reasoning_text` in the thinking mode must be passed back to the API.
//
// 网关侧修复路径（生产 helper）：出站按 APIKey + item id 读 scoped 缓存
// （deepseek_responses:<APIKeyID>:content:<itemID> 优先，:summary:<itemID> 槽位
// 兜底）回填 content[].reasoning_text；未命中时把已有 summary_text 明文原样
// 重编码；native 响应侧把 reasoning 明文按同样的 scoped 键回写（完整 content
// 优先，摘要只落 summary 槽位）。测试上下文统一注入 api_key（ID 7101）——不注入
// 时网关按设计不读也不写缓存，用例会假绿/假红。
//
// seam 说明：deepSeekReasoningTextStrictUpstream 扮演 api.deepseek.com/responses，
// 收到的报文就是线上报文（svc 真正发往上游的 upstream.lastBody）；校验按官方已知
// 协议约束建模（声明了 tools 且处于 thinking mode 时，每个 assistant 子轮的工具
// 调用前必须有非空 reasoning_text；同一子轮的并行调用共享同一段推理；跨用户轮
// 边界清零），不是官方逐字段实现。全程走 httpUpstreamRecorder 的进程内 mock，
// 不连公网、不产生真实调用。
//
// 边界说明：#8706 未留下诊断报文（captured=false），本文件复现的是
// “summary-only / encrypted-only 的 reasoning 结构 + 原生 DeepSeek 严格校验”
// 这一确定性结构场景；所有 fixtures 均为人工构造，不是该工单的实际正文，
// 也不假设 Codex 会主动丢弃 content（控制例恰恰验证 content 会原样保留）。

// deepSeekResponsesReasoningOKBody 是原生 DeepSeek Responses 端点的成功响应。
const deepSeekResponsesReasoningOKBody = `{"id":"resp_ds_reasoning","object":"response","model":"deepseek-reasoner","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`

// deepSeekReasoningText400Body 是 DeepSeek 的原始 400 文本（用户实际收到的报文）。
const deepSeekReasoningText400Body = `{"error":{"type":"invalid_request_error","message":"The ` + "`reasoning_text`" + ` in the thinking mode must be passed back to the API.","code":"invalid_request_error"}}`

// nativeDeepSeekReasoningTestAPIKeyID 是本文件全部用例统一注入的 gin context
// api_key ID：scoped 缓存键按 APIKey 隔离，缺它时网关不读也不写缓存。
const nativeDeepSeekReasoningTestAPIKeyID int64 = 7101

// deepSeekResponsesReasoningScopedKey 拼出生产侧 scoped 缓存键
// deepseek_responses:<apiKeyID>:<scope>:<itemID>（content / summary 分槽位）。
// 这里独立拼写而不是引用生产常量：钉住键格式契约，避免测试与实现同错。
func deepSeekResponsesReasoningScopedKey(scope, itemID string) string {
	return deepSeekResponsesReasoningScopedKeyForAPIKey(nativeDeepSeekReasoningTestAPIKeyID, scope, itemID)
}

// deepSeekResponsesReasoningScopedKeyForAPIKey 同上，供跨 APIKey 隔离用例使用。
func deepSeekResponsesReasoningScopedKeyForAPIKey(apiKeyID int64, scope, itemID string) string {
	return fmt.Sprintf("deepseek_responses:%d:%s:%s", apiKeyID, scope, itemID)
}

// nativeDeepSeekReasoningTestContext 在共享 fixture 的 gin context 上补 api_key。
func nativeDeepSeekReasoningTestContext(t *testing.T, body []byte) *gin.Context {
	t.Helper()
	c := newDeepSeekChatFallbackContext(t, body)
	c.Set("api_key", &APIKey{ID: nativeDeepSeekReasoningTestAPIKeyID})
	return c
}

// deepSeekReasoningTextStrictUpstream 是 httpUpstreamRecorder 的严格校验变体：
// 校验不通过时返回 DeepSeek 的原始 400（并记录被拒报文），通过才返回预置成功响应。
type deepSeekReasoningTextStrictUpstream struct {
	httpUpstreamRecorder
	rejectedBodies [][]byte
}

func newDeepSeekReasoningTextStrictUpstream() *deepSeekReasoningTextStrictUpstream {
	return &deepSeekReasoningTextStrictUpstream{httpUpstreamRecorder: httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_ds_reasoning_text_ok"}},
		Body:       io.NopCloser(strings.NewReader(deepSeekResponsesReasoningOKBody)),
	}}}
}

// deepSeekResponsesReasoningOKSSEBody 是原生 DeepSeek Responses 的成功 SSE：
// 不带 reasoning 明文，只有成功终态与 [DONE]。
const deepSeekResponsesReasoningOKSSEBody = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ds_reasoning_sse\",\"object\":\"response\",\"model\":\"deepseek-reasoner\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2,\"total_tokens\":7}}}\n\ndata: [DONE]\n\n"

// newDeepSeekReasoningTextStrictSSEUpstream 是严格校验的流式变体：校验不通过时
// 同样返回 DeepSeek 原始 400，通过才返回预置 SSE——覆盖 stream=true 真实出站路径。
func newDeepSeekReasoningTextStrictSSEUpstream() *deepSeekReasoningTextStrictUpstream {
	u := newDeepSeekReasoningTextStrictUpstream()
	u.resp.Header.Set("Content-Type", "text/event-stream")
	u.resp.Body = io.NopCloser(strings.NewReader(deepSeekResponsesReasoningOKSSEBody))
	return u
}

func (u *deepSeekReasoningTextStrictUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.lastReq = req
	u.lastProxyURL = proxyURL
	var body []byte
	if req != nil && req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	u.lastBody = body
	u.bodies = append(u.bodies, append([]byte(nil), body...))
	u.requests = append(u.requests, req)
	if missingDeepSeekReasoningText(body) {
		u.rejectedBodies = append(u.rejectedBodies, append([]byte(nil), body...))
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_ds_reasoning_text_400"}},
			Body:       io.NopCloser(strings.NewReader(deepSeekReasoningText400Body)),
		}, nil
	}
	return u.resp, nil
}

func (u *deepSeekReasoningTextStrictUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

// missingDeepSeekReasoningText 按官方已知协议约束建模 DeepSeek thinking mode 的
// 严格入站校验（非官方逐字段实现）：只有声明了 tools 且处于 thinking mode 的请求
// 才要求回传推理——每个 assistant 子轮（工具调用）之前都必须有一段带非空
// content.reasoning_text 的 reasoning item（summary / encrypted_content 一律被
// 忽略），不能只凭入参里出现过任意一段 reasoning 就放行。
//
// 子轮边界：同一 assistant 子轮的并行工具调用（连续 function_call、中间没有
// *call_output）共享同一段推理，call 本身不清空 pending；pending 只在
// function_call_output / custom_tool_call_output 之后（进入新的 assistant
// 子轮时）或新的 user/system/developer 轮清空——因此 reasoning → call_a →
// output_a → call_b 里 call_b 会被判缺料，而 reasoning → call_a → call_b →
// output_a → output_b 的并行调用通过。
func missingDeepSeekReasoningText(body []byte) bool {
	if len(gjson.GetBytes(body, "tools").Array()) == 0 {
		return false
	}
	if !deepSeekThinkingModeRequest(body) {
		return false
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	pendingReasoningText := false
	for _, item := range input.Array() {
		switch item.Get("type").String() {
		case "reasoning":
			// 有 tools + thinking 时，每个 reasoning item 自身都必须带明文
			// reasoning_text：不允许只靠“入参里别处有明文”蒙混过关（assistant
			// 只有文本回复、没有工具调用时同样要求）。
			if !reasoningItemHasReasoningText(item) {
				return true
			}
			pendingReasoningText = true
		case "function_call", "custom_tool_call":
			if !pendingReasoningText {
				return true
			}
		case "function_call_output", "custom_tool_call_output":
			pendingReasoningText = false
		case "message":
			// 新用户轮边界：不得沿用上一轮子轮的推理明文。
			if role := item.Get("role").String(); role == "user" || role == "system" || role == "developer" {
				pendingReasoningText = false
			}
		}
	}
	return false
}

// reasoningItemHasReasoningText 报告单个 reasoning item 是否带非空 content.reasoning_text。
func reasoningItemHasReasoningText(item gjson.Result) bool {
	for _, part := range item.Get("content").Array() {
		if part.Get("type").String() == "reasoning_text" && strings.TrimSpace(part.Get("text").String()) != "" {
			return true
		}
	}
	return false
}

// deepSeekThinkingModeRequest 判断请求是否处于 thinking mode（建模用）：
// 显式 reasoning.effort 或 reasoner 模型名任一满足即可。
func deepSeekThinkingModeRequest(body []byte) bool {
	if strings.TrimSpace(gjson.GetBytes(body, "reasoning.effort").String()) != "" {
		return true
	}
	return strings.Contains(strings.ToLower(gjson.GetBytes(body, "model").String()), "reasoner")
}

// 校验器的契约测试：确认建模规则按官方已知协议约束执行（tools+thinking gate、
// 每个工具子轮各自需要 reasoning_text、同子轮并行调用共享同一段推理、跨用户轮
// 边界清零），避免把 mock 自身规则写错造成假红/假绿。
func TestDeepSeekReasoningTextStrictMockValidationContract(t *testing.T) {
	const toolsJSON = `"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{}}}]`
	prefix := func(model string, rest string) string {
		return `{"model":"` + model + `",` + toolsJSON + `,` + rest + `}`
	}
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			// 无 tools：即使 history 是 summary-only + call，服务端不要求回传推理。
			name: "no_tools_not_rejected",
			body: `{"model":"deepseek-reasoner","input":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"short"}],"content":null},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}]}`,
			want: false,
		},
		{
			// 有 tools 但非 thinking mode：不做该校验。
			name: "tools_without_thinking_mode_not_rejected",
			body: prefix("deepseek-chat", `"input":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"short"}],"content":null},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}]}`),
			want: false,
		},
		{
			// assistant 只有文本回复、没有工具调用：同一约束同样要求 reasoning 明文。
			name: "assistant_message_without_toolcall_rejected",
			body: prefix("deepseek-reasoner", `"input":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"short"}],"content":null},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}]}`),
			want: true,
		},
		{
			name: "reasoning_text_before_single_call",
			body: prefix("deepseek-reasoner", `"input":[{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"raw"}]},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}]`),
			want: false,
		},
		{
			// 同一子轮的并行调用：call_a/call_b 之间没有 output，共享同一段推理。
			name: "parallel_calls_share_single_reasoning",
			body: prefix("deepseek-reasoner", `"input":[{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"raw"}]},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":"a"},{"type":"function_call_output","call_id":"call_b","output":"b"}]`),
			want: false,
		},
		{
			// 串行两个子轮：output_a 之后已进入新子轮，call_b 前缺新 reasoning。
			name: "chained_second_call_without_new_reasoning",
			body: prefix("deepseek-reasoner", `"input":[{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"raw"}]},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":"a"},{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_b","output":"b"}]`),
			want: true,
		},
		{
			// 并行之后进入新子轮：outputs 清空 pending，call_c 前缺新 reasoning。
			name: "parallel_then_new_subturn_without_new_reasoning",
			body: prefix("deepseek-reasoner", `"input":[{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"raw"}]},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":"a"},{"type":"function_call_output","call_id":"call_b","output":"b"},{"type":"function_call","call_id":"call_c","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_c","output":"c"}]`),
			want: true,
		},
		{
			// 跨用户轮边界：新 user 轮之后不得再借用上一轮的推理明文。
			name: "call_after_new_user_turn_rejected",
			body: prefix("deepseek-reasoner", `"input":[{"type":"reasoning","id":"r1","content":[{"type":"reasoning_text","text":"raw"}]},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]},{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"}]`),
			want: true,
		},
		{
			name: "summary_only_rejected",
			body: prefix("deepseek-reasoner", `"input":[{"type":"reasoning","id":"r1","summary":[{"type":"summary_text","text":"short"}],"content":null},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}]`),
			want: true,
		},
		{
			name: "encrypted_only_rejected",
			body: prefix("deepseek-reasoner", `"input":[{"type":"reasoning","id":"r1","summary":[],"encrypted_content":"opaque"},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"}]`),
			want: true,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, missingDeepSeekReasoningText([]byte(tc.body)))
		})
	}
}

// reasoningSharedCache 模拟跨轮共享的 Redis reasoning 缓存（写后读可见）：
// 键就是网关传入的完整字符串（native helper 传 scoped 键，旧 Chat 桥接传裸
// item id），并记录全部读取键，供用例断言“只读 scoped 键、不回读旧全局键”。
type reasoningSharedCache struct {
	stubGatewayCache
	mu         sync.Mutex
	values     map[string]string
	lookupKeys []string
	getErr     error // 非 nil 时 Get 一律返回该错误（模拟缓存故障）
}

func (c *reasoningSharedCache) SetReasoningContent(_ context.Context, cacheKey string, content string, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = make(map[string]string)
	}
	c.values[cacheKey] = content
	return nil
}

func (c *reasoningSharedCache) GetReasoningContent(_ context.Context, cacheKey string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lookupKeys = append(c.lookupKeys, cacheKey)
	if c.getErr != nil {
		return "", c.getErr
	}
	if v, ok := c.values[cacheKey]; ok {
		return v, nil
	}
	return "", ErrReasoningContentNotFound
}

func (c *reasoningSharedCache) snapshot() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.values))
	for k, v := range c.values {
		out[k] = v
	}
	return out
}

func (c *reasoningSharedCache) lookups() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lookupKeys...)
}

func (c *reasoningSharedCache) seed(cacheKey, content string) {
	_ = c.SetReasoningContent(context.Background(), cacheKey, content, time.Hour)
}

// nativeDeepSeekResponsesReasoningAccount 是原生 DeepSeek Responses 账号
// （Credentials api_protocol=responses，上游为 https://api.deepseek.com/responses）。
func nativeDeepSeekResponsesReasoningAccount(id int64, name string) *Account {
	return &Account{
		ID:       id,
		Name:     name,
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"api_protocol": APIProtocolResponses,
		},
	}
}

// adaptiveNativeDeepSeekResponsesReasoningAccount 是生产形态的自适应账号
// （deepseek/apikey，api_protocol=adaptive，探测后 extra
// openai_responses_mode=force_responses）：原生 Responses 走官方端点。
func adaptiveNativeDeepSeekResponsesReasoningAccount(id int64) *Account {
	return &Account{
		ID:       id,
		Name:     "adaptive-native-deepseek-responses",
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceResponses),
		},
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"api_protocol": APIProtocolAdaptive,
		},
	}
}

// 控制例：历史 reasoning item 已自带 content[].reasoning_text 时，网关必须原样
// 保持并成功透传——同时证明严格 mock 只在“声明 tools + thinking mode + 缺料”时
// 才返回 400，红灯不是 mock 造成的。缓存里预置了不同明文，验证已有 content 既不
// 被缓存也不被 summary 覆盖（exact keep）。
func TestForwardNativeDeepSeekResponsesKeepsReasoningTextContent(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-reasoner",
		"stream":false,
		"reasoning":{"effort":"medium"},
		"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{}}}],
		"input":[
			{"type":"reasoning","id":"rs_keep","summary":[{"type":"summary_text","text":"raw chain of thought"}],"content":[{"type":"reasoning_text","text":"raw chain of thought"}]},
			{"type":"function_call","call_id":"call_keep","name":"exec","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_keep","output":"ok"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	c := nativeDeepSeekReasoningTestContext(t, body)
	upstream := newDeepSeekReasoningTextStrictUpstream()
	cache := &reasoningSharedCache{}
	cache.seed(deepSeekResponsesReasoningScopedKey("content", "rs_keep"), "different cached plaintext")
	cache.seed(deepSeekResponsesReasoningScopedKey("summary", "rs_keep"), "different cached summary")
	svc := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: upstream,
		cache:        cache,
	}

	result, err := svc.Forward(context.Background(), c, nativeDeepSeekResponsesReasoningAccount(19, "native-deepseek-responses-keep"), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "https://api.deepseek.com/responses", upstream.lastReq.URL.String())
	require.Empty(t, upstream.rejectedBodies, "带 reasoning_text 的请求不应被严格 mock 拒绝")
	restored := gjson.GetBytes(upstream.lastBody, `input.#(type=="reasoning").content.0`)
	require.Equal(t, "reasoning_text", restored.Get("type").String())
	require.Equal(t, "raw chain of thought", restored.Get("text").String())
	require.Empty(t, cache.lookups(), "已有合法 content 时不得回查缓存")
}

// 工具续轮回归：
//  1. summary_only：客户端回放的历史带明文 summary（挑不同于缓存全文的文本，
//     防止 summary 冒充缓存全文），缺 content，必须按 item id 从 scoped content
//     槽位补回真实全文；
//  2. encrypted_only：summary 为空、只有 encrypted_content，同样只能靠缓存补回。
//
// scoped 缓存里已有该 item id 的推理全文，期望网关送出补回 content[].reasoning_text
// 的报文并成功；严格 mock 缺料即按 DeepSeek 原始 400 拒绝，require.NoError 即红灯。
func TestForwardNativeDeepSeekResponsesRestoresReasoningTextFromCache(t *testing.T) {
	const (
		cachedItemID       = "item_cached_reasoning"
		fullReasoningText  = "fallback raw thinking from previous turn"
		summaryOnlyText    = "concise summary of the previous turn"
		functionCallBodyID = "call_cached"
	)

	buildBody := func(reasoningItem string) []byte {
		return []byte(`{
			"model":"deepseek-reasoner",
			"stream":false,
			"reasoning":{"effort":"medium"},
			"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{}}}],
			"input":[
				` + reasoningItem + `,
				{"type":"function_call","call_id":"` + functionCallBodyID + `","name":"exec","arguments":"{\"cmd\":\"ls\"}"},
				{"type":"function_call_output","call_id":"` + functionCallBodyID + `","output":"ok"},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
			]
		}`)
	}

	cases := []struct {
		name          string
		reasoningItem string
	}{
		{
			name:          "summary_only",
			reasoningItem: `{"type":"reasoning","id":"` + cachedItemID + `","summary":[{"type":"summary_text","text":"` + summaryOnlyText + `"}],"content":null}`,
		},
		{
			name:          "empty_summary_encrypted_only",
			reasoningItem: `{"type":"reasoning","id":"` + cachedItemID + `","summary":[],"encrypted_content":"opaque"}`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := buildBody(tc.reasoningItem)
			c := nativeDeepSeekReasoningTestContext(t, body)
			upstream := newDeepSeekReasoningTextStrictUpstream()
			cache := &reasoningSharedCache{}
			cache.seed(deepSeekResponsesReasoningScopedKey("content", cachedItemID), fullReasoningText)
			svc := &OpenAIGatewayService{
				cfg:          deepSeekChatFallbackTestConfig(),
				httpUpstream: upstream,
				cache:        cache,
			}

			result, err := svc.Forward(context.Background(), c, nativeDeepSeekResponsesReasoningAccount(20, "native-deepseek-responses-cache"), body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, "https://api.deepseek.com/responses", upstream.lastReq.URL.String())
			require.Empty(t, upstream.rejectedBodies, "缺 reasoning_text 时严格 mock 会返回 DeepSeek 原始 400")

			// 补回的必须是 scoped content 槽位的缓存全文，而不是 summary 原文或占位符。
			restored := gjson.GetBytes(upstream.lastBody, `input.#(type=="reasoning").content.0`)
			require.Equal(t, "reasoning_text", restored.Get("type").String())
			require.Equal(t, fullReasoningText, restored.Get("text").String())
			// 读取必须只用 scoped 键、且 content 槽位优先命中（不回落 summary）。
			require.Equal(t, []string{deepSeekResponsesReasoningScopedKey("content", cachedItemID)}, cache.lookups())
		})
	}
}

// 最小原生链式工具序列回归：同一 user turn 里连续两个工具子轮，历史是
// reasoning（带 content 全文）→ call_a → output_a → call_b → output_b，
// call_b 前没有新的 reasoning。网关应在同 user 段内继承最近一段真实明文，
// 给 call_b 补一段 reasoning；原样转发会被 DeepSeek 按“每个子轮都要
// reasoning_text”拒绝（strict mock）。
func TestForwardNativeDeepSeekResponsesBackfillsReasoningForChainedToolCalls(t *testing.T) {
	const fullReasoningText = "chained raw thinking"
	body := []byte(`{
		"model":"deepseek-reasoner",
		"stream":false,
		"reasoning":{"effort":"medium"},
		"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{}}}],
		"input":[
			{"type":"reasoning","id":"item_chain","summary":[{"type":"summary_text","text":"` + fullReasoningText + `"}],"content":[{"type":"reasoning_text","text":"` + fullReasoningText + `"}]},
			{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{\"cmd\":\"ls\"}"},
			{"type":"function_call_output","call_id":"call_a","output":"a"},
			{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{\"cmd\":\"pwd\"}"},
			{"type":"function_call_output","call_id":"call_b","output":"b"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	c := nativeDeepSeekReasoningTestContext(t, body)
	upstream := newDeepSeekReasoningTextStrictUpstream()
	// 本用例的明文就在同段历史里，继承不依赖缓存。
	svc := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: upstream,
	}

	result, err := svc.Forward(context.Background(), c, nativeDeepSeekResponsesReasoningAccount(22, "native-deepseek-responses-chain"), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Empty(t, upstream.rejectedBodies, "call_b 前缺 reasoning_text 时严格 mock 返回 DeepSeek 原始 400")
	// 修复不得以丢弃 call_b 绕过校验：两个子轮必须都保留在线上报文里，
	// 且 call_b 前补了一段可继承的 reasoning 明文。
	require.Len(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call")#`).Array(), 2)
	require.Len(t, gjson.GetBytes(upstream.lastBody, `input.#(type=="reasoning")#`).Array(), 2)
	require.Equal(t, fullReasoningText, gjson.GetBytes(upstream.lastBody, `input.#(type=="reasoning").content.0.text`).String())
}

// extractChatFallbackReplayItems 从第 1 轮 SSE 响应里取出 reasoning item 与
// function_call item 的关键字段，模拟 Codex 把上一轮输出原样放进下一轮 input。
func extractChatFallbackReplayItems(t *testing.T, sse string) (reasoningID, reasoningSummary, callID, callName, callArgs string) {
	t.Helper()
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" || !gjson.Valid(payload) {
			continue
		}
		if gjson.Get(payload, "type").String() != "response.output_item.done" {
			continue
		}
		item := gjson.Get(payload, "item")
		switch item.Get("type").String() {
		case "reasoning":
			if reasoningID == "" {
				reasoningID = item.Get("id").String()
				reasoningSummary = item.Get("summary.0.text").String()
			}
		case "function_call":
			if callID == "" {
				callID = item.Get("call_id").String()
				callName = item.Get("name").String()
				callArgs = item.Get("arguments").String()
			}
		}
	}
	require.NotEmpty(t, reasoningID, "第 1 轮应输出 reasoning item")
	require.NotEmpty(t, reasoningSummary, "第 1 轮 reasoning item 应带 summary 明文")
	require.NotEmpty(t, callID, "第 1 轮应输出 function_call item")
	return reasoningID, reasoningSummary, callID, callName, callArgs
}

// 端到端双轮链路（贴近 #8706 的 Codex 实务路径）：
//  1. 第 1 轮经既有 Chat fallback 生成 reasoning + function_call，桥接把推理全文
//     按裸 item id 写入缓存（旧全局键，既有行为）。这里的 summary 携带全文是
//     “兼容 provider 桥接把 reasoning 原文写进 summary” 的 fixture 形状，
//     不代表真实摘要字段语义——真实 summary 是摘要，不是完整 CoT；
//  2. 第 2 轮把第 1 轮返回的 item 原样回放给原生 DeepSeek Responses 账号（同平台
//     的不同账号）。native 回注不读旧全局键，未命中 scoped 缓存时按 summary 明文
//     原样重编码兜底，因此本轮必须成功；读取日志断言只尝试 scoped 两槽位。
func TestForwardNativeDeepSeekResponsesReplaysChatFallbackReasoningWithCache(t *testing.T) {
	const fullReasoningText = "fallback full thinking"

	// 第 1 轮：Chat fallback 流式返回 reasoning_content + tool_call。
	round1Body := []byte(`{"model":"deepseek-reasoner","input":"hello","stream":true}`)
	round1Rec := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(round1Rec)
	c1.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(round1Body))
	c1.Request.Header.Set("Content-Type", "application/json")
	c1.Set("api_key", &APIKey{ID: nativeDeepSeekReasoningTestAPIKeyID})
	round1UpstreamBody := strings.Join([]string{
		`data: {"id":"chatcmpl_replay","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_replay","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"` + fullReasoningText + `"},"finish_reason":null}]}`,
		"",
		`data: {"id":"chatcmpl_replay","object":"chat.completion.chunk","model":"deepseek-reasoner","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_replay","type":"function","function":{"name":"exec","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	round1Upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_replay_round1"}},
		Body:       io.NopCloser(strings.NewReader(round1UpstreamBody)),
	}}
	cache := &reasoningSharedCache{}
	svc1 := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: round1Upstream,
		cache:        cache,
	}

	round1Result, err := svc1.Forward(context.Background(), c1, openaiPlatformDeepSeekAccount(), round1Body)
	require.NoError(t, err)
	require.NotNil(t, round1Result)
	require.Equal(t, "https://api.deepseek.com/v1/chat/completions", round1Upstream.lastReq.URL.String())

	// 第 1 轮返回给 Codex 的 reasoning item 与 function_call item。
	reasoningID, reasoningSummary, callID, callName, callArgs := extractChatFallbackReplayItems(t, round1Rec.Body.String())

	// 写入侧（既有桥接行为）：推理全文按裸 item id 落缓存——这就是旧全局键，
	// 第 2 轮 native 回注必须只认 scoped 键、不得回读它（下面用读取日志断言）。
	require.Equal(t, map[string]string{reasoningID: fullReasoningText}, cache.snapshot())
	round1Lookups := len(cache.lookups())

	// 第 2 轮：Codex 原样回放第 1 轮的 reasoning item（只有 summary、无 content）
	// 与 function_call / function_call_output，路由到原生 DeepSeek Responses 账号。
	replayInput := []map[string]any{
		{
			"type":    "reasoning",
			"id":      reasoningID,
			"summary": []map[string]any{{"type": "summary_text", "text": reasoningSummary}},
		},
		{"type": "function_call", "call_id": callID, "name": callName, "arguments": callArgs},
		{"type": "function_call_output", "call_id": callID, "output": "ok"},
		{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "continue"}}},
	}
	round2Body, err := json.Marshal(map[string]any{
		"model":     "deepseek-reasoner",
		"stream":    false,
		"reasoning": map[string]any{"effort": "medium"},
		"tools": []map[string]any{
			{"type": "function", "name": "exec", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		},
		"input": replayInput,
	})
	require.NoError(t, err)

	c2 := nativeDeepSeekReasoningTestContext(t, round2Body)
	round2Upstream := newDeepSeekReasoningTextStrictUpstream()
	svc2 := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: round2Upstream,
		cache:        cache,
	}

	round2Result, err := svc2.Forward(context.Background(), c2, nativeDeepSeekResponsesReasoningAccount(21, "native-deepseek-responses-replay"), round2Body)
	require.NoError(t, err)
	require.NotNil(t, round2Result)
	require.Equal(t, "https://api.deepseek.com/responses", round2Upstream.lastReq.URL.String())
	require.Empty(t, round2Upstream.rejectedBodies, "回放缺 reasoning_text 时严格 mock 会返回 DeepSeek 原始 400")

	restored := gjson.GetBytes(round2Upstream.lastBody, `input.#(type=="reasoning").content.0`)
	require.Equal(t, "reasoning_text", restored.Get("type").String())
	require.Equal(t, fullReasoningText, restored.Get("text").String(), "未命中 scoped 缓存时按 summary 明文原样重编码兜底")

	// 第 2 轮不得读到第 1 轮的裸全局键：读取日志剔除第 1 轮后必须恰为 scoped 两槽位。
	require.Equal(t, []string{
		deepSeekResponsesReasoningScopedKey("content", reasoningID),
		deepSeekResponsesReasoningScopedKey("summary", reasoningID),
	}, cache.lookups()[round1Lookups:])
}

// thirdPartyNativeDeepSeekResponsesReasoningAccount 模拟生产侧的第三方原生
// Responses 端点账号（deepseek/apikey，base_url 指向第三方主机，如 ai.akile.ai；
// 生产为 adaptive+force_responses，这里用显式 api_protocol=responses 表达同一语义）。
func thirdPartyNativeDeepSeekResponsesReasoningAccount(id int64, baseURL string) *Account {
	return &Account{
		ID:       id,
		Name:     "third-party-native-deepseek-responses",
		Platform: PlatformDeepseek,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":      "sk-test",
			"base_url":     baseURL,
			"api_protocol": APIProtocolResponses,
		},
	}
}

// 原生闭环回归：整条链路不经过 Chat fallback——
//  1. 第 1 轮打第三方原生 Responses（base_url=第三方主机），上游返回 summary-only
//     的 reasoning item + function_call（兼容 provider 桥接把 reasoning 全文放进
//     summary 的 fixture 形状，不代表 summary 字段的真实摘要语义）。网关把摘要
//     明文按 scoped summary 键落缓存（不写裸 item id；content 槽位留给带完整
//     content 的响应），供后续轮次兜底；
//  2. 第 2 轮把第 1 轮返回的 item 原样回放给官方 api.deepseek.com 的原生
//     Responses 账号：content 槽位未命中，按 summary 槽位兜底补回
//     content[].reasoning_text 后成功。
func TestForwardNativeDeepSeekResponsesReplaysThirdPartyNativeReasoningWithCache(t *testing.T) {
	const fullReasoningText = "third party native raw thinking"

	// 第 1 轮：第三方原生 Responses，输出 summary-only 的 reasoning + function_call。
	round1Body := []byte(`{"model":"deepseek-reasoner","stream":false,"reasoning":{"effort":"medium"},"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{}}}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}]}`)
	round1Rec := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(round1Rec)
	c1.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(round1Body))
	c1.Request.Header.Set("Content-Type", "application/json")
	c1.Set("api_key", &APIKey{ID: nativeDeepSeekReasoningTestAPIKeyID})
	round1Upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}, "x-request-id": []string{"rid_native_round1"}},
		Body: io.NopCloser(strings.NewReader(
			`{"id":"resp_third_party","object":"response","model":"deepseek-reasoner","status":"completed","output":[` +
				`{"type":"reasoning","id":"rs_native_1","summary":[{"type":"summary_text","text":"` + fullReasoningText + `"}]},` +
				`{"type":"function_call","call_id":"call_native_1","name":"exec","arguments":"{\"cmd\":\"ls\"}"}` +
				`],"usage":{"input_tokens":4,"output_tokens":3,"total_tokens":7}}`,
		)),
	}}
	cache := &reasoningSharedCache{}
	svc1 := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: round1Upstream,
		cache:        cache,
	}

	round1Result, err := svc1.Forward(context.Background(), c1, thirdPartyNativeDeepSeekResponsesReasoningAccount(23, "https://ai.akile.ai"), round1Body)
	require.NoError(t, err)
	require.NotNil(t, round1Result)
	require.Contains(t, round1Upstream.lastReq.URL.String(), "/responses")

	relayed := round1Rec.Body.String()
	reasoningID := gjson.Get(relayed, `output.#(type=="reasoning").id`).String()
	require.NotEmpty(t, reasoningID, "转发的响应应保留 reasoning item id 供后续轮次回放")
	callID := gjson.Get(relayed, `output.#(type=="function_call").call_id`).String()
	require.NotEmpty(t, callID)

	// 写入侧：原生转发的响应里 reasoning 只有 summary 明文，按 scoped summary 键
	// 落缓存（不是裸 item id，也不是 content 槽位）。
	require.Equal(t, map[string]string{
		deepSeekResponsesReasoningScopedKey("summary", reasoningID): fullReasoningText,
	}, cache.snapshot())
	round1Lookups := len(cache.lookups())

	// 第 2 轮：原样回放给官方原生 DeepSeek，期望从缓存补回 reasoning_text。
	replayInput := []map[string]any{
		{
			"type":    "reasoning",
			"id":      reasoningID,
			"summary": []map[string]any{{"type": "summary_text", "text": fullReasoningText}},
		},
		{"type": "function_call", "call_id": callID, "name": "exec", "arguments": `{"cmd":"ls"}`},
		{"type": "function_call_output", "call_id": callID, "output": "ok"},
		{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "continue"}}},
	}
	round2Body, err := json.Marshal(map[string]any{
		"model":     "deepseek-reasoner",
		"stream":    false,
		"reasoning": map[string]any{"effort": "medium"},
		"tools": []map[string]any{
			{"type": "function", "name": "exec", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		},
		"input": replayInput,
	})
	require.NoError(t, err)

	c2 := nativeDeepSeekReasoningTestContext(t, round2Body)
	round2Upstream := newDeepSeekReasoningTextStrictUpstream()
	svc2 := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: round2Upstream,
		cache:        cache,
	}

	round2Result, err := svc2.Forward(context.Background(), c2, nativeDeepSeekResponsesReasoningAccount(24, "native-deepseek-responses-official"), round2Body)
	require.NoError(t, err)
	require.NotNil(t, round2Result)
	require.Equal(t, "https://api.deepseek.com/responses", round2Upstream.lastReq.URL.String())
	require.Empty(t, round2Upstream.rejectedBodies, "回放缺 reasoning_text 时严格 mock 会返回 DeepSeek 原始 400")

	restored := gjson.GetBytes(round2Upstream.lastBody, `input.#(type=="reasoning").content.0`)
	require.Equal(t, "reasoning_text", restored.Get("type").String())
	require.Equal(t, fullReasoningText, restored.Get("text").String(), "content 槽位未命中时按 summary 槽位兜底回注")

	// 第 2 轮只允许读 scoped 键：content 未命中后按 summary 槽位兜底命中。
	require.Equal(t, []string{
		deepSeekResponsesReasoningScopedKey("content", reasoningID),
		deepSeekResponsesReasoningScopedKey("summary", reasoningID),
	}, cache.lookups()[round1Lookups:])
}

// collector 写入回归（partial risk；修复前本用例 RED）：
//   - 中断流：既有完整 content 缓存不得被 reasoning_text.done 的残缺片段覆盖
//     （读断/response.failed 时没有 item.done 或成功终态，只有 content_index=1
//     的片段 B，不能把 AB 覆盖成 B）；
//   - 完整流：item.done 已给出 content=[A,B]、terminal 又带更短 summary 时，
//     content 槽位仍必须是 AB（摘要只落 summary 槽位）。
func TestDeepSeekNativeReasoningCachePartialStreamKeepsExistingFullContent(t *testing.T) {
	contentKey := func(itemID string) string {
		return deepSeekResponsesReasoningScopedKey("content", itemID)
	}

	t.Run("interrupted_stream_partial_text_does_not_overwrite_existing_content", func(t *testing.T) {
		cache := &reasoningSharedCache{}
		cache.seed(contentKey("r"), "AB")
		svc := &OpenAIGatewayService{cache: cache}
		collector := newDeepSeekNativeReasoningCacheCollector(nativeDeepSeekReasoningTestAPIKeyID)
		collector.collectStreamEvent([]byte(`{"type":"response.reasoning_text.done","item_id":"r","content_index":1,"text":"B"}`), "response.reasoning_text.done")
		collector.collectStreamEvent([]byte(`{"type":"response.failed","response":{"id":"resp_partial","status":"failed"}}`), "response.failed")
		svc.flushDeepSeekNativeResponsesReasoningCache(collector)

		require.Equal(t, "AB", cache.snapshot()[contentKey("r")], "残缺片段不得覆盖既有完整 content")
	})

	t.Run("full_item_content_not_overwritten_by_terminal_summary", func(t *testing.T) {
		cache := &reasoningSharedCache{}
		svc := &OpenAIGatewayService{cache: cache}
		collector := newDeepSeekNativeReasoningCacheCollector(nativeDeepSeekReasoningTestAPIKeyID)
		collector.collectStreamEvent([]byte(`{"type":"response.output_item.done","item":{"type":"reasoning","id":"r_full","content":[{"type":"reasoning_text","text":"A"},{"type":"reasoning_text","text":"B"}]}}`), "response.output_item.done")
		collector.collectStreamEvent([]byte(`{"type":"response.completed","response":{"output":[{"type":"reasoning","id":"r_full","summary":[{"type":"summary_text","text":"short"}]}]}}`), "response.completed")
		svc.flushDeepSeekNativeResponsesReasoningCache(collector)

		require.Equal(t, map[string]string{contentKey("r_full"): "AB"}, cache.snapshot())
	})

	t.Run("other_item_done_does_not_promote_isolated_part", func(t *testing.T) {
		// r 只收到 content_index=0 的孤立片段，随后同流里 r2 收到了完整 item.done、
		// 全局 terminal 也到达；r 不是完整 item，不得借“全局完成”把 AB 变成 A。
		cache := &reasoningSharedCache{}
		cache.seed(contentKey("r"), "AB")
		svc := &OpenAIGatewayService{cache: cache}
		collector := newDeepSeekNativeReasoningCacheCollector(nativeDeepSeekReasoningTestAPIKeyID)
		collector.collectStreamEvent([]byte(`{"type":"response.reasoning_text.done","item_id":"r","content_index":0,"text":"A"}`), "response.reasoning_text.done")
		collector.collectStreamEvent([]byte(`{"type":"response.output_item.done","item":{"type":"reasoning","id":"r2","content":[{"type":"reasoning_text","text":"XY"}]}}`), "response.output_item.done")
		collector.collectStreamEvent([]byte(`{"type":"response.completed","response":{"output":[{"type":"reasoning","id":"r2","content":[{"type":"reasoning_text","text":"XY"}]}]}}`), "response.completed")
		svc.flushDeepSeekNativeResponsesReasoningCache(collector)

		require.Equal(t, "AB", cache.snapshot()[contentKey("r")], "其他 item 的完整 item.done 不得把孤立片段提升为 r 的完整 content")
	})

	t.Run("cache_get_error_does_not_promote_parts", func(t *testing.T) {
		// scoped content 槽读缓存失败时不得把片段当完整明文写入（也不能覆盖已有值）。
		cache := &reasoningSharedCache{getErr: errors.New("cache unavailable")}
		cache.seed(contentKey("r"), "AB")
		svc := &OpenAIGatewayService{cache: cache}
		collector := newDeepSeekNativeReasoningCacheCollector(nativeDeepSeekReasoningTestAPIKeyID)
		collector.collectStreamEvent([]byte(`{"type":"response.reasoning_text.done","item_id":"r","content_index":0,"text":"A"}`), "response.reasoning_text.done")
		collector.collectStreamEvent([]byte(`{"type":"response.output_item.done","item":{"type":"reasoning","id":"r","content":[]}}`), "response.output_item.done")
		svc.flushDeepSeekNativeResponsesReasoningCache(collector)

		require.Equal(t, "AB", cache.snapshot()[contentKey("r")], "读缓存失败时不得用片段覆盖/提升 content")
	})
}

// 自适应账号原生 Responses 流式双轮闭环（生产形态：deepseek/apikey +
// api_protocol=adaptive + extra openai_responses_mode=force_responses）：
//  1. 第 1 轮 stream=true 打官方端点，上游 SSE 的完整 item.done 带
//     content[].reasoning_text 全文——网关按 scoped content 槽位落缓存，转发给
//     客户端的 SSE 保持上游 data 行原样（无模型映射时不改写）；
//  2. 第 2 轮同 APIKey（另一个账号实例）回放：客户端只有 summary、没有 content，
//     stream=true 经严格 mock 校验，必须从缓存补回原始全文（而不是客户端
//     summary），证明真实 native 流式出站路径同样受回注保护。
func TestForwardAdaptiveNativeDeepSeekResponsesStreamReasoningRoundTrip(t *testing.T) {
	const (
		fullReasoningText = "adaptive native stream raw thinking"
		shortSummaryText  = "short summary that must not replace full thinking"
	)
	newContext := func(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set("api_key", &APIKey{ID: nativeDeepSeekReasoningTestAPIKeyID})
		return c, rec
	}

	// 第 1 轮：上游 SSE 输出完整 reasoning content + function_call + 成功终态。
	round1Body := []byte(`{"model":"deepseek-reasoner","stream":true,"reasoning":{"effort":"medium"},"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{}}}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]}]}`)
	round1SSE := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_stream_native","object":"response","model":"deepseek-reasoner","status":"in_progress"}}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_native_stream","summary":[{"type":"summary_text","text":"` + fullReasoningText + `"}],"content":[{"type":"reasoning_text","text":"` + fullReasoningText + `"}]}}`,
		"",
		"event: response.output_item.done",
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_native_stream","call_id":"call_native_stream","name":"exec","arguments":"{\"cmd\":\"ls\"}"}}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"id":"resp_stream_native","object":"response","model":"deepseek-reasoner","status":"completed","output":[{"type":"reasoning","id":"rs_native_stream","summary":[{"type":"summary_text","text":"` + fullReasoningText + `"}],"content":[{"type":"reasoning_text","text":"` + fullReasoningText + `"}]},{"type":"function_call","id":"fc_native_stream","call_id":"call_native_stream","name":"exec","arguments":"{\"cmd\":\"ls\"}"}],"usage":{"input_tokens":12,"output_tokens":7,"total_tokens":19}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")
	c1, rec1 := newContext(round1Body)
	round1Upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}, "x-request-id": []string{"rid_native_stream_round1"}},
		Body:       io.NopCloser(strings.NewReader(round1SSE)),
	}}
	cache := &reasoningSharedCache{}
	svc1 := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: round1Upstream,
		cache:        cache,
	}

	round1Result, err := svc1.Forward(context.Background(), c1, adaptiveNativeDeepSeekResponsesReasoningAccount(25), round1Body)
	require.NoError(t, err)
	require.NotNil(t, round1Result)
	require.Equal(t, "https://api.deepseek.com/responses", round1Upstream.lastReq.URL.String())

	// 客户端 SSE 字节保持：上游每个事件 data 行都应原样出现在转发流里。裸
	// [DONE] 终结符由既有 relay 归一化吞掉（Responses 协议以 response.completed
	// 为终态），属于既有差异而非本次 reasoning 往返改动。
	relayed := rec1.Body.String()
	for _, line := range strings.Split(round1SSE, "\n") {
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			require.Contains(t, relayed, line)
		}
	}
	require.NotContains(t, relayed, "response.failed")

	// 写入侧：完整 content 落 scoped content 槽位（不写裸 id、不写 summary 槽位）。
	require.Equal(t, map[string]string{
		deepSeekResponsesReasoningScopedKey("content", "rs_native_stream"): fullReasoningText,
	}, cache.snapshot())
	require.Empty(t, cache.lookups(), "第 1 轮出站无历史 reasoning，不应发生缓存读取")

	// 第 2 轮：同 APIKey 回放（只有 summary、没有 content），仍走 stream=true。
	round2Body, err := json.Marshal(map[string]any{
		"model":     "deepseek-reasoner",
		"stream":    true,
		"reasoning": map[string]any{"effort": "medium"},
		"tools": []map[string]any{
			{"type": "function", "name": "exec", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
		},
		"input": []map[string]any{
			{"type": "reasoning", "id": "rs_native_stream", "summary": []map[string]any{{"type": "summary_text", "text": shortSummaryText}}},
			{"type": "function_call", "call_id": "call_native_stream", "name": "exec", "arguments": `{"cmd":"ls"}`},
			{"type": "function_call_output", "call_id": "call_native_stream", "output": "ok"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "continue"}}},
		},
	})
	require.NoError(t, err)

	c2, rec2 := newContext(round2Body)
	round2Upstream := newDeepSeekReasoningTextStrictSSEUpstream()
	svc2 := &OpenAIGatewayService{
		cfg:          deepSeekChatFallbackTestConfig(),
		httpUpstream: round2Upstream,
		cache:        cache,
	}

	round2Result, err := svc2.Forward(context.Background(), c2, adaptiveNativeDeepSeekResponsesReasoningAccount(26), round2Body)
	require.NoError(t, err)
	require.NotNil(t, round2Result)
	require.Equal(t, "https://api.deepseek.com/responses", round2Upstream.lastReq.URL.String())
	require.Empty(t, round2Upstream.rejectedBodies, "缺 reasoning_text 时严格 mock 返回 DeepSeek 原始 400")

	restored := gjson.GetBytes(round2Upstream.lastBody, `input.#(type=="reasoning").content.0`)
	require.Equal(t, "reasoning_text", restored.Get("type").String())
	require.Equal(t, fullReasoningText, restored.Get("text").String(), "必须补回 scoped 缓存的原始全文，而不是客户端 summary")
	require.Equal(t, []string{deepSeekResponsesReasoningScopedKey("content", "rs_native_stream")}, cache.lookups())

	// 第 2 轮转发 SSE 保持成功终态、不注入错误事件。
	round2Relayed := rec2.Body.String()
	require.Contains(t, round2Relayed, `"type":"response.completed"`)
	require.NotContains(t, round2Relayed, "response.failed")
}

// restore helper 的门卫表：按 APIKey 隔离缓存键、无 APIKey 不做缓存 IO、
// effort=none（两种字段）/compact/非 native 账号直接跳过；同 user 段内的 chain
// 继承在用户轮边界与 encrypted cache-miss 后必须清空，parallel 调用不插占位。
func TestRestoreDeepSeekNativeResponsesReasoningGuardsTable(t *testing.T) {
	const (
		toolsJSON  = `"tools":[{"type":"function","name":"exec","parameters":{"type":"object","properties":{}}}]`
		mediumJSON = `"reasoning":{"effort":"medium"}`
	)
	bodyWith := func(reasoningJSON, inputJSON string) string {
		return `{"model":"deepseek-reasoner","stream":false,` + reasoningJSON + `,` + toolsJSON + `,"input":` + inputJSON + `}`
	}
	summaryOnlyInput := `[{"type":"reasoning","id":"r","summary":[{"type":"summary_text","text":"SUM"}],"content":null},{"type":"function_call","call_id":"call_r","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_r","output":"ok"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]`

	cases := []struct {
		name         string
		account      *Account
		apiKeyID     int64 // 0 表示不注入 api_key
		requestPath  string
		body         string
		seeded       map[string]string
		wantChanged  bool
		wantContent  string
		wantLookups  []string
		wantSnapshot map[string]string
	}{
		{
			// 同 itemID、另一个 APIKey：不得读/写 7101 的 scoped 键，只在自己的
			// 键空间内往返（content miss → summary 槽位兜底）。
			name:        "other_api_key_isolated",
			account:     adaptiveNativeDeepSeekResponsesReasoningAccount(27),
			apiKeyID:    9001,
			body:        bodyWith(mediumJSON, summaryOnlyInput),
			seeded:      map[string]string{deepSeekResponsesReasoningScopedKey("content", "r"): "CACHED-FOR-7101"},
			wantChanged: true,
			wantContent: "SUM",
			wantLookups: []string{
				deepSeekResponsesReasoningScopedKeyForAPIKey(9001, "content", "r"),
				deepSeekResponsesReasoningScopedKeyForAPIKey(9001, "summary", "r"),
			},
			wantSnapshot: map[string]string{
				deepSeekResponsesReasoningScopedKey("content", "r"):                "CACHED-FOR-7101",
				deepSeekResponsesReasoningScopedKeyForAPIKey(9001, "summary", "r"): "SUM",
			},
		},
		{
			// 无 APIKey：不读不写缓存，但 summary 明文重编码照常（fail-open）。
			name:        "no_api_key_no_cache_io",
			account:     adaptiveNativeDeepSeekResponsesReasoningAccount(28),
			body:        bodyWith(mediumJSON, summaryOnlyInput),
			seeded:      map[string]string{deepSeekResponsesReasoningScopedKey("content", "r"): "CACHED-FOR-7101"},
			wantChanged: true,
			wantContent: "SUM",
			wantSnapshot: map[string]string{
				deepSeekResponsesReasoningScopedKey("content", "r"): "CACHED-FOR-7101",
			},
		},
		{
			// effort=none（reasoning.effort）：必须在 none 过滤前读到的信号处跳过。
			name:         "effort_none_skips",
			account:      adaptiveNativeDeepSeekResponsesReasoningAccount(30),
			apiKeyID:     nativeDeepSeekReasoningTestAPIKeyID,
			body:         bodyWith(`"reasoning":{"effort":"none"}`, summaryOnlyInput),
			wantSnapshot: map[string]string{},
		},
		{
			// effort=none（顶层 reasoning_effort）：同一信号另一种字段形态。
			name:         "reasoning_effort_none_skips",
			account:      adaptiveNativeDeepSeekResponsesReasoningAccount(31),
			apiKeyID:     nativeDeepSeekReasoningTestAPIKeyID,
			body:         bodyWith(`"reasoning_effort":"none"`, summaryOnlyInput),
			wantSnapshot: map[string]string{},
		},
		{
			// compact 路径：不做 reasoning 往返。
			name:         "compact_path_skips",
			account:      adaptiveNativeDeepSeekResponsesReasoningAccount(32),
			apiKeyID:     nativeDeepSeekReasoningTestAPIKeyID,
			requestPath:  "/v1/responses/compact",
			body:         bodyWith(mediumJSON, summaryOnlyInput),
			wantSnapshot: map[string]string{},
		},
		{
			// 非 DeepSeek 平台（Chat fallback 账号）：不处理。
			name:         "non_deepseek_account_skips",
			account:      openaiPlatformDeepSeekAccount(),
			apiKeyID:     nativeDeepSeekReasoningTestAPIKeyID,
			body:         bodyWith(mediumJSON, summaryOnlyInput),
			wantSnapshot: map[string]string{},
		},
		{
			// DeepSeek 平台但 Chat 协议账号：不属于原生 Responses scope。
			name: "deepseek_chat_protocol_skips",
			account: &Account{
				ID:       33,
				Platform: PlatformDeepseek,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key":      "sk-test",
					"api_protocol": APIProtocolChatCompletions,
				},
			},
			apiKeyID:     nativeDeepSeekReasoningTestAPIKeyID,
			body:         bodyWith(mediumJSON, summaryOnlyInput),
			wantSnapshot: map[string]string{},
		},
		{
			// 新 user 轮边界：不得把上一段明文继承给边界后的工具调用。
			name:         "new_user_turn_clears_chain",
			account:      adaptiveNativeDeepSeekResponsesReasoningAccount(34),
			apiKeyID:     nativeDeepSeekReasoningTestAPIKeyID,
			body:         bodyWith(mediumJSON, `[{"type":"reasoning","id":"r_chain","content":[{"type":"reasoning_text","text":"raw"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]},{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_b","output":"b"}]`),
			wantSnapshot: map[string]string{},
		},
		{
			// encrypted cache-miss 清除早先明文：r_known 已给出 raw content，
			// 随后 r_enc encrypted-only 且缓存未命中——后续 call_b 不得继承
			// r_known 的明文（误继承会让本用例 changed=true 而真失败）；网关也
			// 不编造占位，缺料由上游 400 暴露给客户端。
			name:     "encrypted_cache_miss_clears_chain",
			account:  adaptiveNativeDeepSeekResponsesReasoningAccount(35),
			apiKeyID: nativeDeepSeekReasoningTestAPIKeyID,
			body:     bodyWith(mediumJSON, `[{"type":"reasoning","id":"r_known","content":[{"type":"reasoning_text","text":"raw"}]},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":"a"},{"type":"reasoning","id":"r_enc","summary":[],"encrypted_content":"opaque"},{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_b","output":"b"}]`),
			wantLookups: []string{
				deepSeekResponsesReasoningScopedKey("content", "r_enc"),
				deepSeekResponsesReasoningScopedKey("summary", "r_enc"),
			},
			wantSnapshot: map[string]string{},
		},
		{
			// parallel 调用共享同一段推理：不得多插一段 reasoning。
			name:         "parallel_calls_no_extra_reasoning",
			account:      adaptiveNativeDeepSeekResponsesReasoningAccount(36),
			apiKeyID:     nativeDeepSeekReasoningTestAPIKeyID,
			body:         bodyWith(mediumJSON, `[{"type":"reasoning","id":"r_par","content":[{"type":"reasoning_text","text":"raw"}]},{"type":"function_call","call_id":"call_a","name":"exec","arguments":"{}"},{"type":"function_call","call_id":"call_b","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":"a"},{"type":"function_call_output","call_id":"call_b","output":"b"}]`),
			wantSnapshot: map[string]string{},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cache := &reasoningSharedCache{}
			for key, value := range tc.seeded {
				cache.seed(key, value)
			}
			c := newDeepSeekChatFallbackContext(t, []byte(tc.body))
			if tc.requestPath != "" {
				c.Request.URL.Path = tc.requestPath
			}
			if tc.apiKeyID > 0 {
				c.Set("api_key", &APIKey{ID: tc.apiKeyID})
			}
			svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), cache: cache}

			got, changed := svc.restoreDeepSeekNativeResponsesReasoningText(c, tc.account, []byte(tc.body))
			require.Equal(t, tc.wantChanged, changed)
			require.Equal(t, tc.wantLookups, cache.lookups())
			require.Equal(t, tc.wantSnapshot, cache.snapshot())
			if tc.wantChanged {
				require.Equal(t, tc.wantContent, gjson.GetBytes(got, `input.#(type=="reasoning").content.0.text`).String())
			} else {
				require.Equal(t, tc.body, string(got), "跳过/无变化时必须原样返回报文")
			}
		})
	}
}

// collector 片段兜底表：content 槽位为空且该 item 收到过完整 item.done 时，
// content_index 从 0 连续的 reasoning_text.done 片段可拼合提升为完整明文；
// 任何 index 缺口都不算完整，不得提升。
func TestDeepSeekNativeReasoningCachePartsPromotionTable(t *testing.T) {
	contentKey := func(itemID string) string {
		return deepSeekResponsesReasoningScopedKey("content", itemID)
	}

	cases := []struct {
		name        string
		events      [][2]string // {eventType, data}
		wantContent string      // 空串表示不得写入 content 槽位
	}{
		{
			name: "contiguous_parts_promote_when_slot_empty",
			events: [][2]string{
				{"response.reasoning_text.done", `{"type":"response.reasoning_text.done","item_id":"r","content_index":0,"text":"A"}`},
				{"response.reasoning_text.done", `{"type":"response.reasoning_text.done","item_id":"r","content_index":1,"text":"B"}`},
				{"response.output_item.done", `{"type":"response.output_item.done","item":{"type":"reasoning","id":"r","content":[]}}`},
			},
			wantContent: "AB",
		},
		{
			name: "index_gap_does_not_promote",
			events: [][2]string{
				{"response.reasoning_text.done", `{"type":"response.reasoning_text.done","item_id":"r","content_index":0,"text":"A"}`},
				{"response.reasoning_text.done", `{"type":"response.reasoning_text.done","item_id":"r","content_index":2,"text":"C"}`},
				{"response.output_item.done", `{"type":"response.output_item.done","item":{"type":"reasoning","id":"r","content":[]}}`},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cache := &reasoningSharedCache{}
			svc := &OpenAIGatewayService{cache: cache}
			collector := newDeepSeekNativeReasoningCacheCollector(nativeDeepSeekReasoningTestAPIKeyID)
			for _, event := range tc.events {
				collector.collectStreamEvent([]byte(event[1]), event[0])
			}
			svc.flushDeepSeekNativeResponsesReasoningCache(collector)

			snapshot := cache.snapshot()
			if tc.wantContent == "" {
				require.NotContains(t, snapshot, contentKey("r"), "index 缺口不得提升为完整 content")
				return
			}
			require.Equal(t, tc.wantContent, snapshot[contentKey("r")])
		})
	}
}
