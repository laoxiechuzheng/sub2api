package routes

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

type compositeRouteTarget struct {
	platform string
	model    string
}

func (t compositeRouteTarget) label(routePlatform string) string {
	if t.platform != "" && t.platform != routePlatform {
		return t.platform + "/" + t.model
	}
	return t.model
}

// compositeRouteTargets lists the primary target followed by the route's
// ordered fallbacks; an empty fallback platform inherits the route platform.
func compositeRouteTargets(decision service.CompositeRouteDecision) []compositeRouteTarget {
	targets := []compositeRouteTarget{{platform: decision.TargetPlatform, model: decision.UpstreamModel}}
	if decision.Route == nil {
		return targets
	}
	seen := map[compositeRouteTarget]struct{}{targets[0]: {}}
	for _, fallback := range decision.Route.FallbackModels {
		target := compositeRouteTarget{platform: strings.TrimSpace(fallback.TargetPlatform), model: strings.TrimSpace(fallback.UpstreamModel)}
		if target.model == "" {
			continue
		}
		if target.platform == "" {
			target.platform = decision.TargetPlatform
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	return targets
}

// compositeRewriteModel points the request body at the target model.
func compositeRewriteModel(routePath string, body []byte, publicModel, targetModel string) []byte {
	if targetModel == "" || targetModel == publicModel || !gjson.ValidBytes(body) {
		return body
	}
	if _, modelPath := requestmodel.JSONModelPathForRoute(routePath, body); modelPath != "" {
		if rewritten, err := sjson.SetBytes(body, modelPath, targetModel); err == nil {
			return rewritten
		}
	}
	return body
}

// compositeFallbackRetryable reports whether a failed attempt may be replayed
// on the next target. Only failures another model/platform can plausibly fix
// are replayed: upstream/account-side failures (any status the upstream
// returned, transport errors, in-band upstream stream errors), no schedulable
// account for the model, model/platform-scoped local rejections, and other
// server-side 5xx. Caller-side rejections — user/key concurrency and rate
// limits, balance or quota, content policy, invalid or oversized requests —
// would fail identically on every target and are returned as is. An attempt
// that already handed usage to billing, or whose client went away, is final.
func compositeFallbackRetryable(c *gin.Context, base *http.Request, outcome compositeFallbackOutcome, trace *service.CompositeRouteTrace) bool {
	if !outcome.Failed || outcome.DownstreamFailed || base.Context().Err() != nil || trace.UsageCommitted() || service.GetOpsCyberPolicy(c) != nil {
		return false
	}
	streamErr, hasStreamErr := service.OpsStreamErrorFromContext(c)
	upstreamStatus := c.GetInt(service.OpsUpstreamStatusCodeKey)
	// A generic gateway error must not hide an already known terminal status.
	for _, status := range [3]int{outcome.Status, streamErr.IntendedStatus, upstreamStatus} {
		if status == http.StatusRequestEntityTooLarge || status == 499 {
			return false
		}
	}
	status := outcome.Status
	if hasStreamErr && streamErr.IntendedStatus >= http.StatusBadRequest {
		status = streamErr.IntendedStatus
	} else if outcome.InBand && upstreamStatus >= http.StatusBadRequest {
		status = upstreamStatus
	}
	if service.HasOpsClientBusinessLimited(c) {
		switch service.OpsClientBusinessLimitedReason(c) {
		case service.OpsClientBusinessLimitedReasonLocalModelConfiguration,
			service.OpsClientBusinessLimitedReasonLocalFeatureGate:
			return true
		default:
			return false
		}
	}
	if service.IsOpsRequestScopedError(c) || (hasStreamErr && streamErr.RequestScoped) {
		return false
	}
	if service.HasOpsUpstreamFailure(c) || service.IsOpsRoutingCapacityLimited(c) {
		return true
	}
	if status >= http.StatusInternalServerError {
		return true
	}
	// An SSE error frame the gateway did not author itself came from the upstream stream.
	return outcome.InBand && !hasStreamErr
}

// compositeStreamProtocol picks the terminal error frame format for a stream
// that was already committed when every target failed.
func compositeStreamProtocol(routePath string) compositeSSEProtocol {
	path := strings.ToLower(routePath)
	switch {
	case strings.Contains(path, "/responses"):
		return compositeSSEProtocolResponses
	case strings.Contains(path, "/messages"):
		return compositeSSEProtocolAnthropic
	case strings.Contains(path, "/chat/completions"):
		return compositeSSEProtocolChat
	}
	return compositeSSEProtocolGeneric
}

// runCompositeRouteWithFallback serves a request through an explicit composite
// route. Without fallbacks it is the plain c.Next() path. With fallbacks every
// target gets one full handler run (which keeps the handler's own account
// failover); a failure that has not reached the client is discarded and the
// request is replayed with the next target's platform and model.
//
// Replays invoke only the final route handler (c.Handler()); middleware that
// sits between the composite middleware and the handler runs once, on the
// first attempt. Today that is RequireGroupAssignment (always satisfied for a
// composite group key) and per-route body limits (already enforced on the same
// body), so nothing is skipped in effect; new middleware placed there must be
// replay-safe or moved before the composite middleware.
func runCompositeRouteWithFallback(c *gin.Context, base *http.Request, routePath, publicModel string, originalBody []byte, decision service.CompositeRouteDecision) {
	var routeID int64
	if decision.Route != nil {
		routeID = decision.Route.ID
	}
	targets := compositeRouteTargets(decision)
	// Classify before any handler rewrites the original request path or body.
	needsNativeResponses := service.IsOpenAICompactionRequest(c, originalBody)
	serveTarget := func(target compositeRouteTarget, first bool) {
		if needsNativeResponses && !domain.UsesOpenAIGateway(target.platform) {
			service.MarkOpsClientBusinessLimited(c, service.OpsClientBusinessLimitedReasonLocalModelConfiguration)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
				"type": "compact_not_supported", "message": "No available targets preserve native Responses compaction",
			}})
			return
		}
		if first {
			c.Next()
		} else {
			c.Handler()(c)
		}
	}
	// prepare builds a fresh request per attempt: its own decision, its own
	// usage trace (chain = how this attempt was reached), a private header copy
	// (handlers may edit headers, e.g. Bedrock beta filtering) and the body
	// rewritten from the original bytes.
	prepare := func(target compositeRouteTarget, chain string) *service.CompositeRouteTrace {
		trace := &service.CompositeRouteTrace{RouteID: routeID, RequestKind: decision.RequestKind}
		trace.SetFallbackChain(chain)
		attempt := decision
		attempt.TargetPlatform = target.platform
		attempt.UpstreamModel = target.model
		ctx := service.WithCompositeRouteDecision(base.Context(), attempt)
		ctx = service.WithCompositeRouteTrace(ctx, trace)
		req := base.WithContext(ctx)
		req.Header = base.Header.Clone()
		requestmodel.ResetRequestBody(req, compositeRewriteModel(routePath, originalBody, publicModel, target.model))
		c.Request = req
		return trace
	}
	if len(targets) == 1 {
		prepare(targets[0], "")
		serveTarget(targets[0], true)
		return
	}

	original := c.Writer
	writer := newCompositeFallbackWriter(original, compositeStreamProtocol(routePath))
	c.Writer = writer
	defer func() {
		writer.finish()
		c.Writer = original
	}()
	keys := cloneGinKeys(c.Keys)
	failures := make([]string, 0, len(targets)-1)
	chain := ""
	for index, target := range targets {
		if index > 0 {
			writer.discardAttempt()
			writer.beginAttempt()
			c.Keys = cloneGinKeys(keys)
			c.Errors = c.Errors[:0]
		}
		trace := prepare(target, chain)
		serveTarget(target, index == 0)
		outcome := writer.endAttempt()
		if index == len(targets)-1 || !compositeFallbackRetryable(c, base, outcome, trace) {
			return
		}
		failures = append(failures, fmt.Sprintf("%s (%d)", target.label(decision.TargetPlatform), outcome.Status))
		next := targets[index+1]
		chain = strings.Join(failures, " → ") + " → " + next.label(decision.TargetPlatform)
		logger.L().With(zap.String("component", "routes.composite_fallback")).Info("composite.route_fallback",
			zap.Int64("route_id", routeID),
			zap.String("public_model", publicModel),
			zap.String("failed_platform", target.platform),
			zap.String("failed_model", target.model),
			zap.Int("failed_status", outcome.Status),
			zap.String("next_platform", next.platform),
			zap.String("next_model", next.model),
		)
	}
}

func cloneGinKeys(keys map[string]any) map[string]any {
	if keys == nil {
		return nil
	}
	cloned := make(map[string]any, len(keys))
	for key, value := range keys {
		cloned[key] = value
	}
	return cloned
}
