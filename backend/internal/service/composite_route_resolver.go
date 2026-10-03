package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type CompositeRouteResolver struct {
	repo                   CompositeModelRouteRepository
	modelOwnershipResolver CompositeModelOwnershipResolver
}

func NewCompositeRouteResolver(repo CompositeModelRouteRepository) *CompositeRouteResolver {
	return &CompositeRouteResolver{repo: repo}
}

func (r *CompositeRouteResolver) SetModelOwnershipResolver(resolver CompositeModelOwnershipResolver) {
	if r != nil {
		r.modelOwnershipResolver = resolver
	}
}

func (r *CompositeRouteResolver) Resolve(ctx context.Context, groupID int64, model, endpoint string) (CompositeRouteDecision, error) {
	return r.ResolveWithMatch(ctx, groupID, model, endpoint, CompositeRouteRequestMatch{})
}

// ResolveWithMatch resolves a route using both the model/endpoint scope and the
// inbound request facts. Routes with request conditions are evaluated as an
// override layer first, so a conditional compaction rule can beat an existing
// exact model route without changing that route's configuration.
func (r *CompositeRouteResolver) ResolveWithMatch(ctx context.Context, groupID int64, model, endpoint string, match CompositeRouteRequestMatch) (decision CompositeRouteDecision, err error) {
	model = strings.TrimSpace(model)
	endpoint = normalizeCompositeRouteEndpoint(endpoint)
	decision = CompositeRouteDecision{
		GroupID:     groupID,
		PublicModel: model,
		Endpoint:    endpoint,
	}
	match.facts = newCompositeRequestFacts(match)
	match.facts.endpoint = endpoint
	var inspectedRoutes []CompositeModelRoute
	if match.Explain {
		defer func() {
			classification := match.facts.classify()
			decision.RequestClassification = &classification
			decision.ConditionEvaluations = explainCompositeRoutes(inspectedRoutes, model, endpoint, match, decision.Route)
		}()
	}
	if model == "" {
		decision.Reason = "model is required"
		return decision, nil
	}

	if r != nil && r.repo != nil && groupID > 0 {
		routes, err := r.repo.ListByGroup(ctx, groupID, false)
		if err != nil {
			return decision, fmt.Errorf("list composite routes: %w", err)
		}
		inspectedRoutes = routes
		if route, ok := matchCompositeRoute(routes, model, endpoint, match); ok {
			upstreamModel := strings.TrimSpace(route.UpstreamModel)
			if upstreamModel == "" {
				upstreamModel = model
			}
			return CompositeRouteDecision{
				Matched:        true,
				Source:         CompositeRouteSourceExplicit,
				GroupID:        groupID,
				PublicModel:    model,
				TargetPlatform: route.TargetPlatform,
				UpstreamModel:  upstreamModel,
				Endpoint:       endpoint,
				Route:          &route,
			}, nil
		}
	}

	if r != nil && r.modelOwnershipResolver != nil && groupID > 0 {
		ownership, err := r.modelOwnershipResolver(ctx, groupID, model)
		if err != nil {
			// A recognizable model can still use the existing detector when the
			// account catalog is temporarily unavailable. Unknown aliases cannot.
			if _, detectable := DetectModelPlatform(model); !detectable {
				return decision, fmt.Errorf("resolve account model ownership: %w", err)
			}
		} else if ownership.Ambiguous {
			decision.Reason = "model is exposed by multiple provider platforms"
			return decision, nil
		} else if ownership.Matched {
			platform := strings.TrimSpace(ownership.TargetPlatform)
			if !isConcreteRequestPlatform(platform) {
				decision.Reason = "account model ownership has no concrete target platform"
				return decision, nil
			}
			return CompositeRouteDecision{
				Matched:        true,
				Source:         CompositeRouteSourceAccount,
				GroupID:        groupID,
				PublicModel:    model,
				TargetPlatform: platform,
				UpstreamModel:  model,
				Endpoint:       endpoint,
			}, nil
		}
	}

	if platform, ok := DetectModelPlatform(model); ok {
		return CompositeRouteDecision{
			Matched:        true,
			Source:         CompositeRouteSourceDetector,
			GroupID:        groupID,
			PublicModel:    model,
			TargetPlatform: platform,
			UpstreamModel:  model,
			Endpoint:       endpoint,
		}, nil
	}
	decision.Reason = "no explicit route or built-in detector match"
	return decision, nil
}

func matchCompositeRoute(routes []CompositeModelRoute, model, endpoint string, match CompositeRouteRequestMatch) (CompositeModelRoute, bool) {
	if len(routes) == 0 {
		return CompositeModelRoute{}, false
	}

	conditional := make([]CompositeModelRoute, 0, len(routes))
	fallback := make([]CompositeModelRoute, 0, len(routes))
	for _, route := range routes {
		if compositeRouteHasRequestConditions(route) {
			conditional = append(conditional, route)
			continue
		}
		fallback = append(fallback, route)
	}
	if route, ok := bestCompositeRoute(conditional, model, endpoint, match); ok {
		return route, true
	}
	return bestCompositeRoute(fallback, model, endpoint, match)
}

func compositeRouteHasRequestConditions(route CompositeModelRoute) bool {
	return strings.TrimSpace(route.UserAgentContains) != "" || strings.TrimSpace(route.BodyContains) != "" ||
		strings.TrimSpace(route.BodyNotContains) != "" || compositeConditionDefault(route.RequestKind, "any") != "any"
}

func bestCompositeRoute(routes []CompositeModelRoute, model, endpoint string, match CompositeRouteRequestMatch) (CompositeModelRoute, bool) {
	if len(routes) == 0 {
		return CompositeModelRoute{}, false
	}

	type candidate struct {
		route          CompositeModelRoute
		matchStrength  int
		endpointWeight int
		prefixLen      int
	}
	candidates := make([]candidate, 0, len(routes))
	for _, route := range routes {
		route.Endpoint = normalizeCompositeRouteEndpoint(route.Endpoint)
		if route.Endpoint != endpoint && route.Endpoint != CompositeRouteEndpointAny {
			continue
		}
		route.MatchType = normalizeCompositeRouteMatchType(route.MatchType)
		publicModel := strings.TrimSpace(route.PublicModel)
		if publicModel == "" {
			continue
		}

		matchStrength := 0
		prefixLen := len(publicModel)
		switch route.MatchType {
		case CompositeRouteMatchExact:
			if publicModel != model {
				continue
			}
			matchStrength = 3
		case CompositeRouteMatchPrefix:
			if !strings.HasPrefix(model, publicModel) {
				continue
			}
			matchStrength = 2
		case CompositeRouteMatchContains:
			if !strings.Contains(model, publicModel) {
				continue
			}
			matchStrength = 1
		default:
			continue
		}
		if !compositeRouteRequestConditionsMatch(route, match) {
			continue
		}
		endpointWeight := 0
		if route.Endpoint == endpoint {
			endpointWeight = 1
		}
		candidates = append(candidates, candidate{
			route:          route,
			matchStrength:  matchStrength,
			endpointWeight: endpointWeight,
			prefixLen:      prefixLen,
		})
	}
	if len(candidates) == 0 {
		return CompositeModelRoute{}, false
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.matchStrength != b.matchStrength {
			return a.matchStrength > b.matchStrength
		}
		if a.endpointWeight != b.endpointWeight {
			return a.endpointWeight > b.endpointWeight
		}
		if a.prefixLen != b.prefixLen {
			return a.prefixLen > b.prefixLen
		}
		if a.route.Priority != b.route.Priority {
			return a.route.Priority < b.route.Priority
		}
		return a.route.ID < b.route.ID
	})
	return candidates[0].route, true
}

func compositeRouteRequestConditionsMatch(route CompositeModelRoute, match CompositeRouteRequestMatch) bool {
	return evaluateCompositeRouteConditions(route, match).Matched
}

func requestBodyContainsPattern(body []byte, pattern string) bool {
	if len(body) == 0 || pattern == "" {
		return false
	}
	if bytes.Contains(body, []byte(pattern)) {
		return true
	}
	// Request bodies are normally JSON, so signatures containing quotes or
	// newlines appear escaped in the raw payload. Accept that representation too.
	encoded, err := json.Marshal(pattern)
	if err != nil || len(encoded) < 2 {
		return false
	}
	escaped := string(encoded[1 : len(encoded)-1])
	return escaped != pattern && bytes.Contains(body, []byte(escaped))
}
