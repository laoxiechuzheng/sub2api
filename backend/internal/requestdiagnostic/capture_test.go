package requestdiagnostic_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/requestdiagnostic"
)

func TestCaptureBudgetPrefersInboundAndLatestAttempt(t *testing.T) {
	const maxBytes = 512
	body := []byte(`{"text":"` + strings.Repeat("x", 160) + `"}`)
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: maxBytes, MaxAttempts: 99}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound(body)
	var latest int64
	for i := 0; i < 20; i++ {
		latest = capture.BeginAttempt(requestdiagnostic.Attempt{}, body)
		capture.EndAttempt(latest, requestdiagnostic.AttemptResult{Status: 502, ResponseSummary: []byte(`{"error":"temporary"}`)})
	}
	capture.BindUsage(1, "usage-key", time.Now())
	capture.Finish(502)
	if snapshot == nil || snapshot.StoredBytes > maxBytes {
		t.Fatalf("budget exceeded: %+v", snapshot)
	}
	if len(snapshot.Attempts) != 16 || snapshot.DroppedAttempts != 4 || snapshot.Attempts[15].ID != latest {
		t.Fatalf("latest attempt not retained under attempt cap: len=%d dropped=%d", len(snapshot.Attempts), snapshot.DroppedAttempts)
	}
	if snapshot.Meta.Body.Content != string(body) || snapshot.Attempts[15].Body.Content != string(body) {
		t.Fatal("old attempts or summaries took space from inbound/latest body")
	}
	omittedOld := false
	for _, attempt := range snapshot.Attempts[:15] {
		if attempt.Body.Content == "" && attempt.Body.OmittedReason == requestdiagnostic.OmittedEvicted && attempt.Body.OriginalBytes == len(body) {
			omittedOld = true
		}
	}
	if !omittedOld || snapshot.CaptureStatus != requestdiagnostic.CaptureStatusPartial {
		t.Fatal("evicted older bodies must remain visible as explicit omissions")
	}
}

func TestCaptureStoredBytesCountsAllSavedMetadataAndHonorsSmallAttemptLimit(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: 1024, MaxAttempts: 2}, requestdiagnostic.Inbound{
		RequestID: "server", ClientRequestID: "client", Endpoint: "/v1/messages", Method: "POST", UserAgent: strings.Repeat("a", 300),
	}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound([]byte(`{"entry":true}`))
	for i := 0; i < 3; i++ {
		id := capture.BeginAttempt(requestdiagnostic.Attempt{Platform: "test", Model: "model", Endpoint: "/upstream", Method: "POST"}, []byte(`{"upstream":true}`))
		capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 502, Error: strings.Repeat("safe error ", 100), UpstreamRequestID: "up-id", ResponseSummary: []byte(`{"ok":true}`)})
	}
	capture.BindUsage(1, "canonical", time.Now())
	capture.Finish(502)
	if len(snapshot.Attempts) != 2 || snapshot.DroppedAttempts != 1 {
		t.Fatal("configured smaller attempt cap was ignored")
	}
	stored := len(snapshot.Binding.CanonicalID) + len(snapshot.Meta.RequestID) + len(snapshot.Meta.ClientRequestID) + len(snapshot.Meta.Endpoint) + len(snapshot.Meta.Method) + len(snapshot.Meta.UserAgent) + len(snapshot.Meta.Body.Content)
	for _, attempt := range snapshot.Attempts {
		stored += len(attempt.Platform) + len(attempt.Endpoint) + len(attempt.Method) + len(attempt.Model) + len(attempt.Error) + len(attempt.UpstreamRequestID) + len(attempt.Body.Content) + len(attempt.ResponseSummary.Content)
	}
	if snapshot.StoredBytes != stored || stored > 1024 || !snapshot.MetadataTruncated {
		t.Fatalf("stored bytes do not match saved data: counted=%d reported=%d", stored, snapshot.StoredBytes)
	}
}

func TestCaptureBudgetSplitsBetweenInboundAndLatestAndRemainsJSONMarshalable(t *testing.T) {
	body := []byte(`{"text":"` + strings.Repeat("你", 80) + `"}`)
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: 400, MaxAttempts: 2}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound(body)
	capture.BeginAttempt(requestdiagnostic.Attempt{}, body)
	capture.BindUsage(1, "usage", time.Now())
	capture.Finish(200)
	if snapshot == nil || snapshot.StoredBytes > 400 {
		t.Fatal("combined payload budget was not enforced")
	}
	for _, payload := range []requestdiagnostic.Payload{snapshot.Meta.Body, snapshot.Attempts[0].Body} {
		if !payload.Truncated || payload.Content == "" || payload.StoredBytes != len(payload.Content) || payload.OriginalBytes != len(body) {
			t.Fatalf("both priority bodies must get a bounded share: %+v", payload)
		}
		if !utf8.ValidString(payload.Content) {
			t.Fatal("truncation split a UTF-8 rune")
		}
	}
	if _, err := json.Marshal(snapshot); err != nil {
		t.Fatalf("truncated body cannot break outer JSON: %v", err)
	}
}

func TestCaptureOversizeBodiesKeepOnlyMetadata(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: 256}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	body := []byte(`{"text":"` + strings.Repeat("x", 300) + `"}`)
	capture.SetInbound(body)
	capture.BeginAttempt(requestdiagnostic.Attempt{}, body)
	capture.BindUsage(1, "usage", time.Now())
	capture.Finish(200)
	for _, payload := range []requestdiagnostic.Payload{snapshot.Meta.Body, snapshot.Attempts[0].Body} {
		if payload.Content != "" || payload.StoredBytes != 0 || payload.OriginalBytes != len(body) || payload.OmittedReason != requestdiagnostic.OmittedTooLarge {
			t.Fatal("oversize single body should be metadata-only, not parsed or copied")
		}
	}
}

func TestCaptureLifecycleDropsAndOmissionsNeverRetainContent(t *testing.T) {
	var ready, finished int
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{MaxBytes: 1024, OnFinish: func() { finished++ }}, requestdiagnostic.Inbound{
		Body: requestdiagnostic.Payload{Content: "caller raw content must not be trusted", StoredBytes: 38},
	}, func(s *requestdiagnostic.Snapshot) { ready++; snapshot = s })
	if capture.MaxBytes() != 1024 {
		t.Fatal("caller cannot bound its copy with the configured capture limit")
	}
	capture.SetInbound([]byte(`{"text":"sensitive prompt"}`))
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte(`{"text":"sensitive prompt"}`))
	capture.Drop("arbitrary raw reason with private-secret")
	capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 500, ResponseSummary: []byte(`{"error":"sensitive response"}`)})
	capture.BindUsage(1, "usage", time.Now())
	capture.Finish(500)
	if ready != 1 || finished != 1 || snapshot.CaptureStatus != requestdiagnostic.CaptureStatusDropped {
		t.Fatal("drop must keep normal lifecycle and explicit status")
	}
	for _, payload := range []requestdiagnostic.Payload{snapshot.Meta.Body, snapshot.Attempts[0].Body, snapshot.Attempts[0].ResponseSummary} {
		if payload.Content != "" || payload.StoredBytes != 0 || payload.OmittedReason != requestdiagnostic.OmittedDropped {
			t.Fatalf("drop did not clear content: %+v", payload)
		}
	}
	encoded, _ := json.Marshal(snapshot)
	if strings.Contains(string(encoded), "sensitive prompt") || strings.Contains(string(encoded), "private-secret") || strings.Contains(string(encoded), "caller raw") {
		t.Fatal("dropped raw content/reason was retained")
	}

	capture = requestdiagnostic.NewCapture(requestdiagnostic.Options{OnFinish: func() { finished++ }}, requestdiagnostic.Inbound{}, func(*requestdiagnostic.Snapshot) { ready++ })
	capture.SetInboundOmitted(9<<20, requestdiagnostic.OmittedTooLarge)
	capture.BeginAttemptOmitted(requestdiagnostic.Attempt{}, 9<<20, requestdiagnostic.OmittedTooLarge)
	capture.Finish(499)
	capture.Discard()
	capture.Discard()
	if capture.BindUsage(1, "usage", time.Now()) || ready != 1 || finished != 2 {
		t.Fatal("unbound discard must terminate callbacks and release exactly once")
	}
}

func TestCaptureRejectsInvalidAndConflictingCanonicalBindings(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	created := time.Now()
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{RequestID: "ops-uuid", ClientRequestID: "client-id"}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	for _, test := range []struct {
		apiKeyID int64
		id       string
		created  time.Time
	}{
		{0, "valid", created}, {1, "", created}, {1, "   ", created}, {1, "valid", time.Time{}},
		{1, "Bearer private-token", created}, {1, "sk-proj-abcdefghijklmn", created}, {1, "contains\nnewline", created},
		{1, strings.Repeat("x", 1025), created},
	} {
		if capture.BindUsage(test.apiKeyID, test.id, test.created) {
			t.Fatalf("invalid binding accepted: apiKeyID=%d", test.apiKeyID)
		}
	}
	capture.Finish(200)
	if snapshot != nil {
		t.Fatal("ops/client IDs must not substitute for a canonical usage key")
	}
	if !capture.BindUsage(1, "authoritative-key", created) || !capture.BindUsage(1, "authoritative-key", created) {
		t.Fatal("valid/idempotent canonical binding rejected")
	}
	if capture.BindUsage(2, "authoritative-key", created) || capture.BindUsage(1, "other-key", created) || capture.BindUsage(1, "authoritative-key", created.Add(time.Second)) {
		t.Fatal("conflicting binding was accepted")
	}
	if snapshot == nil || snapshot.Binding.CanonicalID != "authoritative-key" {
		t.Fatal("canonical key was inferred or rewritten")
	}
}

func TestCaptureResponseSummaryIsRedactedOnceAndCappedAt64KiB(t *testing.T) {
	var snapshot *requestdiagnostic.Snapshot
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{}, func(s *requestdiagnostic.Snapshot) { snapshot = s })
	capture.SetInbound([]byte(`{"ok":true}`))
	id := capture.BeginAttempt(requestdiagnostic.Attempt{}, []byte(`{"ok":true}`))
	summary := []byte(`{"error":{"message":"sk-proj-abcdefghijklmnop ` + strings.Repeat("你", 40_000) + `","token":"response-private"}}`)
	capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 502, ResponseSummary: summary})
	capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 200, ResponseSummary: []byte(`{"changed":true}`)})
	capture.BindUsage(1, "usage", time.Now())
	capture.Finish(502)
	payload := snapshot.Attempts[0].ResponseSummary
	if payload.StoredBytes > requestdiagnostic.MaxResponseSummaryBytes || !payload.Truncated || !payload.Redacted || payload.OriginalBytes != len(summary) || !utf8.ValidString(payload.Content) {
		t.Fatal("response summary is not a bounded redacted UTF-8 prefix")
	}
	if snapshot.Attempts[0].Status != 502 || strings.Contains(payload.Content, "sk-proj-") || strings.Contains(payload.Content, "response-private") || strings.Contains(payload.Content, "changed") {
		t.Fatal("duplicate EndAttempt changed the result or credentials leaked")
	}
	if _, err := json.Marshal(snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureKeepsLargeInboundAndUpstreamAndCanonicalBinding(t *testing.T) {
	startedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	text := strings.Repeat("a", 2_800_000)
	body := []byte(`{"messages":[{"role":"user","content":"` + text + `"}],"model":"test-model"}`)
	var snapshot *requestdiagnostic.Snapshot
	calls := 0
	capture := requestdiagnostic.NewCapture(requestdiagnostic.Options{}, requestdiagnostic.Inbound{
		RequestID:       "server-ops-uuid",
		ClientRequestID: "client-visible-id",
		Endpoint:        "/v1/messages",
		Method:          "POST",
		StartedAt:       startedAt,
		UserAgent:       "diagnostic-test/1.0",
	}, func(s *requestdiagnostic.Snapshot) {
		calls++
		snapshot = s
	})
	capture.SetInbound(body)
	id := capture.BeginAttempt(requestdiagnostic.Attempt{AccountID: 42, Platform: "test", Model: "test-model", Endpoint: "/v1/messages", Method: "POST"}, body)
	capture.EndAttempt(id, requestdiagnostic.AttemptResult{Status: 200, UpstreamRequestID: "upstream-id"})
	if !capture.BindUsage(7, "client:canonical-billing-id", startedAt) {
		t.Fatal("valid canonical binding rejected")
	}
	if calls != 0 {
		t.Fatal("binding alone must not publish a live capture")
	}
	capture.Finish(201)
	capture.Finish(500)
	capture.BindUsage(7, "client:canonical-billing-id", startedAt)
	if calls != 1 || snapshot == nil {
		t.Fatalf("ready callback calls = %d, want exactly one", calls)
	}
	if snapshot.SchemaVersion != 1 || snapshot.Status != 201 || snapshot.Binding.CanonicalID != "client:canonical-billing-id" || snapshot.Binding.APIKeyID != 7 {
		t.Fatalf("unexpected snapshot binding/status: %+v", snapshot.Binding)
	}
	if snapshot.Meta.RequestID != "server-ops-uuid" || snapshot.Meta.ClientRequestID != "client-visible-id" {
		t.Fatalf("server and client request IDs were conflated: %+v", snapshot.Meta)
	}
	if len(snapshot.Attempts) != 1 || snapshot.Attempts[0].ID != id || snapshot.Attempts[0].Status != 200 || snapshot.Attempts[0].UpstreamRequestID != "upstream-id" {
		t.Fatal("attempt metadata not preserved")
	}
	for _, payload := range []requestdiagnostic.Payload{snapshot.Meta.Body, snapshot.Attempts[0].Body} {
		if payload.Truncated || payload.OmittedReason != "" || payload.Redacted || payload.OriginalBytes != len(body) || payload.StoredBytes != len(payload.Content) {
			t.Fatalf("2.8MB ordinary JSON was not kept intact: original=%d stored=%d truncated=%v omitted=%q redacted=%v", payload.OriginalBytes, payload.StoredBytes, payload.Truncated, payload.OmittedReason, payload.Redacted)
		}
		var decoded struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		if err := json.Unmarshal([]byte(payload.Content), &decoded); err != nil {
			t.Fatalf("stored body is not JSON: %v", err)
		}
		if len(decoded.Messages) != 1 || decoded.Messages[0].Content != text {
			t.Fatal("ordinary long text changed")
		}
	}
	if snapshot.StoredBytes > requestdiagnostic.DefaultMaxBytes || snapshot.CaptureStatus != "captured" {
		t.Fatalf("unexpected budget/status: bytes=%d status=%q", snapshot.StoredBytes, snapshot.CaptureStatus)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("snapshot marshal: %v", err)
	}
	for _, field := range []string{`"schema_version":1`, `"api_key_id":7`, `"usage_request_id":"client:canonical-billing-id"`, `"usage_created_at":`, `"request_id":"server-ops-uuid"`, `"client_request_id":"client-visible-id"`, `"content":"{\"messages\":`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("missing JSON contract field %s", field)
		}
	}
}
