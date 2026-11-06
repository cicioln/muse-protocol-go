package muse

import (
	"errors"
	"strings"
	"testing"
)

func TestFindToolFailureReadsUpstreamStatus(t *testing.T) {
	events := []map[string]any{
		{"event_name": "message.user", "payload": map[string]any{"text": `{"status":"error","error":"from_user","upstream_status":500}`}},
		{"event_name": "tool.result", "payload": map[string]any{
			"tool_call_id": "c1",
			"output":       `{"status":"error","error":"upstream_unavailable","attempts":3,"failure_scope":"upstream","upstream_status":502,"retry_exhausted":true}`,
		}},
		{"event_name": "message.assistant", "payload": map[string]any{"text": "The video tool failed."}},
	}
	f := findToolFailure(events)
	if f == nil || f.code != "upstream_unavailable" || f.upstream != 502 || f.attempts != 3 {
		t.Fatalf("failure = %+v", f)
	}
	err := f.error("video")
	if Retryable(err) {
		t.Fatal("exhausted upstream failure must not be retried")
	}
	if !strings.Contains(err.Error(), "upstream_unavailable") || !strings.Contains(err.Error(), "502") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestFindToolFailureIgnoresSuccessAndConversation(t *testing.T) {
	events := []map[string]any{
		{"event_name": "tool.result", "payload": map[string]any{"status": "success", "upstream_status": 200}},
		{"event_name": "message.assistant", "payload": map[string]any{"text": `{"status":"error","error":"quoted"}`}},
	}
	if f := findToolFailure(events); f != nil {
		t.Fatalf("unexpected failure %+v", f)
	}
}

func TestToolFailureKinds(t *testing.T) {
	for _, tc := range []struct {
		f    toolFailure
		want Kind
	}{
		{toolFailure{code: "rate_limited", upstream: 429}, KindRateLimit},
		{toolFailure{code: "insufficient_quota"}, KindQuota},
		{toolFailure{code: "content_policy_violation"}, KindBadRequest},
		{toolFailure{code: "upstream_unavailable", upstream: 503}, KindTransient},
	} {
		err := tc.f.error("image")
		if KindOf(err) != tc.want {
			t.Errorf("%+v: kind %v, want %v", tc.f, KindOf(err), tc.want)
		}
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%+v: not a *muse.Error", tc.f)
		}
	}
}
