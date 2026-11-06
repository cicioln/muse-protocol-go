package muse

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// toolFailure is a media tool call that reported an error in the thread
// history, for example
//
//	{"status":"error","error":"upstream_unavailable","attempts":3,
//	 "failure_scope":"upstream","upstream_status":502,"retry_exhausted":true}
type toolFailure struct {
	code     string
	scope    string
	upstream int
	attempts int
}

var policyCodes = []string{"policy", "safety", "moderation", "blocked", "rejected", "refus"}

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
	case f.upstream == 429 || strings.Contains(lower, "rate_limit"):
		return errf(KindRateLimit, "%s", msg)
	case quotaRe.MatchString(code) || strings.Contains(lower, "quota") || strings.Contains(lower, "credit"):
		return errf(KindQuota, "%s", msg)
	case f.upstream == 400 || f.upstream == 422 || containsAny(lower, policyCodes):
		return errf(KindBadRequest, "%s", msg)
	}
	// The tool has already retried upstream, so another attempt on a
	// different account would most likely fail the same way.
	return &NoRetry{Err: errf(KindTransient, "%s", msg)}
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
	var found *toolFailure
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
		walkToolFailures(ev, 0, func(f *toolFailure) { found = f })
	}
	return found
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
	upstream := intField(m, "upstream_status", "upstreamStatus")
	status := strings.ToLower(str(m, "status"))
	failed := status == "error" || status == "failed" || status == "failure"
	code := errorCode(m["error"])
	if code == "" {
		code = firstString(m, "error_code", "errorCode", "code")
	}
	if !(failed && code != "") && upstream < 400 {
		return nil
	}
	return &toolFailure{
		code:     clip(code, 120),
		scope:    clip(firstString(m, "failure_scope", "failureScope"), 40),
		upstream: upstream,
		attempts: intField(m, "attempts", "attempt_count"),
	}
}

func errorCode(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case map[string]any:
		if code := firstString(t, "code", "type"); code != "" {
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
