package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestAdminService_CompositeRouteRejectsUnknownPreciseConditions(t *testing.T) {
	for _, field := range []string{"request_kind", "body_match_scope", "body_match_mode"} {
		for _, operation := range []string{"create", "update"} {
			t.Run(operation+"/"+field, func(t *testing.T) {
				input := CompositeRouteInput{
					PublicModel:    "gpt",
					TargetPlatform: PlatformOpenAI,
					Enabled:        true,
				}
				payload, err := json.Marshal(map[string]string{field: " unsupported "})
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(payload, &input))
				routeRepo := &compositeRouteRepoStubForAdmin{
					routes: []CompositeModelRoute{{ID: 11, GroupID: 7}},
				}
				svc := &adminServiceImpl{
					groupRepo:          &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}},
					compositeRouteRepo: routeRepo,
				}

				if operation == "create" {
					_, err = svc.CreateCompositeRoute(context.Background(), 7, input)
				} else {
					_, err = svc.UpdateCompositeRoute(context.Background(), 7, 11, input)
				}

				require.ErrorContains(t, err, field)
				require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
				require.Nil(t, routeRepo.created)
				require.Nil(t, routeRepo.updated)
			})
		}
	}
}

func TestAdminService_CompositeRouteRejectsOversizedBodyExclusions(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "ascii", value: strings.Repeat("a", 8193)},
		{name: "utf8_bytes", value: strings.Repeat("\u754c", 2731)},
	}
	for _, tt := range tests {
		for _, operation := range []string{"create", "update"} {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				input := CompositeRouteInput{PublicModel: "gpt", TargetPlatform: PlatformOpenAI, Enabled: true}
				payload, err := json.Marshal(map[string]string{"body_not_contains": tt.value})
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(payload, &input))
				routeRepo := &compositeRouteRepoStubForAdmin{routes: []CompositeModelRoute{{ID: 11, GroupID: 7}}}
				svc := &adminServiceImpl{
					groupRepo:          &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}},
					compositeRouteRepo: routeRepo,
				}

				if operation == "create" {
					_, err = svc.CreateCompositeRoute(context.Background(), 7, input)
				} else {
					_, err = svc.UpdateCompositeRoute(context.Background(), 7, 11, input)
				}

				require.ErrorContains(t, err, "body_not_contains is too long")
				require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
				require.Nil(t, routeRepo.created)
				require.Nil(t, routeRepo.updated)
			})
		}
	}
}

func TestAdminService_CompositeRouteNormalizesAndPersistsPreciseConditions(t *testing.T) {
	tests := []struct {
		name, payload, requestKind, scope, mode, exclusions string
	}{
		{name: "legacy", payload: `{}`, requestKind: "any", scope: "full_body", mode: "any"},
		{name: "blank", payload: `{"request_kind":"  ","body_match_scope":"\t","body_match_mode":"\n"}`, requestKind: "any", scope: "full_body", mode: "any"},
		{name: "normalized", payload: `{"request_kind":" COMPACTION ","body_match_scope":" LAST_MESSAGE ","body_match_mode":" ALL ","body_not_contains":" quoted example \r\nquoted example\r\n\r\n second signature \r third signature "}`, requestKind: "compaction", scope: "last_message", mode: "all", exclusions: "quoted example\nsecond signature\nthird signature"},
		{name: "conversation", payload: `{"request_kind":"conversation","body_match_scope":"current_turn","body_match_mode":"prefix"}`, requestKind: "conversation", scope: "current_turn", mode: "prefix"},
		{name: "instructions", payload: `{"body_match_scope":"instructions"}`, requestKind: "any", scope: "instructions", mode: "any"},
		{name: "maximum_exclusion", payload: `{"body_not_contains":"` + strings.Repeat("a", 8192) + `"}`, requestKind: "any", scope: "full_body", mode: "any", exclusions: strings.Repeat("a", 8192)},
	}
	for _, tt := range tests {
		for _, operation := range []string{"create", "update"} {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				input := CompositeRouteInput{PublicModel: "gpt", TargetPlatform: PlatformOpenAI, Enabled: true}
				require.NoError(t, json.Unmarshal([]byte(tt.payload), &input))
				routeRepo := &compositeRouteRepoStubForAdmin{routes: []CompositeModelRoute{{ID: 11, GroupID: 7}}}
				svc := &adminServiceImpl{
					groupRepo:          &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}},
					compositeRouteRepo: routeRepo,
				}

				var route *CompositeModelRoute
				var err error
				if operation == "create" {
					route, err = svc.CreateCompositeRoute(context.Background(), 7, input)
					require.Equal(t, route, routeRepo.created)
				} else {
					route, err = svc.UpdateCompositeRoute(context.Background(), 7, 11, input)
					require.Equal(t, route, routeRepo.updated)
				}

				require.NoError(t, err)
				require.NotNil(t, route)
				require.Equal(t, tt.requestKind, route.RequestKind)
				require.Equal(t, tt.scope, route.BodyMatchScope)
				require.Equal(t, tt.mode, route.BodyMatchMode)
				require.Equal(t, tt.exclusions, route.BodyNotContains)
			})
		}
	}
}

func TestAdminService_PreviewCompositeRouteUsesNativeCompactionAndExplain(t *testing.T) {
	var route CompositeModelRoute
	require.NoError(t, json.Unmarshal([]byte(`{"id":11,"group_id":7,"public_model":"router/precise","match_type":"exact","target_platform":"openai","endpoint":"responses","request_kind":"compaction","enabled":true}`), &route))
	svc := &adminServiceImpl{
		groupRepo:          &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}},
		compositeRouteRepo: &compositeRouteRepoStubForAdmin{routes: []CompositeModelRoute{route}},
	}
	var input CompositeRoutePreviewRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"router/precise","endpoint":"responses","body":"{\"input\":\"Continue work\"}","native_compaction":true}`), &input))

	decision, err := svc.PreviewCompositeRoute(context.Background(), 7, input)
	require.NoError(t, err)
	require.NotNil(t, decision)
	require.NotNil(t, decision.Route)
	require.Equal(t, int64(11), decision.Route.ID)
	require.NotNil(t, decision.RequestClassification)
	require.NotEmpty(t, decision.ConditionEvaluations)

	input.NativeCompaction = false
	decision, err = svc.PreviewCompositeRoute(context.Background(), 7, input)
	require.NoError(t, err)
	require.Nil(t, decision.Route)
	require.NotNil(t, decision.RequestClassification)
}

func TestAdminService_CompositeRouteLimitsDistinctConditionPatterns(t *testing.T) {
	patterns := make([]string, 129)
	for i := range patterns {
		patterns[i] = fmt.Sprintf("p%03d", i)
	}
	tests := []struct {
		name, value string
		invalid     bool
	}{
		{name: "over_limit", value: strings.Join(patterns, "\n"), invalid: true},
		{name: "at_limit", value: strings.Join(patterns[:128], "\n")},
		{name: "duplicate_lines_do_not_count", value: strings.Join(patterns[:128], "\r\n") + strings.Repeat("\r\n p000 \r\n\r\n", 16)},
	}
	for _, field := range []string{"user_agent_contains", "body_contains", "body_not_contains"} {
		for _, operation := range []string{"create", "update"} {
			for _, tt := range tests {
				t.Run(operation+"/"+field+"/"+tt.name, func(t *testing.T) {
					var decoded CompositeModelRoute
					payload, err := json.Marshal(map[string]string{field: tt.value})
					require.NoError(t, err)
					require.NoError(t, json.Unmarshal(payload, &decoded))
					input := CompositeRouteInput{
						PublicModel:       "gpt",
						TargetPlatform:    PlatformOpenAI,
						UserAgentContains: decoded.UserAgentContains,
						BodyContains:      decoded.BodyContains,
						BodyNotContains:   decoded.BodyNotContains,
						Enabled:           true,
					}
					routeRepo := &compositeRouteRepoStubForAdmin{routes: []CompositeModelRoute{{ID: 11, GroupID: 7}}}
					svc := &adminServiceImpl{
						groupRepo:          &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}},
						compositeRouteRepo: routeRepo,
					}

					var route *CompositeModelRoute
					if operation == "create" {
						route, err = svc.CreateCompositeRoute(context.Background(), 7, input)
					} else {
						route, err = svc.UpdateCompositeRoute(context.Background(), 7, 11, input)
					}

					if tt.invalid {
						require.ErrorContains(t, err, field+" has too many patterns (max 128 distinct patterns)")
						require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
						require.Nil(t, routeRepo.created)
						require.Nil(t, routeRepo.updated)
						return
					}
					require.NoError(t, err)
					require.NotNil(t, route)
					encoded, err := json.Marshal(route)
					require.NoError(t, err)
					var actual map[string]any
					require.NoError(t, json.Unmarshal(encoded, &actual))
					require.Equal(t, strings.Join(patterns[:128], "\n"), actual[field])
				})
			}
		}
	}
}

func TestAdminService_PreviewCompositeRouteRejectsNativeCompactionOutsideResponses(t *testing.T) {
	for _, endpoint := range []string{"", "any", "messages", "count_tokens", "chat_completions", "embeddings", "images", "gemini"} {
		t.Run(endpoint, func(t *testing.T) {
			svc := &adminServiceImpl{
				groupRepo:          &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}},
				compositeRouteRepo: &compositeRouteRepoStubForAdmin{},
			}
			input := CompositeRoutePreviewRequest{Model: "router/precise", Endpoint: endpoint, NativeCompaction: true}

			decision, err := svc.PreviewCompositeRoute(context.Background(), 7, input)

			require.Nil(t, decision)
			require.ErrorContains(t, err, "native_compaction requires endpoint responses")
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
		})
	}
}

func TestAdminService_UpdateCompositeRouteLegacyPUTPreservesPreciseConditions(t *testing.T) {
	existing := CompositeModelRoute{
		ID:              11,
		GroupID:         7,
		PublicModel:     "gpt",
		TargetPlatform:  PlatformOpenAI,
		RequestKind:     "compaction",
		BodyMatchScope:  "last_message",
		BodyMatchMode:   "all",
		BodyNotContains: "quoted example\nnot a compaction",
		Enabled:         true,
		Notes:           "old note",
	}
	routeRepo := &compositeRouteRepoStubForAdmin{routes: []CompositeModelRoute{existing}}
	svc := &adminServiceImpl{
		groupRepo:          &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}},
		compositeRouteRepo: routeRepo,
	}
	input := CompositeRouteInput{PublicModel: "gpt", TargetPlatform: PlatformOpenAI, Priority: 17}

	updated, err := svc.UpdateCompositeRoute(context.Background(), 7, 11, input)

	require.NoError(t, err)
	require.Equal(t, "compaction", updated.RequestKind)
	require.Equal(t, "last_message", updated.BodyMatchScope)
	require.Equal(t, "all", updated.BodyMatchMode)
	require.Equal(t, "quoted example\nnot a compaction", updated.BodyNotContains)
	require.Equal(t, updated, routeRepo.updated)
	require.Equal(t, 17, updated.Priority)
	require.False(t, updated.Enabled, "legacy fields must retain their existing replacement semantics")
	require.Empty(t, updated.Notes, "this must not turn legacy PUT fields into an implicit PATCH")
}

func TestAdminService_UpdateCompositeRouteExplicitPreciseConditions(t *testing.T) {
	tests := []struct {
		name                                 string
		input                                CompositeRouteInput
		requestKind, scope, mode, exclusions string
	}{
		{
			name:        "clear_exclusions",
			input:       CompositeRouteInput{BodyNotContainsProvided: true},
			requestKind: "compaction", scope: "last_message", mode: "all",
		},
		{
			name: "reset_to_legacy_defaults",
			input: CompositeRouteInput{
				RequestKind: "any", BodyMatchScope: "full_body", BodyMatchMode: "any",
				BodyNotContainsProvided: true,
			},
			requestKind: "any", scope: "full_body", mode: "any",
		},
		{
			name:        "update_kind_only",
			input:       CompositeRouteInput{RequestKind: "conversation"},
			requestKind: "conversation", scope: "last_message", mode: "all", exclusions: "quoted example",
		},
		{
			name:        "update_scope_only",
			input:       CompositeRouteInput{BodyMatchScope: "instructions"},
			requestKind: "compaction", scope: "instructions", mode: "all", exclusions: "quoted example",
		},
		{
			name:        "update_mode_only",
			input:       CompositeRouteInput{BodyMatchMode: "prefix"},
			requestKind: "compaction", scope: "last_message", mode: "prefix", exclusions: "quoted example",
		},
		{
			name:        "update_exclusions_only",
			input:       CompositeRouteInput{BodyNotContains: " new exclusion \r\nnew exclusion"},
			requestKind: "compaction", scope: "last_message", mode: "all", exclusions: "new exclusion",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routeRepo := &compositeRouteRepoStubForAdmin{routes: []CompositeModelRoute{{
				ID: 11, GroupID: 7, PublicModel: "gpt", TargetPlatform: PlatformOpenAI,
				RequestKind: "compaction", BodyMatchScope: "last_message", BodyMatchMode: "all",
				BodyNotContains: "quoted example", Enabled: true,
			}}}
			svc := &adminServiceImpl{
				groupRepo:          &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}},
				compositeRouteRepo: routeRepo,
			}
			input := tt.input
			input.PublicModel = "gpt"
			input.TargetPlatform = PlatformOpenAI
			input.Enabled = true

			updated, err := svc.UpdateCompositeRoute(context.Background(), 7, 11, input)

			require.NoError(t, err)
			require.NotNil(t, updated)
			require.Equal(t, tt.requestKind, updated.RequestKind)
			require.Equal(t, tt.scope, updated.BodyMatchScope)
			require.Equal(t, tt.mode, updated.BodyMatchMode)
			require.Equal(t, tt.exclusions, updated.BodyNotContains)
			require.Equal(t, updated, routeRepo.updated)
		})
	}
}
