package service

import (
	"net/http"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
)

type CompositeRequestClassification struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

func explainCompositeRoutes(routes []CompositeModelRoute, model, endpoint string, match CompositeRouteRequestMatch, selected *CompositeModelRoute) []CompositeRouteConditionEvaluation {
	evaluations := make([]CompositeRouteConditionEvaluation, 0, len(routes))
	for _, route := range routes {
		if !route.Enabled {
			continue
		}
		evaluation := evaluateCompositeRouteConditions(route, match)
		routeEndpoint := normalizeCompositeRouteEndpoint(route.Endpoint)
		if routeEndpoint != endpoint && routeEndpoint != CompositeRouteEndpointAny {
			evaluation.Matched = false
			evaluation.Checks = append(evaluation.Checks, CompositeRouteConditionCheck{Field: "endpoint", Matched: false, Reason: "endpoint_mismatch"})
		}
		publicModel := strings.TrimSpace(route.PublicModel)
		modelMatch := publicModel != ""
		switch normalizeCompositeRouteMatchType(route.MatchType) {
		case CompositeRouteMatchExact:
			modelMatch = modelMatch && publicModel == model
		case CompositeRouteMatchPrefix:
			modelMatch = modelMatch && strings.HasPrefix(model, publicModel)
		case CompositeRouteMatchContains:
			modelMatch = modelMatch && strings.Contains(model, publicModel)
		}
		if !modelMatch {
			evaluation.Matched = false
			evaluation.Checks = append(evaluation.Checks, CompositeRouteConditionCheck{Field: "public_model", Matched: false, Reason: "model_mismatch"})
		}
		evaluation.Selected = selected != nil && selected.ID == route.ID
		evaluations = append(evaluations, evaluation)
	}
	return evaluations
}

type CompositeRouteConditionCheck struct {
	Field   string `json:"field"`
	Matched bool   `json:"matched"`
	Reason  string `json:"reason"`
}

type CompositeRouteConditionEvaluation struct {
	RouteID        int64                          `json:"route_id"`
	Matched        bool                           `json:"matched"`
	Selected       bool                           `json:"selected"`
	BodyMatchScope string                         `json:"body_match_scope"`
	Checks         []CompositeRouteConditionCheck `json:"checks"`
}

// Facts are shared across all candidate rules and parsed only when a scoped
// condition or request-kind check needs them. No request content enters traces.
type compositeRequestFacts struct {
	body                 []byte
	endpoint             string
	nativeCompaction     bool
	claudeCode           bool
	claudeCompactionHint string
	parsed               bool
	valid                bool
	invalidReason        string
	instructions         []string
	terminal             []string
	terminalBlocks       []string
	terminalTailBlock    string
}

func newCompositeRequestFacts(match CompositeRouteRequestMatch) *compositeRequestFacts {
	ua := strings.ToLower(strings.TrimSpace(match.UserAgent))
	return &compositeRequestFacts{body: match.Body, nativeCompaction: match.NativeCompaction,
		claudeCode:           strings.HasPrefix(ua, "claude-cli/") || strings.HasPrefix(ua, "claude-code/"),
		claudeCompactionHint: match.ClaudeCompactionHint}
}

// Client hints describe request intent, not authorization or a native wire protocol.
// Ignore duplicate/conflicting/unknown values instead of guessing from substrings.
func ClaudeCompactionHintFromHeaders(headers http.Header) string {
	hint := ""
	for _, name := range []string{"X-Claude-Code-Compaction", "X-Cc-Compaction-Request", "X-Claude-Code-Request-Class"} {
		values := headers.Values(name)
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 {
			return ""
		}
		value := strings.ToLower(strings.TrimSpace(values[0]))
		if name == "X-Claude-Code-Request-Class" {
			if value != "compaction" {
				return ""
			}
			if hint == "" {
				hint = value
			}
			continue
		}
		if value != "auto" && value != "manual" && value != "reactive" {
			return ""
		}
		if hint != "" && hint != value {
			return ""
		}
		hint = value
	}
	return hint
}

func (f *compositeRequestFacts) parse() {
	if f.parsed {
		return
	}
	f.parsed = true
	f.invalidReason = "invalid_json"
	if !gjson.ValidBytes(f.body) {
		return
	}
	root := gjson.ParseBytes(f.body)
	if !root.IsObject() {
		return
	}
	if compositeEnvelopeAmbiguous(root) {
		f.invalidReason = "ambiguous_json"
		return
	}
	var messages gjson.Result
	switch f.endpoint {
	case CompositeRouteEndpointResponses:
		// Share the forwarding contract: native input wins over messages;
		// legacy messages/prompt are accepted only through the ingress adapter.
		normalized, _, err := normalizeOpenAIResponsesLegacyIngress(f.body)
		if err != nil {
			return
		}
		root = gjson.ParseBytes(normalized)
		// Responses root instructions are a string, unlike Anthropic system
		// blocks. Unsupported types must not contribute routing evidence.
		if instructions := root.Get("instructions"); instructions.Type == gjson.String {
			f.instructions = []string{instructions.String()}
		}
		messages = root.Get("input")
	case CompositeRouteEndpointMessages, CompositeRouteEndpointCountTokens:
		f.instructions = compositeDirectText(root.Get("system"), f.endpoint)
		messages = root.Get("messages")
	case CompositeRouteEndpointChatCompletions:
		messages = root.Get("messages")
	default:
		f.invalidReason = "unsupported_endpoint"
		return
	}
	f.valid = true
	if messages.Type == gjson.String {
		f.terminal = append(f.terminal, messages.String())
		f.terminalBlocks = append(f.terminalBlocks, messages.String())
		f.terminalTailBlock = messages.String()
		return
	}
	if !messages.IsArray() {
		return
	}
	items := messages.Array()
	for _, item := range items {
		itemType := item.Get("type").String()
		if itemType != "" && itemType != "message" {
			break
		}
		role := item.Get("role").String()
		if role != "system" && role != "developer" {
			break
		}
		f.instructions = append(f.instructions, compositeDirectText(item.Get("content"), f.endpoint)...)
	}
	if len(items) == 0 {
		return
	}
	index := len(items) - 1
	// Claude Code can flush pure-text system carriers after its pending user
	// message. Never cross assistant/tool output or change other protocols.
	if f.claudeCode && f.endpoint == CompositeRouteEndpointMessages {
		for index >= 0 && compositeClaudeSystemCarrier(items[index]) {
			index--
		}
	}
	if index < 0 {
		return
	}
	last := items[index]
	if last.Get("role").String() != "user" {
		return
	}
	// A function/tool item cannot masquerade as a user message by adding role.
	itemType := last.Get("type").String()
	if itemType != "" && itemType != "message" {
		return
	}
	f.terminal = compositeDirectText(last.Get("content"), f.endpoint)
	f.terminalBlocks = compositeDirectTextBlocks(last.Get("content"), f.endpoint)
	f.terminalTailBlock = compositeLastDirectTextBlock(last.Get("content"), f.endpoint)
}

func compositeClaudeSystemCarrier(item gjson.Result) bool {
	if item.Get("role").String() != "system" {
		return false
	}
	if typ := item.Get("type"); typ.Exists() && (typ.Type != gjson.String || typ.String() != "message") {
		return false
	}
	content := item.Get("content")
	if content.Type == gjson.String {
		return true
	}
	if !content.IsArray() || len(content.Array()) == 0 {
		return false
	}
	for _, block := range content.Array() {
		if block.Get("type").String() != "text" || block.Get("text").Type != gjson.String {
			return false
		}
	}
	return true
}

func compositeClaudeDirective(text string) string {
	text = strings.TrimSpace(text)
	// Strip only complete leading reminder envelopes. Their contents are not
	// directives; arbitrary prose/quotes must not gain a new artificial start.
	for strings.HasPrefix(text, "<system-reminder>") {
		depth, offset := 1, len("<system-reminder>")
		for depth > 0 {
			next := strings.IndexByte(text[offset:], '<')
			if next < 0 {
				return ""
			}
			offset += next
			switch {
			case strings.HasPrefix(text[offset:], "<system-reminder>"):
				depth++
				offset += len("<system-reminder>")
			case strings.HasPrefix(text[offset:], "</system-reminder>"):
				depth--
				offset += len("</system-reminder>")
			default:
				offset++
			}
		}
		text = strings.TrimSpace(text[offset:])
	}
	return text
}

func compositeObjectAmbiguous(value gjson.Result) bool {
	if !value.IsObject() {
		return false
	}
	keys := make(map[string]struct{})
	ambiguous := false
	value.ForEach(func(key, _ gjson.Result) bool {
		name := compositeFoldedJSONKey(key.String())
		if _, exists := keys[name]; exists {
			ambiguous = true
			return false
		}
		keys[name] = struct{}{}
		return true
	})
	return ambiguous
}

// Use the same Unicode equivalence classes as encoding/json struct fields,
// including long s and the Kelvin sign; lowercasing alone is not equivalent.
func compositeFoldedJSONKey(key string) string {
	var folded strings.Builder
	folded.Grow(len(key))
	for _, r := range key {
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				folded.WriteRune(next)
				break
			}
			r = next
		}
	}
	return folded.String()
}

// Inspect only routing envelopes and direct content blocks, not tool schemas
// or arbitrary nested output. GJSON and encoding/json differ on duplicate keys.
func compositeEnvelopeAmbiguous(root gjson.Result) bool {
	if compositeObjectAmbiguous(root) {
		return true
	}
	contentAmbiguous := func(content gjson.Result) bool {
		if content.IsArray() {
			for _, block := range content.Array() {
				if compositeObjectAmbiguous(block) {
					return true
				}
			}
		}
		return false
	}
	for _, field := range []string{"instructions", "system"} {
		if contentAmbiguous(root.Get(field)) {
			return true
		}
	}
	for _, field := range []string{"messages", "input"} {
		if items := root.Get(field); items.IsArray() {
			for _, item := range items.Array() {
				if compositeObjectAmbiguous(item) || contentAmbiguous(item.Get("content")) {
					return true
				}
			}
		}
	}
	return false
}

func compositeDirectText(content gjson.Result, endpoint string) []string {
	blocks := compositeDirectTextBlocks(content, endpoint)
	if len(blocks) == 0 {
		return nil
	}
	// Preserve a message as one instruction. A quoted directive in a later
	// text block must not gain a new artificial beginning for general rules.
	return []string{strings.Join(blocks, "\n")}
}

func compositeDirectTextBlocks(content gjson.Result, endpoint string) []string {
	if content.Type == gjson.String {
		return []string{content.String()}
	}
	if !content.IsArray() {
		return nil
	}
	var text []string
	for _, block := range content.Array() {
		switch block.Get("type").String() {
		case "text":
		case "input_text":
			if endpoint != CompositeRouteEndpointResponses {
				continue
			}
		default:
			continue
		}
		value := block.Get("text")
		if value.Type == gjson.String {
			text = append(text, value.String())
		}
	}
	if len(text) == 0 {
		return nil
	}
	return text
}

func compositeLastDirectTextBlock(content gjson.Result, endpoint string) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	blocks := content.Array()
	for index := len(blocks) - 1; index >= 0; index-- {
		block := blocks[index]
		blockType := block.Get("type").String()
		if blockType != "text" && !(endpoint == CompositeRouteEndpointResponses && blockType == "input_text") {
			return ""
		}
		value := block.Get("text")
		if value.Type != gjson.String {
			return ""
		}
		if strings.TrimSpace(value.String()) != "" {
			return value.String()
		}
	}
	return ""
}

func (f *compositeRequestFacts) classify() CompositeRequestClassification {
	f.parse()
	if !f.valid {
		return CompositeRequestClassification{Kind: "unknown", Source: f.invalidReason, Reason: f.invalidReason}
	}
	if f.nativeCompaction {
		return CompositeRequestClassification{Kind: "compaction", Source: "native_endpoint", Reason: "native_endpoint"}
	}
	if f.claudeCode && f.endpoint == CompositeRouteEndpointMessages {
		switch f.claudeCompactionHint {
		case "auto", "manual", "reactive", "compaction":
			return CompositeRequestClassification{Kind: "compaction", Source: "claude_request_header", Reason: "claude_request_header"}
		}
	}
	for _, text := range f.terminal {
		if f.claudeCode && f.endpoint == CompositeRouteEndpointMessages {
			text = compositeClaudeDirective(text)
		}
		if source := compositeCompactionSignature(text, false, f.claudeCode && f.endpoint == CompositeRouteEndpointMessages); source != "" {
			return CompositeRequestClassification{Kind: "compaction", Source: source, Reason: source}
		}
	}
	if f.claudeCode && f.endpoint == CompositeRouteEndpointMessages {
		text := compositeClaudeDirective(f.terminalTailBlock)
		// Claude Code can merge earlier user text, local-command transport
		// blocks, and this complete official template into one user message.
		// Accept only the final effective text block when the complete official
		// template starts at byte zero; short signatures and quoted prompts stay out.
		if len(text) >= compositeClaudeCompactionStandaloneMinBytes &&
			compositeClaudeCompleteCompactionPrompt(text, true) {
			return CompositeRequestClassification{Kind: "compaction", Source: "claude_terminal_prompt", Reason: "claude_terminal_prompt"}
		}
	}
	for _, text := range f.instructions {
		if source := compositeCompactionSignature(text, true, false); source != "" {
			return CompositeRequestClassification{Kind: "compaction", Source: source, Reason: source}
		}
	}
	return CompositeRequestClassification{Kind: "conversation", Source: "no_compaction_signature", Reason: "no_compaction_signature"}
}

const (
	compositeClaudeCompactionCritical = "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools."
	compositeClaudeCompactionFull     = "Your task is to create a detailed summary of the conversation so far"
	// Public 2.1.286 complete templates are at least 4.4 KiB before custom
	// instructions. The lower bound rejects short quoted signatures.
	compositeClaudeCompactionStandaloneMinBytes = 2048
)

var compositeClaudeCompactionWarningLines = []string{
	"- Do NOT use Read, Bash, Grep, Glob, Edit, Write, or ANY other tool.",
	"- You already have all the context you need in the conversation above.",
	"- Tool calls will be REJECTED and will waste your only turn \u2014 you will fail the task.",
	"- Your entire response must be plain text: an <analysis> block followed by a <summary> block.",
}

func compositeClaudeCompactionPrompt(text string, allowPartial bool) bool {
	return compositeClaudeCompactionPromptInternal(text, allowPartial, false)
}

func compositeClaudeCompleteCompactionPrompt(text string, allowPartial bool) bool {
	return compositeClaudeCompactionPromptInternal(text, allowPartial, true)
}

func compositeClaudeCompactionPromptInternal(text string, allowPartial, requireWarnings bool) bool {
	if strings.HasPrefix(text, compositeClaudeCompactionCritical) {
		text = strings.TrimSpace(text[len(compositeClaudeCompactionCritical):])
		if strings.HasPrefix(text, compositeClaudeCompactionWarningLines[0]) {
			for _, line := range compositeClaudeCompactionWarningLines {
				if !strings.HasPrefix(text, line) {
					return false
				}
				text = strings.TrimSpace(text[len(line):])
			}
		} else if requireWarnings {
			return false
		}
	} else {
		if requireWarnings {
			return false
		}
		return strings.HasPrefix(text, compositeClaudeCompactionFull)
	}
	if strings.HasPrefix(text, compositeClaudeCompactionFull) {
		return true
	}
	if !allowPartial {
		return false
	}
	return strings.HasPrefix(text, "Your task is to create a detailed summary of the RECENT portion of the conversation") ||
		strings.HasPrefix(text, "Your task is to create a detailed summary of this conversation. This summary will be placed at the start of a continuing session; newer messages that build on this context will follow after your summary")
}

func compositeCompactionSignature(text string, instructions, allowClaudePartial bool) string {
	text = strings.TrimSpace(text)
	// Anchor full, known directives at the beginning. A mention in a quote,
	// tool result, summary or explanatory paragraph is not a compaction signal.
	if !instructions && compositeClaudeCompactionPrompt(text, allowClaudePartial) {
		return "claude_terminal_prompt"
	}
	codexPrefixes := []string{
		"You are a context summarization and handoff agent. Produce one accurate, self-contained record that allows another coding agent to continue the work without losing the user's intent, constraints, technical context, or latest working state.",
		"You are a context summarization agent. Based on the current conversation, produce a structured summary so another coding agent can continue the work.",
	}
	for _, prefix := range codexPrefixes {
		if strings.HasPrefix(text, prefix) {
			if instructions {
				return "codex_instructions"
			}
			return "codex_terminal_prompt"
		}
	}
	if (strings.HasPrefix(text, "You are performing a CONTEXT CHECKPOINT COMPACTION") ||
		strings.HasPrefix(text, "You are doing a CONTEXT CHECKPOINT COMPACTION")) &&
		strings.Contains(strings.ToLower(text), "summary") && strings.Contains(strings.ToLower(text), "another") {
		if instructions {
			return "codex_instructions"
		}
		return "codex_terminal_prompt"
	}
	opencodePrefixes := []string{
		"You are a context summarization agent. You are given a conversation between a user and an agent. Your goal is to produce a structured summary matching the format specified so another coding agent can continue the work.",
		"You are a helpful AI assistant tasked with summarizing conversations.",
	}
	for _, prefix := range opencodePrefixes {
		if strings.HasPrefix(text, prefix) {
			if instructions {
				return "opencode_instructions"
			}
			return "opencode_terminal_prompt"
		}
	}
	return ""
}

func compositeConditionDefault(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return fallback
	}
	return value
}

func evaluateCompositeRouteConditions(route CompositeModelRoute, match CompositeRouteRequestMatch) CompositeRouteConditionEvaluation {
	scope := compositeConditionDefault(route.BodyMatchScope, "full_body")
	evaluation := CompositeRouteConditionEvaluation{RouteID: route.ID, Matched: true, BodyMatchScope: scope}
	if match.IgnoreRequestConditions {
		return evaluation
	}
	add := func(field string, matched bool, reason string) {
		evaluation.Matched = evaluation.Matched && matched
		if match.Explain {
			evaluation.Checks = append(evaluation.Checks, CompositeRouteConditionCheck{Field: field, Matched: matched, Reason: reason})
		}
	}
	facts := match.facts
	if facts == nil {
		facts = newCompositeRequestFacts(match)
	}
	uaPatterns := splitCompositeRouteConditionPatterns(route.UserAgentContains)
	if len(uaPatterns) > 0 {
		ua := strings.ToLower(strings.TrimSpace(match.UserAgent))
		ok := false
		for _, pattern := range uaPatterns {
			if ua != "" && strings.Contains(ua, strings.ToLower(pattern)) {
				ok = true
				break
			}
		}
		reason := "user_agent_match"
		if !ok {
			reason = "user_agent_mismatch"
		}
		add("user_agent_contains", ok, reason)
	}
	if !evaluation.Matched && !match.Explain {
		return evaluation
	}
	kind := compositeConditionDefault(route.RequestKind, "any")
	switch kind {
	case "any":
	case "compaction", "conversation":
		ok := facts.classify().Kind == kind
		reason := "request_kind_match"
		if !ok {
			reason = "request_kind_mismatch"
		}
		add("request_kind", ok, reason)
	default:
		add("request_kind", false, "unknown_request_kind")
	}
	if !evaluation.Matched && !match.Explain {
		return evaluation
	}
	mode := compositeConditionDefault(route.BodyMatchMode, "any")
	switch mode {
	case "any", "all", "prefix":
	default:
		add("body_match_mode", false, "unknown_body_mode")
		return evaluation
	}
	switch scope {
	case "full_body", "instructions", "last_message", "current_turn":
	default:
		add("body_match_scope", false, "unknown_body_scope")
		return evaluation
	}
	positive := splitCompositeRouteConditionPatterns(route.BodyContains)
	negative := splitCompositeRouteConditionPatterns(route.BodyNotContains)
	if len(positive) == 0 && len(negative) == 0 {
		if match.Explain && len(evaluation.Checks) == 0 {
			add("conditions", true, "no_conditions")
		}
		return evaluation
	}
	var texts []string
	if scope == "full_body" {
		if len(match.Body) == 0 {
			add("body_match_scope", false, "scope_empty")
			return evaluation
		}
	} else {
		facts.parse()
		if !facts.valid {
			add("body_match_scope", false, facts.invalidReason)
			return evaluation
		}
		switch scope {
		case "instructions":
			texts = facts.instructions
		case "last_message":
			if facts.claudeCode && facts.endpoint == CompositeRouteEndpointMessages && len(facts.terminalBlocks) > 0 {
				texts = facts.terminalBlocks
			} else {
				texts = facts.terminal
			}
		case "current_turn":
			texts = make([]string, 0, len(facts.instructions)+len(facts.terminal))
			texts = append(texts, facts.instructions...)
			texts = append(texts, facts.terminal...)
		}
		hasText := false
		for _, text := range texts {
			if strings.TrimSpace(text) != "" {
				hasText = true
				break
			}
		}
		if !hasText {
			add("body_match_scope", false, "scope_empty")
			return evaluation
		}
	}
	contains := func(pattern string, prefix bool) bool {
		if scope == "full_body" {
			if prefix {
				return strings.HasPrefix(strings.TrimSpace(string(match.Body)), pattern)
			}
			return requestBodyContainsPattern(match.Body, pattern)
		}
		for _, text := range texts {
			if prefix && strings.HasPrefix(strings.TrimSpace(text), pattern) {
				return true
			}
			if !prefix && strings.Contains(text, pattern) {
				return true
			}
		}
		return false
	}
	if len(positive) > 0 {
		ok := mode == "all"
		for _, pattern := range positive {
			found := contains(pattern, mode == "prefix")
			if mode == "all" && !found {
				ok = false
				break
			}
			if mode != "all" && found {
				ok = true
				break
			}
		}
		reason := "body_contains_match"
		if !ok {
			reason = "body_contains_missing"
		}
		add("body_contains", ok, reason)
	}
	if !evaluation.Matched && !match.Explain {
		return evaluation
	}
	if len(negative) > 0 {
		clear := true
		for _, pattern := range negative {
			if contains(pattern, false) {
				clear = false
				break
			}
		}
		reason := "body_not_contains_clear"
		if !clear {
			reason = "body_not_contains_excluded"
		}
		add("body_not_contains", clear, reason)
	}
	return evaluation
}
