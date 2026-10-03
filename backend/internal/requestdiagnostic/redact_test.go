package requestdiagnostic_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
)

func snapshotForBody(t *testing.T, body []byte) *requestdiagnostic.Snapshot {
	t.Helper()
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound(body)
	capture.BindUsage(1, "canonical", time.Now())
	capture.Finish(200)
	if snapshot == nil {
		t.Fatal("capture did not publish")
	}
	return snapshot
}

func TestCaptureRedactsCredentialsRecursivelyIncludingStringBodies(t *testing.T) {
	body := []byte(`{
		"Authorization":"Bearer auth-private",
		"Cookie":"session=cookie-private",
		"Proxy-Authorization":"Basic proxy-private",
		"nested":[{"apiKey":"key-private","refresh_token":"refresh-private","private_key":"pem-private"}],
		"proxy":{"username":"proxy-user-private","password":"proxy-password-private","url":"http://url-user-private:url-password-private@proxy.example:8080/?token=url-token-private"},
		"headers":[{"name":"Cookie","value":"header-cookie-private"}],
		"messages":[{"content":"keep ordinary prompt; sk-proj-abcdefghijklmnopqrstuvwxyz123456; Bearer bearer-private; https://url-user:url-password@example.com/path?api_key=query-private"}],
		"embedded":"{\"api_key\":\"embedded-private\",\"text\":\"normal embedded text\"}",
		"sk-ant-secretkeyinfieldname":"field value is ordinary",
		"number":123456789012345678901234567890,
		"ordinary":"do not alter this text"
	}`)
	original := bytes.Clone(body)
	snapshot := snapshotForBody(t, body)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"auth-private", "cookie-private", "proxy-private", "key-private", "refresh-private", "pem-private", "proxy-user-private", "proxy-password-private", "url-user-private", "url-password-private", "url-token-private", "header-cookie-private", "sk-proj-abcdefghijklmnopqrstuvwxyz123456", "bearer-private", "url-user", "url-password", "query-private", "embedded-private", "sk-ant-secretkeyinfieldname",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("credential leaked: %q", secret)
		}
	}
	if !snapshot.Meta.Body.Redacted || snapshot.Meta.Body.Truncated || !json.Valid([]byte(snapshot.Meta.Body.Content)) {
		t.Fatal("redaction flags or JSON output incorrect")
	}
	for _, ordinary := range []string{"keep ordinary prompt", "normal embedded text", "do not alter this text", "123456789012345678901234567890"} {
		if !strings.Contains(snapshot.Meta.Body.Content, ordinary) {
			t.Errorf("ordinary text/number was damaged: %q", ordinary)
		}
	}
	if !bytes.Equal(body, original) {
		t.Fatal("capturing modified original request bytes")
	}
}

func TestCaptureOmitsBinaryImagesButPreservesOrdinaryLongText(t *testing.T) {
	ordinary := strings.Repeat("ordinary prompt text 中文 ", 20_000)
	binary := strings.Repeat("A", 100_000)
	input := map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": ordinary},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": binary}},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + binary}},
			},
		}},
		"base64":      binary,
		"attachments": []any{map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": binary}}},
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := snapshotForBody(t, encoded)
	payload := snapshot.Meta.Body
	if payload.Truncated || payload.OmittedReason != requestdiagnostic.OmittedBinaryData || snapshot.CaptureStatus != requestdiagnostic.CaptureStatusPartial {
		t.Fatalf("binary omission must be explicit, not arbitrary string truncation: original=%d stored=%d truncated=%v omitted=%q status=%q", payload.OriginalBytes, payload.StoredBytes, payload.Truncated, payload.OmittedReason, snapshot.CaptureStatus)
	}
	if strings.Contains(payload.Content, binary[:1024]) || strings.Contains(payload.Content, "data:image/png;base64,") {
		t.Fatal("binary data stored")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload.Content), &decoded); err != nil {
		t.Fatalf("binary omission broke JSON: %v", err)
	}
	messages := decoded["messages"].([]any)
	content := messages[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["text"] != ordinary {
		t.Fatal("ordinary long text was shortened")
	}
}

func TestCaptureRedactsErrorMetadataAndAlternateCredentialFields(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{
		Endpoint:  "https://endpoint-user:endpoint-password@example.com/v1/messages?token=endpoint-token#fragment-private",
		UserAgent: "agent Bearer ua-private",
	}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound([]byte(`{"auth":"auth-private","session_token":"session-private","x-goog-api-key":"goog-private","proxy":{"user":"proxy-user","pass":"proxy-pass"}}`))
	id := capture.BeginAttempt(requestdiagnostic.Attempt{Endpoint: "http://upstream-user:upstream-pass@up.example/path?key=upstream-key"}, []byte(`{"ok":true}`))
	capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 502, Error: `{"error":{"cookie":"error-cookie-private","auth":"error-auth-private","session_token":"error-session-private"}}`})
	capture.BindUsage(1, "usage", time.Now())
	capture.Finish(502)
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"endpoint-user", "endpoint-password", "endpoint-token", "fragment-private", "ua-private", "auth-private", "session-private", "goog-private", "proxy-user", "proxy-pass", "upstream-user", "upstream-pass", "upstream-key", "error-cookie-private", "error-auth-private", "error-session-private"} {
		if strings.Contains(string(encoded), secret) {
			t.Errorf("credential metadata leaked: %q", secret)
		}
	}
	if snapshot.Meta.Endpoint != "https://example.com/v1/messages" || snapshot.Attempts[0].Endpoint != "http://up.example/path" || !snapshot.MetadataRedacted || !snapshot.Attempts[0].MetadataRedacted {
		t.Fatal("metadata redaction must preserve safe endpoint and flag changes")
	}
}

func TestCaptureNeverStoresCookieListsOrProxyCredentialStrings(t *testing.T) {
	body := []byte(`{"cookies":[{"name":"session","value":"cookie-list-private"}],"proxy":"proxy-user-private:proxy-pass-private@proxy.example:8080","proxy_config":{"user":"proxy-config-user","pwd":"proxy-config-password"}}`)
	snapshot := snapshotForBody(t, body)
	for _, secret := range []string{"cookie-list-private", "proxy-user-private", "proxy-pass-private", "proxy-config-user", "proxy-config-password"} {
		if strings.Contains(snapshot.Meta.Body.Content, secret) {
			t.Errorf("credential representation leaked: %q", secret)
		}
	}
	if !snapshot.Meta.Body.Redacted || !json.Valid([]byte(snapshot.Meta.Body.Content)) {
		t.Fatal("alternate credentials must be explicitly redacted in valid JSON")
	}
}

func TestCapturePreservesOrdinaryBracketedText(t *testing.T) {
	ordinary := "[NOTE] " + strings.Repeat("ordinary instructions ", 100_000)
	body, _ := json.Marshal(map[string]string{"text": ordinary})
	snapshot := snapshotForBody(t, body)
	var decoded map[string]string
	if err := json.Unmarshal([]byte(snapshot.Meta.Body.Content), &decoded); err != nil || decoded["text"] != ordinary || snapshot.Meta.Body.OmittedReason != "" {
		t.Fatal("ordinary bracketed long prompt was mistaken for embedded JSON")
	}
}

func TestCapturePreservesOrdinaryEmbeddedJSONAndBoundsNodeCount(t *testing.T) {
	t.Run("ordinary_embedded", func(t *testing.T) {
		body := []byte(`{"arguments":"{\"ordinary\":\"unchanged\"}"}`)
		snapshot := snapshotForBody(t, body)
		if snapshot.Meta.Body.Content != string(body) || snapshot.Meta.Body.OmittedReason != "" || snapshot.Meta.Body.Redacted {
			t.Fatal("valid ordinary JSON string must be preserved")
		}
	})
	t.Run("node_limit", func(t *testing.T) {
		body := []byte(`[` + strings.Repeat(`0,`, 70_000) + `0]`)
		snapshot := snapshotForBody(t, body)
		if snapshot.Meta.Body.Content != "" || snapshot.Meta.Body.OmittedReason != requestdiagnostic.OmittedNodeLimit {
			t.Fatal("excessive tiny JSON nodes must fail closed before building an unbounded tree")
		}
	})
}

func TestCaptureBoundsJSONDepthAndMalformedEmbeddedJSON(t *testing.T) {
	t.Run("depth", func(t *testing.T) {
		body := []byte(strings.Repeat(`{"nested":`, 100) + `"private-at-depth-limit"` + strings.Repeat("}", 100))
		snapshot := snapshotForBody(t, body)
		if snapshot.Meta.Body.OmittedReason != requestdiagnostic.OmittedDepthLimit || snapshot.Meta.Body.Content != "" || snapshot.Meta.Body.StoredBytes != 0 {
			t.Fatal("deep JSON must be explicitly omitted without leaking unvisited content")
		}
	})
	t.Run("malformed_embedded", func(t *testing.T) {
		body := []byte(`{"text":"{\"private_unrecognized_field\":\"embedded-private\""}`)
		snapshot := snapshotForBody(t, body)
		if strings.Contains(snapshot.Meta.Body.Content, "embedded-private") || snapshot.Meta.Body.OmittedReason != requestdiagnostic.OmittedInvalidJSON {
			t.Fatal("object-looking malformed JSON string was not fail-closed")
		}
	})
}

func TestCaptureFailsClosedForInvalidJSONAndUTF8(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   []byte
		reason string
	}{
		{name: "plain_text", body: []byte("private unstructured body"), reason: requestdiagnostic.OmittedInvalidJSON},
		{name: "invalid_object", body: []byte(`{"password":"private-secret"`), reason: requestdiagnostic.OmittedInvalidJSON},
		{name: "trailing_json", body: []byte(`{"ok":true}{"secret":"private-secret"}`), reason: requestdiagnostic.OmittedInvalidJSON},
		{name: "invalid_utf8", body: []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}, reason: requestdiagnostic.OmittedInvalidUTF8},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := snapshotForBody(t, test.body)
			payload := snapshot.Meta.Body
			if payload.Content != "" || payload.StoredBytes != 0 || payload.OriginalBytes != len(test.body) || payload.OmittedReason != test.reason {
				t.Fatalf("unsafe input was stored: %+v", payload)
			}
			if snapshot.CaptureStatus != requestdiagnostic.CaptureStatusOmitted {
				t.Fatalf("invalid body status: %q", snapshot.CaptureStatus)
			}
		})
	}
}
