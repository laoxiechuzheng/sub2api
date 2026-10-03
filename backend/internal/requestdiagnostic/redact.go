package requestdiagnostic

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	redactedMarker    = "[REDACTED]"
	binaryMarker      = "[OMITTED:binary_data]"
	maxCanonicalBytes = 1024
	maxMetadataBytes  = 1024
	maxErrorBytes     = 4096
	maxJSONDepth      = 64
	maxJSONNodes      = 65536
)

var (
	knownTokens       = regexp.MustCompile(`(?i)(?:sk-(?:ant-|proj-|svcacct-)?[a-z0-9_-]{8,}|AIza[a-z0-9_-]{35}|GOCSPX-[a-z0-9_-]{8,}|ya29\.[a-z0-9._~-]{8,}|gh[pousr]_[a-z0-9]{16,}|github_pat_[a-z0-9_]{16,}|xox[baprs]-[a-z0-9-]{8,}|eyJ[a-z0-9_-]{8,}\.[a-z0-9_-]{8,}\.[a-z0-9_-]{8,}|AKIA[A-Z0-9]{16})`)
	bearerTokens      = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[a-z0-9._~+/=-]+`)
	credentialURL     = regexp.MustCompile(`(?i)\b(?:https?|socks5h?|socks4a?)://[^\s<>"']+`)
	credentialLines   = regexp.MustCompile(`(?im)\b(?:authorization|proxy[-_]authorization|cookie|set[-_]cookie)\s*[:=]\s*[^\r\n]+`)
	secretAssignments = regexp.MustCompile(`(?i)\b(?:api[-_]?key|x[-_]api[-_]key|access[-_]?token|refresh[-_]?token|id[-_]?token|client[-_]?secret|password|passwd|proxy[-_]?password|proxy[-_]?user(?:name)?|secret|token)\b["']?\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;&\]}]+)`)
)

func sanitizePayload(raw []byte, maxBytes int) Payload {
	p := Payload{OriginalBytes: len(raw)}
	if len(raw) == 0 {
		p.OmittedReason = OmittedEmptyBody
		return p
	}
	if !utf8.Valid(raw) {
		p.OmittedReason = OmittedInvalidUTF8
		return p
	}
	if reason := checkJSONShape(raw); reason != "" {
		p.OmittedReason = reason
		return p
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		p.OmittedReason = OmittedInvalidJSON
		return p
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		p.OmittedReason = OmittedInvalidJSON
		return p
	}
	state := redactionState{}
	value = state.redact(value, false, 0)
	p.Redacted = state.redacted
	p.OmittedReason = state.omittedReason
	encoded, err := json.Marshal(value)
	if err != nil {
		p.OmittedReason = OmittedInvalidJSON
		return p
	}
	p.Content = string(encoded)
	if maxBytes > 0 && len(p.Content) > maxBytes {
		p.Content = utf8Prefix(p.Content, maxBytes)
		p.Truncated = true
	}
	p.StoredBytes = len(p.Content)
	return p
}

// checkJSONShape先验证完整JSON，再无需分配树地限制容器深度和节点数量。
// 字符串的长度不会触发节点限制，因此普通长prompt仍可完整保留。
func checkJSONShape(raw []byte) string {
	if !json.Valid(raw) {
		return OmittedInvalidJSON
	}
	depth, nodes := 0, 0
	inString, escaped := false, false
	for i := 0; i < len(raw); i++ {
		char := raw[i]
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
			nodes++
		case '{', '[':
			depth++
			nodes++
		case '}', ']':
			depth--
		case ' ', '\n', '\r', '\t', ':', ',':
		default:
			nodes++
			for i+1 < len(raw) {
				next := raw[i+1]
				if next == ',' || next == '}' || next == ']' || next == ' ' || next == '\n' || next == '\r' || next == '\t' {
					break
				}
				i++
			}
		}
		if depth > maxJSONDepth {
			return OmittedDepthLimit
		}
		if nodes > maxJSONNodes {
			return OmittedNodeLimit
		}
	}
	return ""
}

type redactionState struct {
	redacted      bool
	omittedReason string
}

func (s *redactionState) omit(reason string) {
	if s.omittedReason == "" {
		s.omittedReason = reason
	}
}

func (s *redactionState) redact(value any, proxy bool, depth int) any {
	if depth > maxJSONDepth {
		s.omit(OmittedDepthLimit)
		return "[OMITTED:depth_limit]"
	}
	switch v := value.(type) {
	case map[string]any:
		// name/value 形式的 header 列表也必须遵守敏感字段脱敏。
		secretValue := false
		binaryObject := false
		for key, item := range v {
			normalized := normalizeKey(key)
			if normalized == "name" || normalized == "key" {
				if name, ok := item.(string); ok && sensitiveKey(name) {
					secretValue = true
				}
			}
			if normalized == "type" || normalized == "encoding" || normalized == "mediatype" || normalized == "mimetype" {
				if label, ok := item.(string); ok {
					label = strings.ToLower(label)
					binaryObject = binaryObject || label == "base64" || label == "image" || strings.HasPrefix(label, "image/") || strings.HasPrefix(label, "audio/") || strings.HasPrefix(label, "video/") || label == "application/pdf"
				}
			}
		}
		output := make(map[string]any, len(v))
		for key, item := range v {
			cleanKey, keyChanged := redactText(key)
			s.redacted = s.redacted || keyChanged
			normalized := normalizeKey(key)
			var clean any
			switch {
			case sensitiveKey(key) || (secretValue && (normalized == "value" || normalized == "values")) || (proxy && (normalized == "username" || normalized == "user" || normalized == "pass" || normalized == "login")):
				clean = redactedMarker
				s.redacted = true
			case binaryKey(normalized) || (binaryObject && (normalized == "data" || normalized == "bytes")):
				clean = binaryMarker
				s.omit(OmittedBinaryData)
			default:
				childProxy := proxy || strings.Contains(normalized, "proxy")
				if _, scalar := item.(string); childProxy && scalar {
					clean = redactedMarker
					s.redacted = true
				} else {
					clean = s.redact(item, childProxy, depth+1)
				}
			}
			if _, exists := output[cleanKey]; !exists {
				output[cleanKey] = clean
			}
		}
		return output
	case []any:
		for i, item := range v {
			v[i] = s.redact(item, proxy, depth+1)
		}
		return v
	case string:
		if strings.Contains(strings.ToLower(v), "data:") && strings.Contains(strings.ToLower(v), ";base64,") {
			s.omit(OmittedBinaryData)
			return binaryMarker
		}
		// 工具参数或嵌套请求可能编码在 JSON 字符串里，仍需递归脱敏。
		// 仅解析明显像 JSON object/array 的字符串，避免误删普通长文本。
		trimmed := strings.TrimSpace(v)
		if looksLikeEmbeddedJSON(trimmed) {
			if reason := checkJSONShape([]byte(trimmed)); reason != "" {
				s.omit(reason)
				return "[OMITTED:" + reason + "]"
			}
			decoder := json.NewDecoder(strings.NewReader(trimmed))
			decoder.UseNumber()
			var embedded any
			if decoder.Decode(&embedded) == nil {
				var extra any
				if decoder.Decode(&extra) == io.EOF {
					nested := redactionState{}
					clean := nested.redact(embedded, proxy, depth+1)
					if nested.redacted || nested.omittedReason != "" {
						s.redacted = s.redacted || nested.redacted
						s.omit(nested.omittedReason)
						if encoded, err := json.Marshal(clean); err == nil {
							return string(encoded)
						}
						s.redacted = true
						return redactedMarker
					}
					return v
				}
			}
			s.omit(OmittedInvalidJSON)
			return "[OMITTED:invalid_json]"
		}
		redacted, changed := redactText(v)
		s.redacted = s.redacted || changed
		return redacted
	default:
		return value
	}
}

func looksLikeEmbeddedJSON(input string) bool {
	if len(input) < 2 || (input[0] != '{' && input[0] != '[') {
		return false
	}
	remainder := strings.TrimLeft(input[1:], " \r\n\t")
	if remainder == "" {
		return false
	}
	if input[0] == '{' {
		return remainder[0] == '"' || remainder[0] == '}'
	}
	char := remainder[0]
	return char == '"' || char == '{' || char == '[' || char == ']' || char == '-' || (char >= '0' && char <= '9') || strings.HasPrefix(remainder, "true") || strings.HasPrefix(remainder, "false") || strings.HasPrefix(remainder, "null")
}

func binaryKey(normalized string) bool {
	switch normalized {
	case "base64", "b64json", "imagedata", "imagebase64", "audiobase64", "filedata", "inlinedata":
		return true
	default:
		return false
	}
}

func sensitiveKey(key string) bool {
	key = normalizeKey(key)
	switch key {
	case "authorization", "proxyauthorization", "cookie", "cookies", "setcookie", "setcookies", "apikey", "xapikey", "token", "accesstoken", "refreshtoken", "idtoken", "authtoken", "bearertoken", "clientsecret", "secret", "secretkey", "password", "passwd", "pwd", "credentials", "credential", "proxycredentials", "proxypassword", "proxyusername", "proxyuser", "privatekey", "signingkey", "codeverifier", "authorizationcode":
		return true
	default:
		for _, suffix := range []string{"authorization", "cookie", "apikey", "token", "secret", "password", "passwd", "privatekey", "credential", "credentials"} {
			if strings.HasSuffix(key, suffix) {
				return true
			}
		}
		return key == "auth" || key == "authentication"
	}
}

func normalizeKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.NewReplacer("-", "", "_", "", " ", "", ".", "").Replace(key)
}

func redactText(input string) (string, bool) {
	out := credentialURL.ReplaceAllStringFunc(input, func(candidate string) string {
		u, err := url.Parse(candidate)
		if err != nil || u.Host == "" {
			return redactedMarker
		}
		changed := u.User != nil || u.RawQuery != "" || u.Fragment != ""
		if !changed {
			return candidate
		}
		u.User = nil
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		return u.String()
	})
	out = credentialLines.ReplaceAllString(out, redactedMarker)
	out = secretAssignments.ReplaceAllString(out, redactedMarker)
	out = bearerTokens.ReplaceAllString(out, redactedMarker)
	out = knownTokens.ReplaceAllString(out, redactedMarker)
	return out, out != input
}

func cleanInbound(input Inbound) (Inbound, bool, bool) {
	output := Inbound{StartedAt: input.StartedAt.UTC(), Body: omittedPayload(0, OmittedNotCaptured)}
	if output.StartedAt.IsZero() {
		output.StartedAt = time.Now().UTC()
	}
	redacted, truncated := false, false
	output.RequestID = cleanMetadata(input.RequestID, maxMetadataBytes, &redacted, &truncated)
	output.ClientRequestID = cleanMetadata(input.ClientRequestID, maxMetadataBytes, &redacted, &truncated)
	output.Endpoint = cleanEndpoint(input.Endpoint, &redacted, &truncated)
	output.Method = cleanMetadata(input.Method, 32, &redacted, &truncated)
	output.UserAgent = cleanMetadata(input.UserAgent, maxMetadataBytes, &redacted, &truncated)
	return output, redacted, truncated
}

func cleanAttempt(input Attempt, started time.Time) Attempt {
	output := Attempt{
		AccountID:       input.AccountID,
		StartedAt:       input.StartedAt.UTC(),
		ResponseSummary: omittedPayload(0, OmittedNotCaptured),
	}
	if output.StartedAt.IsZero() {
		output.StartedAt = started.UTC()
	}
	output.Platform = cleanMetadata(input.Platform, 128, &output.MetadataRedacted, &output.MetadataTruncated)
	output.Endpoint = cleanEndpoint(input.Endpoint, &output.MetadataRedacted, &output.MetadataTruncated)
	output.Method = cleanMetadata(input.Method, 32, &output.MetadataRedacted, &output.MetadataTruncated)
	output.Model = cleanMetadata(input.Model, maxMetadataBytes, &output.MetadataRedacted, &output.MetadataTruncated)
	return output
}

func cleanResult(input AttemptResult, redacted, truncated bool) (string, string, bool, bool) {
	errorInput := input.Error
	if looksLikeEmbeddedJSON(strings.TrimSpace(errorInput)) {
		payload := sanitizePayload([]byte(errorInput), maxErrorBytes)
		if payload.Content == "" && payload.OmittedReason != "" {
			errorInput = "[OMITTED:" + payload.OmittedReason + "]"
		} else {
			errorInput = payload.Content
		}
		redacted = redacted || payload.Redacted || payload.OmittedReason != ""
		truncated = truncated || payload.Truncated
	}
	errText := cleanMetadata(errorInput, maxErrorBytes, &redacted, &truncated)
	requestID := cleanMetadata(input.UpstreamRequestID, maxMetadataBytes, &redacted, &truncated)
	return errText, requestID, redacted, truncated
}

func cleanMetadata(input string, maxBytes int, redacted, truncated *bool) string {
	if !utf8.ValidString(input) {
		*redacted = true
		return redactedMarker
	}
	output, changed := redactText(input)
	*redacted = *redacted || changed
	if len(output) > maxBytes {
		output = utf8Prefix(output, maxBytes)
		*truncated = true
	}
	return strings.Clone(output)
}

func cleanEndpoint(input string, redacted, truncated *bool) string {
	parsed, err := url.Parse(input)
	if err != nil {
		*redacted = true
		return redactedMarker
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		*redacted = true
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return cleanMetadata(parsed.String(), maxMetadataBytes, redacted, truncated)
}

func validCanonicalID(input string, maxBytes int) bool {
	if len(input) > maxCanonicalBytes || len(input) > maxBytes || !utf8.ValidString(input) {
		return false
	}
	for _, char := range input {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	_, changed := redactText(input)
	return !changed
}

func omittedPayload(originalBytes int, reason string) Payload {
	return Payload{OriginalBytes: max(0, originalBytes), OmittedReason: normalizeReason(reason, OmittedNotCaptured)}
}

func droppedPayload(p Payload, reason string) Payload {
	p.Content = ""
	p.StoredBytes = 0
	p.OmittedReason = normalizeReason(reason, OmittedDropped)
	return p
}

func normalizeReason(reason, fallback string) string {
	switch reason {
	case OmittedNotCaptured, OmittedEmptyBody, OmittedInvalidJSON, OmittedInvalidUTF8, OmittedDepthLimit, OmittedNodeLimit, OmittedBinaryData, OmittedBudget, OmittedEvicted, OmittedDropped, OmittedTooLarge:
		return reason
	default:
		return fallback
	}
}

func utf8Prefix(input string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(input) <= maxBytes {
		return input
	}
	for maxBytes > 0 && !utf8.RuneStart(input[maxBytes]) {
		maxBytes--
	}
	return strings.Clone(input[:maxBytes])
}
