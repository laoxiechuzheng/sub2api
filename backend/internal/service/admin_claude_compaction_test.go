package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdminPreviewCompositeClaudeCompactionHint(t *testing.T) {
	route := CompositeModelRoute{ID: 9, GroupID: 7, PublicModel: "claude-opus-5", MatchType: "exact", TargetPlatform: PlatformOpenAI, Endpoint: "messages", RequestKind: "compaction", Enabled: true}
	svc := &adminServiceImpl{groupRepo: &groupRepoStubForAdmin{getByID: &Group{ID: 7, Platform: PlatformComposite}}, compositeRouteRepo: &compositeRouteRepoStubForAdmin{routes: []CompositeModelRoute{route}}}
	var input CompositeRoutePreviewRequest
	require.NoError(t, json.Unmarshal([]byte(`{"model":"claude-opus-5","endpoint":"messages","user_agent":"claude-cli/2.1.286","body":"{\"messages\":[{\"role\":\"user\",\"content\":\"work\"}]}","claude_compaction_hint":"manual"}`), &input))
	d, err := svc.PreviewCompositeRoute(context.Background(), 7, input)
	require.NoError(t, err)
	require.NotNil(t, d.Route)
	require.Equal(t, "claude_request_header", d.RequestClassification.Source)
}
