package muse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/CoolkHz/muse-protocol-go/hatch"
)

var mediaActivityPollInterval = 5 * time.Second

// Activity reports expose tool outcomes omitted from /chat/history. Both the
// thread agent and execution ID must match: accounts can render concurrently,
// and one thread can have multiple turns. Outer goal success is not media success.
type mediaActivityTracker struct {
	agentID, executionID, activityID string
	nextPoll                         time.Time
	active                           bool
	failure                          *toolFailure
}

func (s *mediaActivityTracker) observe(events []map[string]any, sessionID string) {
	var agent, execution string
	for _, ev := range events {
		if eventName(ev) != "message.assistant" {
			continue
		}
		p, _ := ev["payload"].(map[string]any)
		if str(ev, "reply_to_message_id") != "" || str(p, "reply_to_message_id") != "" {
			continue
		}
		if sid := str(p, "session_id"); sid != "" && sid != sessionID {
			continue
		}
		md, _ := p["metadata"].(map[string]any)
		nextExecution := str(md, "root_message_execution_id")
		nextAgent := str(ev, "parent_agent_id")
		if nextAgent == "" {
			nextAgent = str(p, "parent_agent_id")
		}
		if nextExecution != "" && nextAgent != "" {
			agent, execution = nextAgent, nextExecution
		}
	}
	if execution != "" && (execution != s.executionID || agent != s.agentID) {
		*s = mediaActivityTracker{agentID: agent, executionID: execution}
	}
}

func (s *mediaActivityTracker) poll(ctx context.Context, conn *hatch.Conn) {
	if s.executionID == "" || time.Now().Before(s.nextPoll) {
		return
	}
	s.nextPoll = time.Now().Add(mediaActivityPollInterval)
	// Never reuse a failure after an unsuccessful refresh. Keep a known running
	// tool pending until a successful refresh or the caller's deadline.
	s.failure = nil
	path := "/activity?" + url.Values{"limit": {"30"}, "capabilities": {"ACTIVITY_FEED_GOALS"}}.Encode()
	if s.activityID != "" {
		path = "/activity/" + url.PathEscape(s.activityID)
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := conn.Do(readCtx, hatch.Request{Method: http.MethodGet, Path: path})
	if err != nil {
		return
	}
	if resp.Status == http.StatusNotFound {
		s.activityID = ""
	}
	if resp.Status != http.StatusOK {
		return
	}
	var data struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(resp.Body, &data) != nil || !data.OK {
		return
	}
	if s.activityID != "" {
		var activity mediaActivity
		if json.Unmarshal(data.Result, &activity) == nil {
			s.accept(activity)
		}
		return
	}
	var list struct {
		Days []struct {
			Activities []mediaActivity `json:"activities"`
		} `json:"days"`
	}
	if json.Unmarshal(data.Result, &list) != nil {
		return
	}
	for _, day := range list.Days {
		for _, activity := range day.Activities {
			if s.accept(activity) {
				return
			}
		}
	}
}

type mediaActivity struct {
	ID      string `json:"message_id"`
	Details struct {
		AgentID string `json:"activity_thread_agent_id"`
		Actions []struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Status string `json:"status"`
			Report string `json:"report"`
		} `json:"goal_actions"`
	} `json:"details"`
}

var (
	videoBudgetCodeRe       = regexp.MustCompile(`\bvideo_daily_budget_exhausted\b`)
	activityReportFieldRe   = regexp.MustCompile(`(?im)^[ \t]*(?:[-*][ \t]+)?(?:\*\*)?([a-z][a-z _-]*)(?:\*\*)?[ \t]*:(?:\*\*)?[ \t]*`)
	integrityErrorCodeRe    = regexp.MustCompile("^[`*\"' \\t]*integrity_check_failed\\b")
	mediaIntegritySummaryRe = regexp.MustCompile("(?m)^The `media\\.generate_video` tool failed with `integrity_check_failed`(?:[ .(]|$)")
)

func mediaActivityFailure(report string) *toolFailure {
	// The report includes the original prompt. Read the labeled tool/error/result
	// fields so a quoted error or policy name in that prompt is not authoritative.
	var tool, detail string
	integrity := false
	fields := activityReportFieldRe.FindAllStringSubmatchIndex(report, -1)
	summary := report
	if len(fields) > 0 {
		summary = report[:fields[0][0]]
	}
	// Muse also emits a prose summary ahead of the labeled sections. Restrict
	// this fallback to that preamble so quoted text inside Prompt is excluded.
	if mediaIntegritySummaryRe.MatchString(strings.TrimSpace(summary)) {
		tool, integrity = "media.generate_video", true
	}
	for i, field := range fields {
		end := len(report)
		if i+1 < len(fields) {
			end = fields[i+1][0]
		}
		value := strings.TrimSpace(report[field[1]:end])
		switch strings.ToLower(strings.TrimSpace(report[field[2]:field[3]])) {
		case "tool":
			tool = strings.Trim(value, "`* \t\r\n")
		case "error", "error kind", "error code", "kind", "code":
			integrity = integrity || integrityErrorCodeRe.MatchString(value)
		case "result", "reason", "error detail":
			detail += " " + value
		}
	}
	if tool == "media.generate_video" && integrity {
		return &toolFailure{code: "integrity_check_failed", exhausted: true, refusal: mediaPolicyRe.MatchString(detail)}
	}
	if videoBudgetCodeRe.MatchString(report) {
		return &toolFailure{code: "video_daily_budget_exhausted", exhausted: true}
	}
	return nil
}

func (s *mediaActivityTracker) accept(activity mediaActivity) bool {
	if activity.Details.AgentID != s.agentID {
		return false
	}
	prefix := "ga-root-tool-" + s.executionID + "-call_"
	matched, active := false, false
	var failure *toolFailure
	for _, action := range activity.Details.Actions {
		if action.Kind != "tool_call" || !strings.HasPrefix(action.ID, prefix) {
			continue
		}
		matched = true
		switch strings.ToLower(action.Status) {
		case "failed":
			// Reports are Markdown, not JSON. Only the explicit media rejection
			// code is authoritative; prose and successful tool reports may quote
			// unrelated errors or contain the input prompt.
			if f := mediaActivityFailure(action.Report); f != nil {
				if failure == nil || !failure.refusal {
					failure = f
				}
			}
		case "success", "cancelled", "canceled":
		default:
			active = true // Unknown states are not proof of completion.
		}
	}
	if !matched {
		return false
	}
	s.activityID, s.active, s.failure = activity.ID, active, failure
	return true
}

func assistantCommentary(p map[string]any) bool {
	md, _ := p["metadata"].(map[string]any)
	return md["public_commentary"] == true || str(md, "message_kind") == "commentary"
}
