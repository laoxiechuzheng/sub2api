package service

import (
	"context"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	CompositeRouteMatchExact    = "exact"
	CompositeRouteMatchPrefix   = "prefix"
	CompositeRouteMatchContains = "contains"

	CompositeRouteEndpointAny             = "any"
	CompositeRouteEndpointMessages        = "messages"
	CompositeRouteEndpointCountTokens     = "count_tokens"
	CompositeRouteEndpointResponses       = "responses"
	CompositeRouteEndpointChatCompletions = "chat_completions"
	CompositeRouteEndpointEmbeddings      = "embeddings"
	CompositeRouteEndpointImages          = "images"
	CompositeRouteEndpointGemini          = "gemini"

	CompositeRouteSourceExplicit = "route"
	CompositeRouteSourceDetector = "detector"
	CompositeRouteSourceAccount  = "account_model"
)

// CompositeModelOwnership identifies the concrete provider that exposes a
// public model through an account-level exact mapping.
type CompositeModelOwnership struct {
	TargetPlatform string
	Matched        bool
	Ambiguous      bool
}

type CompositeModelOwnershipResolver func(context.Context, int64, string) (CompositeModelOwnership, error)

var (
	ErrCompositeRouteNotFound = infraerrors.NotFound("COMPOSITE_ROUTE_NOT_FOUND", "composite route not found")
	ErrCompositeRouteExists   = infraerrors.Conflict("COMPOSITE_ROUTE_EXISTS", "composite route already exists")
)

// CompositeModelRoute maps one public model identifier in a composite group to
// the concrete provider/model that should handle the request.
type CompositeModelRoute struct {
	ID                int64     `json:"id"`
	GroupID           int64     `json:"group_id"`
	PublicModel       string    `json:"public_model"`
	MatchType         string    `json:"match_type"`
	TargetPlatform    string    `json:"target_platform"`
	UpstreamModel     string    `json:"upstream_model"`
	Endpoint          string    `json:"endpoint"`
	UserAgentContains string    `json:"user_agent_contains"`
	BodyContains      string    `json:"body_contains"`
	Priority          int       `json:"priority"`
	Enabled           bool      `json:"enabled"`
	Notes             string    `json:"notes"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type CompositeRoutePreviewRequest struct {
	Model     string `json:"model"`
	Endpoint  string `json:"endpoint"`
	UserAgent string `json:"user_agent"`
	Body      string `json:"body"`
}

// CompositeRouteRequestMatch carries request-level facts used by conditional
// routes. Empty conditions never match a conditional rule.
type CompositeRouteRequestMatch struct {
	UserAgent string
	Body      []byte
	// IgnoreRequestConditions is used by catalog/model-list resolution, which
	// asks whether a model is routable at all rather than resolving one live
	// request.
	IgnoreRequestConditions bool
}

type CompositeRouteDecision struct {
	Matched        bool                 `json:"matched"`
	Source         string               `json:"source"`
	GroupID        int64                `json:"group_id"`
	PublicModel    string               `json:"public_model"`
	TargetPlatform string               `json:"target_platform"`
	UpstreamModel  string               `json:"upstream_model"`
	Endpoint       string               `json:"endpoint"`
	Route          *CompositeModelRoute `json:"route,omitempty"`
	Reason         string               `json:"reason,omitempty"`
}

type CompositeRouteInput struct {
	PublicModel       string
	MatchType         string
	TargetPlatform    string
	UpstreamModel     string
	Endpoint          string
	UserAgentContains string
	BodyContains      string
	Priority          int
	Enabled           bool
	Notes             string
}

type CompositeModelRouteRepository interface {
	ListByGroup(ctx context.Context, groupID int64, includeDisabled bool) ([]CompositeModelRoute, error)
	Create(ctx context.Context, route *CompositeModelRoute) error
	Update(ctx context.Context, route *CompositeModelRoute) error
	Delete(ctx context.Context, id int64) error
	DeleteByGroup(ctx context.Context, groupID int64) error
}

func normalizeCompositeRouteEndpoint(endpoint string) string {
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))
	if endpoint == "" {
		return CompositeRouteEndpointAny
	}
	switch endpoint {
	case CompositeRouteEndpointMessages,
		CompositeRouteEndpointCountTokens,
		CompositeRouteEndpointResponses,
		CompositeRouteEndpointChatCompletions,
		CompositeRouteEndpointEmbeddings,
		CompositeRouteEndpointImages,
		CompositeRouteEndpointGemini:
		return endpoint
	default:
		return CompositeRouteEndpointAny
	}
}

func normalizeCompositeRouteMatchType(matchType string) string {
	matchType = strings.ToLower(strings.TrimSpace(matchType))
	switch matchType {
	case CompositeRouteMatchPrefix:
		return CompositeRouteMatchPrefix
	case CompositeRouteMatchContains:
		return CompositeRouteMatchContains
	default:
		return CompositeRouteMatchExact
	}
}

func normalizeCompositeRouteInput(input CompositeRouteInput) CompositeRouteInput {
	input.PublicModel = strings.TrimSpace(input.PublicModel)
	input.MatchType = normalizeCompositeRouteMatchType(input.MatchType)
	input.TargetPlatform = strings.TrimSpace(input.TargetPlatform)
	input.UpstreamModel = strings.TrimSpace(input.UpstreamModel)
	input.Endpoint = normalizeCompositeRouteEndpoint(input.Endpoint)
	input.UserAgentContains = normalizeCompositeRouteConditionPatterns(input.UserAgentContains)
	input.BodyContains = normalizeCompositeRouteConditionPatterns(input.BodyContains)
	// 仅对 exact 路由把空 upstream_model 回填成 public_model：exact 命中时请求模型
	// 恒等于 public_model，回填只影响持久化/后台展示，保留原有契约不变。
	// prefix/contains 路由留空则不回填——Resolve 会回退到具体请求模型，从而透传
	// 原始模型（否则 public=deepseek-v4 的模糊路由会把 deepseek-v4-flash /
	// deepseek-v4-pro 都塌缩成固定的 deepseek-v4）。显式填写 upstream_model 时
	// 任何模式都原样固定转发。
	if input.UpstreamModel == "" && input.MatchType == CompositeRouteMatchExact {
		input.UpstreamModel = input.PublicModel
	}
	input.Notes = strings.TrimSpace(input.Notes)
	return input
}

// normalizeCompositeRouteConditionPatterns turns a multi-line admin input into
// a stable newline-separated OR list: blank lines are dropped and duplicates
// are removed in first-seen order.
func normalizeCompositeRouteConditionPatterns(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	out := make([]string, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func splitCompositeRouteConditionPatterns(value string) []string {
	normalized := normalizeCompositeRouteConditionPatterns(value)
	if normalized == "" {
		return nil
	}
	return strings.Split(normalized, "\n")
}
