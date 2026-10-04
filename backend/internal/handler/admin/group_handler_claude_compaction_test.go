package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGroupHandlerPreviewCompositeClaudeCompactionHint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, hint := range []string{"auto", "manual", "reactive", "compaction", "invalid"} {
		t.Run(hint, func(t *testing.T) {
			svc := &preciseCompositePreviewAdminStub{}
			handler := NewGroupHandler(svc, nil, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Params = gin.Params{{Key: "id", Value: "7"}}
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/groups/7/composite-routes/preview", strings.NewReader(`{"model":"claude-opus-5","endpoint":"messages","user_agent":"claude-cli/2.1.286","body":"{\"messages\":[]}","claude_compaction_hint":"`+hint+`"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.PreviewCompositeRoute(c)
			if hint == "invalid" {
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				return
			}
			require.Equal(t, http.StatusOK, recorder.Code)
			encoded, err := json.Marshal(svc.request)
			require.NoError(t, err)
			var input map[string]any
			require.NoError(t, json.Unmarshal(encoded, &input))
			require.Equal(t, hint, input["claude_compaction_hint"])
		})
	}
}
