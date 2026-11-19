package muse

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// toolFailure is a media tool call that reported an error in the thread
// history, for example
//
//	{"status":"error","error":"upstream_unavailable","attempts":3,
//	 "failure_scope":"upstream","upstream_status":502,"retry_exhausted":true}
type toolFailure struct {
	code      string
	scope     string
	upstream  int
	attempts  int
	exhausted bool
	refusal   bool
}

var policyCodes = []string{"policy", "safety", "moderation", "blocked", "rejected", "refus"}

// Require an affirmative rejection, not a quoted policy name in an input.
var mediaPolicyRe = regexp.MustCompile(`(?i)(?:would violate|will violate|(?:request|prompt|content) violates)\s+(?:(?:a|the|our)\s+)?(?:safety(?:\s+or\s+content)?|content)\s+polic(?:y|ies)|(?:request|prompt|content)\s+(?:was|is|has been)\s+(?:blocked|rejected)\s+(?:by|due to)\s+(?:the\s+)?(?:content|safety)\s+policy|(?:请求|内容|提示词)(?:已|会)?(?:违反|触犯)(?:了)?(?:内容|安全)(?:政策|策略)|未通过(?:内容|安全)审核`)

// error turns the failure into an SDK error whose message carries the
// tool's error code and upstream status.
func (f *toolFailure) error(kind string) error {
	code := f.code
	if code == "" {
		code = "error"
	}
	var details []string
	if f.upstream > 0 {
		details = append(details, fmt.Sprintf("上游 HTTP %d", f.upstream))
	}
	if f.scope != "" && f.upstream == 0 {
		details = append(details, "范围 "+f.scope)
	}
	if f.attempts > 1 {
		details = append(details, fmt.Sprintf("已尝试 %d 次", f.attempts))
	}
	msg := fmt.Sprintf("%s生成失败：%s", kindName(kind), code)
	if len(details) > 0 {
		msg += "（" + strings.Join(details, "，") + "）"
	}

	lower := strings.ToLower(code)
	switch {
	case f.refusal:
		return errf(KindRefusal, "%s", msg)
	case lower == "integrity_check_failed":
		// This is a definite tool rejection, not a temporary rate limit. The
		// code alone does not establish whether content policy was the cause.
		return &NoRetry{Err: &exhaustedToolError{Err: errf(KindTransient, "%s", msg)}}
	case strings.Contains(lower, "quota") || strings.Contains(lower, "credit") || strings.Contains(lower, "video_daily_budget_exhausted"):
		return &rejectedToolError{Err: errf(KindQuota, "%s", msg)}
	case f.upstream == 429 || strings.Contains(lower, "rate_limit"):
		return &rejectedToolError{Err: errf(KindRateLimit, "%s", msg)}
	case quotaRe.MatchString(code):
		return &rejectedToolError{Err: errf(KindQuota, "%s", msg)}
	case f.upstream == 400 || f.upstream == 422 || containsAny(lower, policyCodes):
		return errf(KindBadRequest, "%s", msg)
	}
	// The tool has already retried upstream, so another attempt on a
	// different account would most likely fail the same way.
	return &NoRetry{Err: &exhaustedToolError{Err: errf(KindTransient, "%s", msg)}}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// findToolFailure returns the last tool error reported after the user's
// message, or nil. User and root assistant messages are skipped: their text
// is the conversation, not tool output.
func findToolFailure(events []map[string]any) *toolFailure {
	failures := map[string]*toolFailure{}
	var order []string
	for _, ev := range events {
		switch eventName(ev) {
		case "message.user":
			continue
		case "message.assistant":
			p, _ := ev["payload"].(map[string]any)
			if str(ev, "reply_to_message_id") == "" && str(p, "reply_to_message_id") == "" {
				continue
			}
		}
		key := toolEventID(ev)
		p, _ := ev["payload"].(map[string]any)
		active, completed := mediaLifecycle(p)
		if active || completed {
			delete(failures, key)
		}
		walkToolFailures(ev, 0, func(f *toolFailure) { failures[key] = f; order = append(order, key) })
	}
	for i := len(order) - 1; i >= 0; i-- {
		if f := failures[order[i]]; f != nil {
			return f
		}
	}
	return nil
}

func toolEventID(ev map[string]any) string {
	p, _ := ev["payload"].(map[string]any)
	for _, field := range []string{"tool_call_id", "call_id", "message_id", "id"} {
		id := str(ev, field)
		if id == "" {
			id = str(p, field)
		}
		if id != "" {
			return "id:" + id
		}
	}
	return "type:" + eventName(ev)
}

const maxToolFailureDepth = 10

func walkToolFailures(v any, depth int, report func(*toolFailure)) {
	if depth > maxToolFailureDepth {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if f := toolFailureOf(t); f != nil {
			report(f)
			return
		}
		for _, x := range t {
			walkToolFailures(x, depth+1, report)
		}
	case []any:
		for _, x := range t {
			walkToolFailures(x, depth+1, report)
		}
	case string:
		// Tool output is often a JSON document carried as a string.
		s := strings.TrimSpace(t)
		if len(s) > 1 && s[0] == '{' && s[len(s)-1] == '}' {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				walkToolFailures(parsed, depth+1, report)
			}
		}
	}
}

func toolFailureOf(m map[string]any) *toolFailure {
	upstream := intField(m, "upstream_status", "upstreamStatus", "downstream_status", "downstreamStatus")
	status := strings.ToLower(str(m, "status"))
	failed := status == "error" || status == "failed" || status == "failure"
	code := firstString(m, "error_code", "errorCode", "code", "kind")
	if code == "" {
		code = errorCode(m["error"])
	}
	if !(failed && code != "") && upstream < 400 && code != "video_daily_budget_exhausted" {
		return nil
	}
	detail := firstString(m, "message", "reason")
	if e, ok := m["error"].(map[string]any); ok {
		detail += " " + firstString(e, "message", "reason")
	} else if message, ok := m["error"].(string); ok {
		detail += " " + message
	}
	return &toolFailure{
		code:      clip(code, 120),
		scope:     clip(firstString(m, "failure_scope", "failureScope"), 40),
		upstream:  upstream,
		attempts:  intField(m, "attempts", "attempt_count"),
		exhausted: m["retry_exhausted"] == true || m["retryExhausted"] == true || code == "video_daily_budget_exhausted" || code == "integrity_check_failed",
		refusal:   code == "integrity_check_failed" && mediaPolicyRe.MatchString(detail),
	}
}

func errorCode(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		if code := firstString(t, "code", "type", "kind"); code != "" {
			return code
		}
		return firstString(t, "message")
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func intField(m map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int(v)
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return 0
}
