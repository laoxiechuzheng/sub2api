package service

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// WithResolvedTargetPlatform stores the concrete provider chosen for a request
// made through a composite group.
func WithResolvedTargetPlatform(ctx context.Context, platform string) context.Context {
	platform = strings.TrimSpace(platform)
	if ctx == nil || platform == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxkey.ResolvedTargetPlatform, platform)
}

// ResolvedTargetPlatformFromContext returns the concrete provider chosen for
// the current request, if one was resolved.
func ResolvedTargetPlatformFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	platform, ok := ctx.Value(ctxkey.ResolvedTargetPlatform).(string)
	platform = strings.TrimSpace(platform)
	if !ok || platform == "" {
		return "", false
	}
	return platform, true
}

func WithCompositeRouteDecision(ctx context.Context, decision CompositeRouteDecision) context.Context {
	if ctx == nil || !decision.Matched {
		return ctx
	}
	ctx = WithResolvedTargetPlatform(ctx, decision.TargetPlatform)
	if model := strings.TrimSpace(decision.UpstreamModel); model != "" {
		ctx = context.WithValue(ctx, ctxkey.ResolvedUpstreamModel, model)
	}
	if model := strings.TrimSpace(decision.PublicModel); model != "" {
		ctx = context.WithValue(ctx, ctxkey.RequestedPublicModel, model)
	}
	if source := strings.TrimSpace(decision.Source); source != "" {
		ctx = context.WithValue(ctx, ctxkey.CompositeRouteSource, source)
	}
	return ctx
}

type compositeAccountTargetKey struct{}
type compositeAccountRPMKey struct{}

type compositeAccountRPMAdmission struct {
	groupID  int64
	admitted atomic.Bool
}

// WithCompositeAccountFallbackState shares successful RPM admission across
// automatic attempts in the request's original group.
func WithCompositeAccountFallbackState(ctx context.Context, groupID int64) context.Context {
	return context.WithValue(ctx, compositeAccountRPMKey{}, &compositeAccountRPMAdmission{groupID: groupID})
}

// WithCompositeAccountTarget limits one HTTP fallback attempt to the given
// provider and panel priority, including sticky and load-aware selection.
func WithCompositeAccountTarget(ctx context.Context, target CompositeAccountTarget) context.Context {
	return context.WithValue(ctx, compositeAccountTargetKey{}, target)
}

func compositeAccountModelEligible(ctx context.Context, account *Account) bool {
	if ctx == nil {
		return true
	}
	if target, ok := ctx.Value(compositeAccountTargetKey{}).(CompositeAccountTarget); ok {
		// The account roster and scheduler snapshot may refresh separately
		// after a panel priority edit. Keep the chosen IDs eligible while
		// retaining the ordering captured at the start of this request.
		if _, inTier := slices.BinarySearch(target.accountIDs, account.ID); account.Platform != target.Platform || !inTier {
			return false
		}
	}
	if source, ok := CompositeRouteSourceFromContext(ctx); ok && source == CompositeRouteSourceAccount {
		if model, modelOK := RequestedPublicModelFromContext(ctx); modelOK {
			return compositeAccountClaimsModel(account, model)
		}
	}
	return true
}

// RewriteCompositeRequestModel puts a composite route's upstream model into the
// request body at modelPath. The client chose its thinking mode for the model
// it asked for, so a mode the upstream model rejects becomes the nearest mode
// it accepts. The body is returned unchanged when it cannot be rewritten.
func RewriteCompositeRequestModel(body []byte, modelPath, upstreamModel string) []byte {
	rewritten, err := sjson.SetBytes(body, modelPath, upstreamModel)
	if err != nil {
		return body
	}
	return adaptThinkingToClaude55(rewritten, upstreamModel)
}

func ResolvedUpstreamModelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	model, ok := ctx.Value(ctxkey.ResolvedUpstreamModel).(string)
	model = strings.TrimSpace(model)
	if !ok || model == "" {
		return "", false
	}
	return model, true
}

func RequestedPublicModelFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	model, ok := ctx.Value(ctxkey.RequestedPublicModel).(string)
	model = strings.TrimSpace(model)
	if !ok || model == "" {
		return "", false
	}
	return model, true
}

// compositeClientModel returns the public model the client asked for when a
// composite route sent a different model upstream, and fallback otherwise. The
// response reports that public name whatever model the upstream says served it.
func compositeClientModel(ctx context.Context, fallback string) string {
	publicModel, ok := RequestedPublicModelFromContext(ctx)
	if !ok {
		return fallback
	}
	if upstreamModel, ok := ResolvedUpstreamModelFromContext(ctx); !ok || upstreamModel == publicModel {
		return fallback
	}
	return publicModel
}

// restoreCompositeClientModel reports the client's public model in an Anthropic
// response body when a composite route rewrote the model.
func restoreCompositeClientModel(ctx context.Context, body []byte) []byte {
	clientModel := compositeClientModel(ctx, "")
	if clientModel == "" || !gjson.GetBytes(body, "model").Exists() {
		return body
	}
	if rewritten, err := sjson.SetBytes(body, "model", clientModel); err == nil {
		return rewritten
	}
	return body
}

func CompositeRouteSourceFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	source, ok := ctx.Value(ctxkey.CompositeRouteSource).(string)
	source = strings.TrimSpace(source)
	if !ok || source == "" {
		return "", false
	}
	return source, true
}

// CompositeRouteTrace attributes one attempt of a request served through an
// explicit composite route. The fallback runner gives every attempt its own
// trace (with the chain that led to it) in the request context, so the usage
// writer stamps each row with the attempt that produced it, and the runner can
// tell whether a failed attempt already handed usage to billing.
type CompositeRouteTrace struct {
	RouteID         int64
	RequestKind     string
	AccountFallback bool

	mu             sync.Mutex
	fallbackChain  string
	usageSubmitted atomic.Bool
	usageRecorded  atomic.Bool
}

type compositeRouteTraceKey struct{}

func WithCompositeRouteTrace(ctx context.Context, trace *CompositeRouteTrace) context.Context {
	if ctx == nil || trace == nil || (trace.RouteID <= 0 && !trace.AccountFallback) {
		return ctx
	}
	return context.WithValue(ctx, compositeRouteTraceKey{}, trace)
}

func CompositeRouteTraceFromContext(ctx context.Context) *CompositeRouteTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(compositeRouteTraceKey{}).(*CompositeRouteTrace)
	return trace
}

// SetFallbackChain records the failed targets and the target now serving,
// e.g. "model-a (429) → model-b".
func (t *CompositeRouteTrace) SetFallbackChain(chain string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.fallbackChain = chain
	t.mu.Unlock()
}

func (t *CompositeRouteTrace) FallbackChain() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fallbackChain
}

func (t *CompositeRouteTrace) UsageRecorded() bool {
	return t != nil && t.usageRecorded.Load()
}

// MarkUsageSubmitted is called synchronously on the request goroutine when a
// usage-record task is handed to billing (before the async worker runs it).
func (t *CompositeRouteTrace) MarkUsageSubmitted() {
	if t != nil {
		t.usageSubmitted.Store(true)
	}
}

// UsageCommitted reports whether this attempt submitted or persisted usage. A
// failed attempt that did must not be replayed: the replay shares the request
// id, so billing would conflict and could drop the successful attempt's bill.
func (t *CompositeRouteTrace) UsageCommitted() bool {
	return t != nil && (t.usageSubmitted.Load() || t.usageRecorded.Load())
}

// applyCompositeRouteTrace stamps route attribution onto a usage row about to
// be persisted. Rows outside an explicit route keep NULL columns.
func applyCompositeRouteTrace(ctx context.Context, usage *UsageLog) {
	trace := CompositeRouteTraceFromContext(ctx)
	if trace == nil || usage == nil {
		return
	}
	trace.usageRecorded.Store(true)
	if routeID := trace.RouteID; routeID > 0 {
		usage.CompositeRouteID = &routeID
	}
	if kind := strings.TrimSpace(trace.RequestKind); kind != "" {
		usage.CompositeRouteKind = &kind
	}
	if chain := trace.FallbackChain(); chain != "" {
		usage.CompositeFallbackChain = &chain
	}
}

// DetectModelPlatform maps common public model IDs to the concrete provider
// platform used by sub2api. It intentionally returns false for ambiguous model
// names so composite groups fail closed instead of guessing.
func DetectModelPlatform(model string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return "", false
	}

	normalized = strings.TrimPrefix(normalized, "models/")
	if slash := strings.IndexByte(normalized, '/'); slash > 0 {
		provider := strings.TrimSpace(normalized[:slash])
		rest := strings.TrimSpace(normalized[slash+1:])
		switch provider {
		case "anthropic", "claude":
			return PlatformAnthropic, true
		case "openai", "chatgpt":
			return PlatformOpenAI, true
		case "google", "google-ai-studio", "gemini":
			return PlatformGemini, true
		case "xai", "x-ai", "grok":
			return PlatformGrok, true
		case "kimi", "moonshot":
			return PlatformKimi, true
		case "zhipu", "glm", "bigmodel":
			return PlatformZhipu, true
		case "deepseek":
			return PlatformDeepseek, true
		case "minimax":
			return PlatformMiniMax, true
		case "typesafe", "jev":
			return PlatformTypeSafe, true
		}
		if rest != "" {
			normalized = strings.TrimPrefix(rest, "models/")
		}
	}

	switch {
	case strings.HasPrefix(normalized, "anthropic.claude-"),
		strings.HasPrefix(normalized, "claude-"):
		return PlatformAnthropic, true
	case strings.HasPrefix(normalized, "gpt-"),
		strings.HasPrefix(normalized, "chatgpt-"),
		strings.HasPrefix(normalized, "codex-"),
		strings.HasPrefix(normalized, "text-embedding-"),
		strings.HasPrefix(normalized, "text-moderation-"),
		strings.HasPrefix(normalized, "omni-moderation-"),
		strings.HasPrefix(normalized, "dall-e-"),
		strings.HasPrefix(normalized, "gpt-image-"),
		strings.HasPrefix(normalized, "tts-"),
		strings.HasPrefix(normalized, "whisper-"),
		hasOpenAISeriesPrefix(normalized):
		return PlatformOpenAI, true
	case strings.HasPrefix(normalized, "gemini-"),
		strings.HasPrefix(normalized, "learnlm-"):
		return PlatformGemini, true
	case normalized == "grok" || strings.HasPrefix(normalized, "grok-"):
		return PlatformGrok, true
	case normalized == "k3",
		normalized == "k3-256k",
		strings.HasPrefix(normalized, "kimi-"),
		strings.HasPrefix(normalized, "moonshot-"):
		return PlatformKimi, true
	case strings.HasPrefix(normalized, "glm-"):
		return PlatformZhipu, true
	case strings.HasPrefix(normalized, "deepseek-"):
		return PlatformDeepseek, true
	case strings.HasPrefix(normalized, "minimax-"),
		strings.HasPrefix(normalized, "abab5"),
		strings.HasPrefix(normalized, "abab6"),
		strings.HasPrefix(normalized, "abab7"):
		return PlatformMiniMax, true
	case normalized == "jev-latest" || strings.HasPrefix(normalized, "jev-"):
		return PlatformTypeSafe, true
	default:
		return "", false
	}
}

func hasOpenAISeriesPrefix(model string) bool {
	for _, prefix := range []string{"o1", "o3", "o4", "o5"} {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}

func (s *GatewayService) resolveCompositeRouteDecision(ctx context.Context, group *Group, requestedModel, endpoint string) (CompositeRouteDecision, bool, error) {
	if group == nil || group.Platform != PlatformComposite {
		return CompositeRouteDecision{}, false, nil
	}
	if platform, ok := ResolvedTargetPlatformFromContext(ctx); ok {
		upstreamModel := requestedModel
		if resolvedModel, modelOK := ResolvedUpstreamModelFromContext(ctx); modelOK {
			upstreamModel = resolvedModel
		}
		source := CompositeRouteSourceDetector
		if resolvedSource, sourceOK := CompositeRouteSourceFromContext(ctx); sourceOK {
			source = resolvedSource
		}
		return CompositeRouteDecision{
			Matched:        true,
			Source:         source,
			GroupID:        group.ID,
			PublicModel:    requestedModel,
			TargetPlatform: platform,
			UpstreamModel:  upstreamModel,
			Endpoint:       normalizeCompositeRouteEndpoint(endpoint),
		}, true, nil
	}
	decision, err := s.compositeResolver.Resolve(ctx, group.ID, requestedModel, endpoint)
	if err != nil {
		return decision, false, err
	}
	return decision, decision.Matched, nil
}

// isConcreteRequestPlatform 判定平台是否可作为 composite 的具体请求目标。
// 与迁移 227 + 229 重建的 composite_model_routes_target_platform_check 终态一致。
//
// Kiro is selected from account model mappings or explicit routes. Its model
// names overlap with other providers, so name detection cannot select it.
func isConcreteRequestPlatform(platform string) bool {
	return domain.IsConcretePlatform(platform)
}
