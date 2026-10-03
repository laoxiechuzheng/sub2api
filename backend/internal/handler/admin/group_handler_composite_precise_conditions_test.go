package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/stretchr/testify/require"
)

func TestCompositeRouteRequestRejectsUnknownPreciseConditions(t *testing.T) {
	for _, field := range []string{"request_kind", "body_match_scope", "body_match_mode"} {
		t.Run(field, func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{
				"public_model":    "gpt",
				"target_platform": "openai",
				field:             "unsupported",
			})
			require.NoError(t, err)
			var req CompositeRouteRequest
			require.NoError(t, json.Unmarshal(payload, &req))

			require.Error(t, binding.Validator.ValidateStruct(&req))
		})
	}
}

func TestCompositeRouteRequestRejectsOversizedBodyExclusions(t *testing.T) {
	payload, err := json.Marshal(map[string]string{
		"public_model":      "gpt",
		"target_platform":   "openai",
		"body_not_contains": strings.Repeat("a", 8193),
	})
	require.NoError(t, err)
	var req CompositeRouteRequest
	require.NoError(t, json.Unmarshal(payload, &req))

	require.Error(t, binding.Validator.ValidateStruct(&req))
}

func TestCompositeRouteRequestForwardsPreciseConditions(t *testing.T) {
	tests := []struct {
		name, kind, scope, mode string
	}{
		{name: "legacy"},
		{name: "any", kind: "any", scope: "full_body", mode: "any"},
		{name: "compaction", kind: "compaction", scope: "last_message", mode: "all"},
		{name: "conversation", kind: "conversation", scope: "current_turn", mode: "prefix"},
		{name: "instructions", kind: "any", scope: "instructions", mode: "any"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := map[string]any{
				"public_model":      "gpt",
				"target_platform":   "openai",
				"body_not_contains": "first exclusion\nsecond exclusion",
				"enabled":           false,
			}
			if tt.name != "legacy" {
				fields["request_kind"] = tt.kind
				fields["body_match_scope"] = tt.scope
				fields["body_match_mode"] = tt.mode
			}
			payload, err := json.Marshal(fields)
			require.NoError(t, err)
			var req CompositeRouteRequest
			require.NoError(t, json.Unmarshal(payload, &req))
			require.NoError(t, binding.Validator.ValidateStruct(&req))

			input := compositeRouteRequestToInput(req, true)
			require.Equal(t, tt.kind, input.RequestKind)
			require.Equal(t, tt.scope, input.BodyMatchScope)
			require.Equal(t, tt.mode, input.BodyMatchMode)
			require.Equal(t, tt.name != "legacy", input.RequestKindProvided)
			require.Equal(t, tt.name != "legacy", input.BodyMatchScopeProvided)
			require.Equal(t, tt.name != "legacy", input.BodyMatchModeProvided)
			require.True(t, input.BodyNotContainsProvided)
			require.Equal(t, "first exclusion\nsecond exclusion", input.BodyNotContains)
			require.False(t, input.Enabled)
		})
	}
}

type preciseCompositeUpdateAdminStub struct {
	service.AdminService
	groupID int64
	routeID int64
	input   service.CompositeRouteInput
	called  bool
}

func (s *preciseCompositeUpdateAdminStub) UpdateCompositeRoute(_ context.Context, groupID, routeID int64, input service.CompositeRouteInput) (*service.CompositeModelRoute, error) {
	s.groupID = groupID
	s.routeID = routeID
	s.input = input
	s.called = true
	return &service.CompositeModelRoute{ID: routeID, GroupID: groupID}, nil
}

func TestGroupHandlerUpdateCompositeRoutePreservesPreciseConditionPresence(t *testing.T) {
	tests := []struct {
		name, conditions   string
		kind, scope, mode  string
		enumsProvided      bool
		exclusionsProvided bool
	}{
		{name: "legacy_put"},
		{name: "null_is_omitted", conditions: `,"request_kind":null,"body_match_scope":null,"body_match_mode":null,"body_not_contains":null`},
		{name: "clear_exclusions", conditions: `,"body_not_contains":""`, exclusionsProvided: true},
		{
			name:       "explicit_legacy_defaults",
			conditions: `,"request_kind":"any","body_match_scope":"full_body","body_match_mode":"any","body_not_contains":""`,
			kind:       "any", scope: "full_body", mode: "any", enumsProvided: true, exclusionsProvided: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			adminService := &preciseCompositeUpdateAdminStub{}
			handler := NewGroupHandler(adminService, nil, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Params = gin.Params{{Key: "id", Value: "7"}, {Key: "route_id", Value: "11"}}
			body := `{"public_model":"gpt","target_platform":"openai","priority":17,"enabled":false` + tt.conditions + `}`
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/groups/7/composite-routes/11", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")

			handler.UpdateCompositeRoute(c)

			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.True(t, adminService.called)
			require.Equal(t, int64(7), adminService.groupID)
			require.Equal(t, int64(11), adminService.routeID)
			require.Equal(t, tt.kind, adminService.input.RequestKind)
			require.Equal(t, tt.scope, adminService.input.BodyMatchScope)
			require.Equal(t, tt.mode, adminService.input.BodyMatchMode)
			require.Equal(t, tt.enumsProvided, adminService.input.RequestKindProvided)
			require.Equal(t, tt.enumsProvided, adminService.input.BodyMatchScopeProvided)
			require.Equal(t, tt.enumsProvided, adminService.input.BodyMatchModeProvided)
			require.Equal(t, tt.exclusionsProvided, adminService.input.BodyNotContainsProvided)
			require.Empty(t, adminService.input.BodyNotContains)
			require.Equal(t, 17, adminService.input.Priority)
			require.False(t, adminService.input.Enabled)
			require.Empty(t, adminService.input.Notes)
		})
	}
}

type preciseCompositePreviewAdminStub struct {
	service.AdminService
	request service.CompositeRoutePreviewRequest
}

func (s *preciseCompositePreviewAdminStub) PreviewCompositeRoute(_ context.Context, _ int64, input service.CompositeRoutePreviewRequest) (*service.CompositeRouteDecision, error) {
	s.request = input
	return &service.CompositeRouteDecision{Matched: true}, nil
}

func TestGroupHandlerPreviewCompositeRouteForwardsNativeCompaction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	adminService := &preciseCompositePreviewAdminStub{}
	handler := NewGroupHandler(adminService, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: "7"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/groups/7/composite-routes/preview", strings.NewReader(`{"model":"gpt","endpoint":"responses","body":"{\"input\":\"work\"}","native_compaction":true}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.PreviewCompositeRoute(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "gpt", adminService.request.Model)
	require.Equal(t, "responses", adminService.request.Endpoint)
	require.Equal(t, `{"input":"work"}`, adminService.request.Body)
	require.True(t, adminService.request.NativeCompaction)
}
