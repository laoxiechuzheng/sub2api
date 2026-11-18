package service

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 只串行现有有界cache上的短RMW，不在锁内做I/O或递归调用。
var openAICompatSessionBindingMu sync.Mutex

type openAICompatSessionResponseBinding struct {
	ResponseID           string
	TurnState            string
	ContinuationDisabled bool
	Proof                compatContinuationProof
	ExpiresAt            time.Time
}

func (b openAICompatSessionResponseBinding) sessionExpiresAt() time.Time { return b.ExpiresAt }
func (b openAICompatSessionResponseBinding) sessionCost() int {
	return len(b.ResponseID) + len(b.TurnState) + 80
}
func (b openAICompatSessionResponseBinding) cloneSessionValue() sessionMemoryValue {
	b.ResponseID = strings.Clone(b.ResponseID)
	b.TurnState = strings.Clone(b.TurnState)
	return b
}

func openAICompatContinuationEnabled(account *Account, model string) bool {
	return account != nil && account.Type == AccountTypeAPIKey && account.Platform != PlatformGrok && shouldAutoInjectPromptCacheKeyForCompat(model)
}

func isOpenAICompatPreviousResponseNotFound(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != 400 && statusCode != 404 {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		return strings.Contains(lower, "previous_response_not_found") || lower == "previous_response_id is not available for this user" || (strings.Contains(lower, "previous response") && strings.Contains(lower, "not found")) || (strings.Contains(lower, "unsupported parameter") && strings.Contains(lower, "previous_response_id"))
	}
	return check(upstreamMsg) || check(string(upstreamBody)) || check(gjson.GetBytes(upstreamBody, "error.code").String()) || check(gjson.GetBytes(upstreamBody, "error.message").String())
}
func isOpenAICompatPreviousResponseUnsupported(statusCode int, upstreamMsg string, upstreamBody []byte) bool {
	if statusCode != 400 {
		return false
	}
	check := func(s string) bool {
		lower := strings.ToLower(strings.TrimSpace(s))
		return strings.Contains(lower, "previous_response_id") && (strings.Contains(lower, "unsupported parameter") || strings.Contains(lower, "only supported on responses websocket") || strings.Contains(lower, "not supported") || strings.Contains(lower, "is not available for this user") || strings.Contains(lower, "requires an openai api-key account for http requests"))
	}
	return check(upstreamMsg) || check(string(upstreamBody)) || check(gjson.GetBytes(upstreamBody, "error.code").String()) || check(gjson.GetBytes(upstreamBody, "error.message").String())
}
func openAICompatSessionResponseKey(c *gin.Context, account *Account, promptCacheKey string) string {
	key := strings.TrimSpace(promptCacheKey)
	if account == nil || key == "" {
		return ""
	}
	return strings.Join([]string{strconv.FormatInt(account.ID, 10), strconv.FormatInt(getAPIKeyIDFromContext(c), 10), key}, "\x00")
}

func (s *OpenAIGatewayService) loadOpenAICompatSessionBinding(c *gin.Context, account *Account, key string) openAICompatSessionResponseBinding {
	if s == nil {
		return openAICompatSessionResponseBinding{}
	}
	cacheKey := openAICompatSessionResponseKey(c, account, key)
	if cacheKey == "" {
		return openAICompatSessionResponseBinding{}
	}
	openAICompatSessionBindingMu.Lock()
	defer openAICompatSessionBindingMu.Unlock()
	raw, ok := s.openaiCompatSessionResponses.Load(cacheKey)
	if !ok {
		return openAICompatSessionResponseBinding{}
	}
	binding, ok := raw.(openAICompatSessionResponseBinding)
	if !ok || (!binding.ExpiresAt.IsZero() && time.Now().After(binding.ExpiresAt)) {
		s.openaiCompatSessionResponses.Delete(cacheKey)
		return openAICompatSessionResponseBinding{}
	}
	return binding
}
func (s *OpenAIGatewayService) bindOpenAICompatSessionResponseProof(_ context.Context, c *gin.Context, account *Account, key, id string, proof compatContinuationProof) {
	if s == nil || strings.TrimSpace(id) == "" || proof.ReqCount <= 0 || proof.Count <= proof.ReqCount {
		return
	}
	cacheKey := openAICompatSessionResponseKey(c, account, key)
	if cacheKey == "" {
		return
	}
	openAICompatSessionBindingMu.Lock()
	defer openAICompatSessionBindingMu.Unlock()
	binding := openAICompatSessionResponseBinding{ResponseID: strings.TrimSpace(id), Proof: proof, ExpiresAt: time.Now().Add(s.openAIWSResponseStickyTTL())}
	if raw, ok := s.openaiCompatSessionResponses.Load(cacheKey); ok {
		if existing, ok := raw.(openAICompatSessionResponseBinding); ok {
			binding.TurnState = existing.TurnState
			if existing.ContinuationDisabled {
				existing.ExpiresAt = binding.ExpiresAt
				s.openaiCompatSessionResponses.Store(cacheKey, existing)
				return
			}
		}
	}
	s.openaiCompatSessionResponses.Store(cacheKey, binding)
}
func (s *OpenAIGatewayService) deleteOpenAICompatSessionResponseIDIf(_ context.Context, c *gin.Context, account *Account, key string, used openAICompatSessionResponseBinding) {
	if s == nil || used.ResponseID == "" {
		return
	}
	cacheKey := openAICompatSessionResponseKey(c, account, key)
	if cacheKey == "" {
		return
	}
	openAICompatSessionBindingMu.Lock()
	defer openAICompatSessionBindingMu.Unlock()
	raw, ok := s.openaiCompatSessionResponses.Load(cacheKey)
	if !ok {
		return
	}
	binding, ok := raw.(openAICompatSessionResponseBinding)
	if !ok {
		return
	}
	if binding.ResponseID != used.ResponseID || binding.Proof != used.Proof {
		return
	}
	binding.ResponseID = ""
	binding.Proof = compatContinuationProof{}
	if binding.TurnState == "" && !binding.ContinuationDisabled {
		s.openaiCompatSessionResponses.Delete(cacheKey)
		return
	}
	binding.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
	s.openaiCompatSessionResponses.Store(cacheKey, binding)
}
func (s *OpenAIGatewayService) disableOpenAICompatSessionContinuation(_ context.Context, c *gin.Context, account *Account, key string) {
	if s == nil {
		return
	}
	cacheKey := openAICompatSessionResponseKey(c, account, key)
	if cacheKey == "" {
		return
	}
	openAICompatSessionBindingMu.Lock()
	defer openAICompatSessionBindingMu.Unlock()
	binding := openAICompatSessionResponseBinding{ContinuationDisabled: true, ExpiresAt: time.Now().Add(s.openAIWSResponseStickyTTL())}
	if raw, ok := s.openaiCompatSessionResponses.Load(cacheKey); ok {
		if existing, ok := raw.(openAICompatSessionResponseBinding); ok {
			binding.TurnState = existing.TurnState
		}
	}
	s.openaiCompatSessionResponses.Store(cacheKey, binding)
}
func (s *OpenAIGatewayService) getOpenAICompatSessionTurnState(_ context.Context, c *gin.Context, account *Account, key string) string {
	return strings.TrimSpace(s.loadOpenAICompatSessionBinding(c, account, key).TurnState)
}
func (s *OpenAIGatewayService) bindOpenAICompatSessionTurnState(_ context.Context, c *gin.Context, account *Account, key, turnState string) {
	if s == nil || strings.TrimSpace(turnState) == "" {
		return
	}
	cacheKey := openAICompatSessionResponseKey(c, account, key)
	if cacheKey == "" {
		return
	}
	openAICompatSessionBindingMu.Lock()
	defer openAICompatSessionBindingMu.Unlock()
	binding := openAICompatSessionResponseBinding{}
	if raw, ok := s.openaiCompatSessionResponses.Load(cacheKey); ok {
		binding, _ = raw.(openAICompatSessionResponseBinding)
	}
	binding.TurnState = strings.TrimSpace(turnState)
	binding.ExpiresAt = time.Now().Add(s.openAIWSResponseStickyTTL())
	s.openaiCompatSessionResponses.Store(cacheKey, binding)
}
