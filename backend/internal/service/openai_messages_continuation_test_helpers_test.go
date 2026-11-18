package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

const openAICompatTestHistoryBoundary = 12

// 仅测试：从合成完整历史的已回显assistant边界构造真实proof，不为产品保留无proof绑定。
func bindCompatProofForNextRequest(t *testing.T, s *OpenAIGatewayService, c *gin.Context, account *Account, key, id string, body []byte, model string) {
	t.Helper()
	var req apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	req.Model = model
	converted, err := apicompat.AnthropicToResponses(&req)
	if err != nil {
		t.Fatal(err)
	}
	observation, items := newContinuationObservation(converted)
	if observation == nil {
		t.Fatal("synthetic proof unavailable")
	}
	end := len(items)
	for end > 0 && ((items[end-1].Type == "message" && items[end-1].Role == "user") || items[end-1].Type == "function_call_output") {
		end--
	}
	start := end
	for start > 0 && (items[start-1].Type == "function_call" || items[start-1].Type == "reasoning" || (items[start-1].Type == "message" && items[start-1].Role == "assistant")) {
		start--
	}
	if start == 0 || end <= start {
		t.Fatal("fixture needs prior user, actual assistant echo, and a new user/tool turn")
	}
	chain, ok := continuationChain(items[:end], [32]byte{})
	if !ok {
		t.Fatal("invalid synthetic history")
	}
	proof := observation.request
	proof.ChainHash = chain
	proof.ReqCount, proof.Count = start, end
	s.bindOpenAICompatSessionResponseProof(context.Background(), c, account, key, id, proof)
}
