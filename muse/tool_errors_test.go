package muse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/CoolkHz/muse-protocol-go/internal/fakemuse"
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
		{toolFailure{code: "insufficient_quota", upstream: 429}, KindQuota},
		{toolFailure{code: "video_daily_budget_exhausted", upstream: 429}, KindQuota},
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

func TestVideoDailyBudgetFailureCanSwitchAccounts(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprintf("resume=%v", resume), func(t *testing.T) {
			site, gw := fakemuse.NewWithGateway()
			defer site.Close()
			gw.HistoryResponse = func(string) ([]byte, bool) {
				return []byte(`{"chat_events":[{"event_name":"message.user","message_id":"anchor","text":"create video"},{"event_name":"tool.result","payload":{"call_id":"render","output":"{\"status\":\"error\",\"kind\":\"video_daily_budget_exhausted\",\"downstream_status\":429}"}}]}`), true
			}
			e, account := newTestProto(t, site.URL, "ws"+strings.TrimPrefix(site.URL, "http")+"/v1/noise")
			oldSettle, oldPoll := textSettle, pollInterval
			textSettle, pollInterval = 5*time.Millisecond, 5*time.Millisecond
			defer func() { textSettle, pollInterval = oldSettle, oldPoll }()
			state := &GenerationState{}
			if resume {
				state.SessionID, state.MessageID = "failure", "anchor"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			files, err := e.Generate(ctx, account, GenRequest{Prompt: "create video", Kind: "video", State: state})
			if len(files) != 0 || err == nil || KindOf(err) != KindQuota || !Retryable(err) {
				t.Fatalf("expected quota rejection safe for another account, files=%d err=%v", len(files), err)
			}
			if Retryable(segmentError(err, 1, 3)) {
				t.Fatal("already generated segments must not be replayed on another account")
			}
		})
	}
	for _, body := range []string{
		`{"status":"error","kind":"video_daily_budget_exhausted","error":"This account has reached its video limit for the last 24 hours"}`,
		`{"status":"error","error":{"kind":"video_daily_budget_exhausted"}}`,
	} {
		f := findToolFailure([]map[string]any{{"event_name": "tool.result", "payload": map[string]any{"output": body}}})
		if f == nil || KindOf(f.error("video")) != KindQuota {
			t.Fatalf("quota payload was not recognized: %s", body)
		}
	}
	if Retryable(turnFailure(errf(KindQuota, "polling failed without a tool rejection"))) {
		t.Fatal("uncertain polling failures must remain non-retryable")
	}
	text := "Media generation failed. Result: kind: video_daily_budget_exhausted — downstream status 429. This account has reached its video limit for the last 24 hours. No video file was produced in this attempt."
	if err := classifyTurnErr(text); err == nil || KindOf(err) != KindQuota || !Retryable(err) {
		t.Fatalf("media budget report was not recognized: %v", err)
	}
	if err := classifyChatNotice(text); err != nil {
		t.Fatal("media-only classification must not change ordinary chat")
	}
	if KindOf(classifyHTTPError("/chat/stream", 429, `{"kind":"video_daily_budget_exhausted"}`, nil)) != KindQuota {
		t.Fatal("daily video budget HTTP rejection was classified as temporary throttling")
	}
}

func TestToolRecoveryClearsEarlierFailure(t *testing.T) {
	events := []map[string]any{
		{"event_name": "tool.result", "payload": map[string]any{"tool_call_id": "one", "status": "error", "error": "temporary", "retry_exhausted": true}},
		{"event_name": "tool.result", "payload": map[string]any{"call_id": "one", "status": "success"}},
	}
	if f := findToolFailure(events); f != nil {
		t.Fatalf("stale failure survived recovery: %+v", f)
	}
}
