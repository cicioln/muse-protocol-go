package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// VideoShot describes one planned shot in a multi-segment video storyboard.
type VideoShot struct {
	Seconds            int    `json:"seconds"`
	ContinuousFromPrev bool   `json:"continuous_from_prev"`
	Prompt             string `json:"prompt"`
}

// StoryboardRequest asks Muse to plan a multi-segment video storyboard.
type StoryboardRequest struct {
	Prompt         string
	MaxSeconds     int        // tier ceiling (e.g. 20 for muse-video-20s, 60 for muse-video-60s)
	TargetSeconds  int        // default total duration when prompt does not specify custom shot times
	SegmentSeconds int        // max seconds per segment; defaults to MaxVideoSegmentSeconds (10)
	Refs           []RefImage // optional reference images
	HasFirstFrame  bool
	HasLastFrame   bool
	AspectRatio    string
	CameraMovement string
	Style          string
	NegativePrompt string
	Timeout        time.Duration // defaults to 45s
}

const defaultStoryboardTimeout = 45 * time.Second

// PlanStoryboard asks Muse (in a fast text-only director turn) to break a
// video prompt into a sequence of timed shots with per-shot durations,
// transition continuity flags, and visual prompts.
func (e *ProtoEngine) PlanStoryboard(ctx context.Context, acc *Account, r StoryboardRequest) ([]VideoShot, error) {
	prompt := strings.TrimSpace(r.Prompt)
	if prompt == "" {
		return nil, BadRequest("prompt is required")
	}
	if err := validateRequest(prompt, r.Refs); err != nil {
		return nil, err
	}
	segMax := r.SegmentSeconds
	if segMax <= 0 || segMax > MaxVideoSegmentSeconds {
		segMax = MaxVideoSegmentSeconds
	}
	maxSec := r.MaxSeconds
	if maxSec <= 0 {
		maxSec = r.TargetSeconds
	}
	if maxSec <= 0 {
		maxSec = 20
	}
	if maxSec > MaxLongVideoSeconds {
		maxSec = MaxLongVideoSeconds
	}
	targetSec := r.TargetSeconds
	if targetSec <= 0 || targetSec > maxSec {
		targetSec = maxSec
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = defaultStoryboardTimeout
	}

	a, err := e.acquire(ctx, acc)
	if err != nil {
		return nil, err
	}
	defer e.release(acc.ID, a)

	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	directorPrompt := buildStoryboardPrompt(r, maxSec, targetSec, segMax)
	turn, err := e.runTurn(pctx, a, directorPrompt, r.Refs, "", "deny", nil, nil)
	e.persist(acc.ID, a.protoAcct, nil)
	if err != nil {
		return nil, err
	}
	if err := classifyChatNotice(turn.text); err != nil {
		return nil, err
	}
	shots, err := parseStoryboard(turn.text, maxSec, segMax)
	if err != nil {
		return nil, wrap(KindTransient, "解析分镜规划失败", err)
	}
	return shots, nil
}

func buildStoryboardPrompt(r StoryboardRequest, maxSec, targetSec, segMax int) string {
	var b strings.Builder
	b.WriteString("You are a professional AI film director planning the shot-by-shot storyboard for a multi-segment video.\n")
	b.WriteString("IMPORTANT: Do NOT call any image generation, video generation, web search, or shell tools. Reply ONLY with a raw JSON array of shot objects.\n\n")
	fmt.Fprintf(&b, "Constraints:\n- Maximum allowed total video duration: %d seconds.\n", maxSec)
	fmt.Fprintf(&b, "- Each shot object in the JSON array represents one generation segment and MUST have \"seconds\" between 2 and %d.\n", segMax)
	fmt.Fprintf(&b, "- Default target total duration (ONLY when the user does not specify custom shot times/durations): %d seconds.\n", targetSec)
	b.WriteString(`
Rules:
1. Custom Time Ranges & Durations:
   - If the user's description explicitly specifies shot time ranges or durations (for example: "1-7s ... 7-15s ...", "0-7s ... 7-15s ...", "1-7秒 ... 8-15秒 ...", "前7秒...后8秒...", "镜头1(5s)...镜头2(8s)..."), you MUST strictly follow the user's per-shot durations and actual total duration.
   - Note on 1-based time ranges: when a user writes "1-7s" followed by "7-15s" (or "1-7s" and "8-15s"), Shot 1 is 7 seconds (0s to 7s) and Shot 2 is 8 seconds (7s to 15s), totaling 15 seconds. Do NOT pad or stretch a 15-second user storyboard to fill the maximum ceiling.
   - If a user-specified shot is longer than the per-shot maximum (` + fmt.Sprintf("%d", segMax) + `s), split that shot into consecutive sub-shots of at most ` + fmt.Sprintf("%d", segMax) + `s with "continuous_from_prev": true.
2. General Descriptions (No Explicit Timestamps):
   - When the user writes a general story or scene description without explicit shot durations, break the narrative into a cohesive sequence of shots whose "seconds" sum to ` + fmt.Sprintf("%d", targetSec) + ` seconds. Pace the story across all shots so the first shot does NOT rush through the whole plot.
3. Continuity vs. Scene Cut ("continuous_from_prev"):
   - For the first shot (index 0), always set "continuous_from_prev": false.
   - For any subsequent shot (index > 0):
     * Set "continuous_from_prev": true if it continues smoothly in the same scene and camera flow from the end of the previous shot (the system will attach the previous shot's final frame as this shot's opening frame).
     * Set "continuous_from_prev": false if it is a hard cut to a new scene, a different location, or a distinct camera angle where starting from the previous shot's last frame would lock or distort the composition.
4. Per-Shot Prompt ("prompt"):
   - Write a rich, self-contained visual prompt for each shot in the same language as the user's description.
   - Describe the subject appearance, concrete action that starts and completes naturally within that shot's exact "seconds" duration, camera framing/movement, lighting, and a clean ending pose/composition.
   - Do NOT include timestamps of other shots inside a shot's "prompt".
`)
	if r.HasFirstFrame && r.HasLastFrame {
		b.WriteString("\nAttached reference images: Image 1 is the opening frame of the first shot; Image 2 is the final frame of the last shot; any remaining images are character/style references.\n")
	} else if r.HasFirstFrame {
		b.WriteString("\nAttached reference images: Image 1 is the opening frame of the first shot; any remaining images are character/style references.\n")
	} else if len(r.Refs) > 0 {
		b.WriteString("\nAttached reference images are references for characters, objects, and visual style.\n")
	}
	if r.AspectRatio != "" {
		b.WriteString("Aspect ratio: " + r.AspectRatio + "\n")
	}
	if r.CameraMovement != "" {
		b.WriteString("Preferred camera movement: " + r.CameraMovement + "\n")
	}
	if r.Style != "" {
		b.WriteString("Visual style: " + r.Style + "\n")
	}
	if r.NegativePrompt != "" {
		b.WriteString("Avoid: " + r.NegativePrompt + "\n")
	}
	b.WriteString(`
Output format (raw JSON array only, no markdown commentary):
[
  {
    "seconds": 7,
    "continuous_from_prev": false,
    "prompt": "..."
  },
  {
    "seconds": 8,
    "continuous_from_prev": true,
    "prompt": "..."
  }
]

User video description:
`)
	b.WriteString(r.Prompt)
	return b.String()
}

func parseStoryboard(text string, maxSec, segMax int) ([]VideoShot, error) {
	if segMax <= 0 || segMax > MaxVideoSegmentSeconds {
		segMax = MaxVideoSegmentSeconds
	}
	raw, err := extractStoryboardJSON(text)
	if err != nil {
		return nil, err
	}
	var parsed []VideoShot
	if err := json.Unmarshal(raw, &parsed); err != nil {
		var wrapper struct {
			Shots      []VideoShot `json:"shots"`
			Storyboard []VideoShot `json:"storyboard"`
		}
		if err2 := json.Unmarshal(raw, &wrapper); err2 != nil {
			return nil, err
		}
		if len(wrapper.Shots) > 0 {
			parsed = wrapper.Shots
		} else {
			parsed = wrapper.Storyboard
		}
	}
	var out []VideoShot
	total := 0
	for _, s := range parsed {
		p := strings.TrimSpace(s.Prompt)
		if p == "" || s.Seconds <= 0 {
			continue
		}
		remaining := maxSec - total
		if remaining <= 0 {
			break
		}
		secs := min(s.Seconds, remaining)
		if secs <= segMax {
			out = append(out, VideoShot{
				Seconds:            secs,
				ContinuousFromPrev: s.ContinuousFromPrev,
				Prompt:             p,
			})
			total += secs
			continue
		}
		parts := SplitVideoSeconds(secs, segMax)
		for i, partSec := range parts {
			cont := s.ContinuousFromPrev
			if i > 0 {
				cont = true
			}
			out = append(out, VideoShot{
				Seconds:            partSec,
				ContinuousFromPrev: cont,
				Prompt:             p,
			})
			total += partSec
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty storyboard")
	}
	out[0].ContinuousFromPrev = false
	return out, nil
}

func extractStoryboardJSON(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	if start := strings.IndexByte(text, '['); start >= 0 {
		if end := strings.LastIndexByte(text, ']'); end > start {
			candidate := text[start : end+1]
			if json.Valid([]byte(candidate)) {
				return []byte(candidate), nil
			}
		}
	}
	if start := strings.IndexByte(text, '{'); start >= 0 {
		if end := strings.LastIndexByte(text, '}'); end > start {
			candidate := text[start : end+1]
			if json.Valid([]byte(candidate)) {
				return []byte(candidate), nil
			}
		}
	}
	return nil, fmt.Errorf("no valid JSON found in response")
}
