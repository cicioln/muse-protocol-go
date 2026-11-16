package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
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
	// Background is story context (setting, world, scenes) shown to the
	// director only; it is not copied into the shot prompts.
	Background string
	// Characters is a fixed character description from the caller. When
	// set, the director refers to characters by its labels instead of
	// writing its own, and this text is put in every shot's prompt.
	Characters string
	Timeout    time.Duration // defaults to 45s
}

const defaultStoryboardTimeout = 45 * time.Second

// PlanStoryboard asks Muse (in a fast text-only director turn) to break a
// video prompt into a sequence of timed shots with per-shot durations,
// transition continuity flags, and visual prompts. When the plan describes
// recurring characters, that description is put word for word at the start
// of every shot's prompt: rewording a character between segments is the
// main cause of the face or clothes changing from one segment to the next.
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
	shots, characters, err := parseStoryboard(turn.text, maxSec, segMax)
	if err != nil {
		return nil, wrap(KindTransient, "解析分镜规划失败", err)
	}
	if c := strings.TrimSpace(r.Characters); c != "" {
		characters = c
	}
	shots = finalizeStoryboard(r.Prompt, shots, maxSec, targetSec, segMax)
	return withCharacters(shots, characters), nil
}

// withCharacters puts the shared character description at the start of
// every shot's prompt. It runs after the shots are merged and expanded, so
// each prompt gets the description exactly once.
func withCharacters(shots []VideoShot, characters string) []VideoShot {
	characters = strings.TrimSpace(characters)
	if characters == "" {
		return shots
	}
	for i := range shots {
		shots[i].Prompt = "Recurring characters and places (keep their appearance exactly as described here in this shot):\n" + characters + "\n\nShot:\n" + shots[i].Prompt
	}
	return shots
}

func maxPreferredShots(total, segMax int) int {
	if segMax <= 0 {
		segMax = MaxVideoSegmentSeconds
	}
	minNeed := (total + segMax - 1) / segMax
	// Prefer longer cohesive clips (~6-10s) rather than choppy 2-3s fragments.
	pref := (total + 5) / 6
	if pref < minNeed {
		pref = minNeed
	}
	if total > segMax && pref < 2 {
		pref = 2
	}
	if pref < 1 {
		pref = 1
	}
	return pref
}

func formatStoryboardExample(targetSec, segMax int) string {
	parts := SplitVideoSeconds(targetSec, segMax)
	var b strings.Builder
	b.WriteString("{\n  \"characters\": \"...\",\n  \"shots\": [\n")
	for i, sec := range parts {
		cont := i > 0
		fmt.Fprintf(&b, "    {\n      \"seconds\": %d,\n      \"continuous_from_prev\": %t,\n      \"prompt\": \"...\"\n    }", sec, cont)
		if i < len(parts)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("  ]\n}")
	return b.String()
}

func buildStoryboardPrompt(r StoryboardRequest, maxSec, targetSec, segMax int) string {
	minShot := 4
	if targetSec < minShot {
		minShot = max(targetSec, 1)
	}
	prefMin := min(5, segMax)
	maxShots := maxPreferredShots(targetSec, segMax)

	var b strings.Builder
	b.WriteString("You are a professional AI film director planning the shot-by-shot storyboard for a multi-segment video.\n")
	b.WriteString("IMPORTANT: Do NOT call any image generation, video generation, web search, or shell tools. Reply ONLY with one raw JSON object holding \"characters\" and \"shots\".\n\n")
	fmt.Fprintf(&b, "Constraints:\n- Maximum allowed total video duration: %d seconds.\n", maxSec)
	fmt.Fprintf(&b, "- Required default total duration: %d seconds (keep the total at %d seconds unless the user explicitly writes custom shot timestamps or a specific shorter total duration).\n", targetSec, targetSec)
	fmt.Fprintf(&b, "- Each shot object represents ONE video generation call and MUST have \"seconds\" between %d and %d (strongly prefer %d to %d seconds per shot for smooth motion and high reliability).\n", minShot, segMax, prefMin, segMax)
	fmt.Fprintf(&b, "- Avoid chopping the video into short 2-3 second clips. Keep the storyboard compact (at most %d shots unless the user explicitly specifies more timestamped cuts).\n", maxShots)
	b.WriteString(`
Rules:
1. Custom Time Ranges & Durations:
   - ONLY when the user's description explicitly writes shot time ranges or custom durations (for example: timecoded ranges "A-B s ... B-C s ...", "前X秒...后Y秒...", "镜头1(Xs)...镜头2(Ys)...", or an explicit total video length), strictly follow the user's per-shot durations and custom total duration (capped at ` + fmt.Sprintf("%d", maxSec) + `s).
   - Note on 1-based time ranges: when a user writes "1-A s" followed by "A-B s" (or "1-A s" and "(A+1)-B s"), Shot 1 lasts A seconds (0s to A) and Shot 2 lasts (B-A) seconds (A to B). Do NOT stretch a user-specified shorter storyboard beyond what they asked for.
   - If a user-specified shot exceeds ` + fmt.Sprintf("%d", segMax) + `s, split it into consecutive sub-shots of at most ` + fmt.Sprintf("%d", segMax) + `s with "continuous_from_prev": true.
2. General Descriptions (No Explicit Timestamps):
   - When the user writes a general story or scene description without explicit shot durations or timecodes, break the narrative into a cohesive sequence of long shots (typically ` + fmt.Sprintf("%d-%d", prefMin, segMax) + `s each, at most ` + fmt.Sprintf("%d", maxShots) + ` shots total) whose "seconds" MUST sum to EXACTLY ` + fmt.Sprintf("%d", targetSec) + ` seconds. Pace the story evenly without cutting into short 2-3s pieces.
3. Continuity vs. Scene Cut ("continuous_from_prev"):
   - For the first shot (index 0), always set "continuous_from_prev": false.
   - For any subsequent shot (index > 0):
     * Set "continuous_from_prev": true if it continues smoothly in the same scene and camera flow from the end of the previous shot (the system will attach the previous shot's final frame as this shot's opening frame).
     * Set "continuous_from_prev": false if it is a hard cut to a new scene, a different location, or a distinct camera angle where starting from the previous shot's last frame would lock or distort the composition.
4. Characters ("characters"):
   - Each shot is generated in a separate call, and the system puts this exact text at the start of every shot's prompt, so the characters must be fully defined here once.
   - For every person, animal or creature that appears in more than one shot, give a short label and a precise, fixed description: age, gender, face and build, hairstyle and hair color, every piece of clothing with its exact color and style, and accessories. When attached reference images show a character, describe them as they look in the images.
   - Write it in the same language as the user's description. Use "" when no character appears in more than one shot.
5. Per-Shot Prompt ("prompt"):
   - Write a rich visual prompt for each shot in the same language as the user's description.
   - Refer to characters only by their labels from "characters". Do NOT restate, reword or change their appearance or clothing inside a shot's prompt.
   - Describe the concrete action that starts and completes naturally within that shot's exact "seconds" duration, the setting, camera framing/movement, lighting, and a clean ending pose/composition.
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
		b.WriteString("Aspect ratio: exactly " + r.AspectRatio + "\n")
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
	if bg := strings.TrimSpace(r.Background); bg != "" {
		b.WriteString("\nStory background (context for the whole film; keep every shot consistent with it):\n" + bg + "\n")
	}
	if c := strings.TrimSpace(r.Characters); c != "" {
		b.WriteString("\nFixed characters and places (already defined by the user and added to every shot by the system; set \"characters\" to \"\" and refer to them only by these labels):\n" + c + "\n")
	}
	fmt.Fprintf(&b, "\nOutput format (raw JSON object only, no markdown commentary; example matching %ds total):\n%s\n\nUser video description:\n", targetSec, formatStoryboardExample(targetSec, segMax))
	b.WriteString(r.Prompt)
	return b.String()
}

var (
	shotTimeRangeRe  = regexp.MustCompile(`(?i)(?:^|[^a-zA-Z0-9.])\d+(?:\.\d+)?\s*(?:s|sec|secs|second|seconds|秒)?\s*[-~～到至]\s*\d+(?:\.\d+)?\s*(?:秒|(?:s|sec|secs|second|seconds)(?:$|[^a-zA-Z0-9]))`)
	shotClockRangeRe = regexp.MustCompile(`\b\d{1,2}:\d{2}\s*[-~～到至]\s*\d{1,2}:\d{2}\b`)
	explicitTotalRe  = regexp.MustCompile(`(?i)(?:总时长|总计|片长|总长度|total\s+(?:duration|length)|duration\s*[:=]|length\s*[:=])\s*\d+\s*(?:s|sec|secs|second|seconds|秒)|\d+\s*[-\s]?(?:second|sec|s|秒)\s*(?:的)?(?:视频|短片|成片|动画|video|clip|film|reel|storyboard)`)
	shotDurationRe   = regexp.MustCompile(`(?i)(?:(?:镜头|分镜|片段|场景|第\s*\d+\s*[段个幕节镜]|shot|scene|part|segment|clip|前|后|最后|末尾|开头|结尾)\s*\d*\s*[（(\[:：]?\s*\d+\s*(?:s|sec|secs|second|seconds|秒)|\[\s*\d+\s*(?:s|sec|secs|second|seconds|秒)\s*\]|\(\s*\d+\s*(?:s|sec|secs|second|seconds|秒)\s*\)|（\s*\d+\s*秒\s*）)`)
)

// hasExplicitShotTiming reports whether the user's prompt itself explicitly
// lays out timed shots/ranges or a specific video length.
func hasExplicitShotTiming(prompt string) bool {
	if shotTimeRangeRe.MatchString(prompt) || shotClockRangeRe.MatchString(prompt) || explicitTotalRe.MatchString(prompt) {
		return true
	}
	return len(shotDurationRe.FindAllString(prompt, 3)) >= 2
}

func finalizeStoryboard(userPrompt string, shots []VideoShot, maxSec, targetSec, segMax int) []VideoShot {
	if len(shots) == 0 {
		return shots
	}
	if segMax <= 0 || segMax > MaxVideoSegmentSeconds {
		segMax = MaxVideoSegmentSeconds
	}
	explicitTiming := hasExplicitShotTiming(userPrompt)
	shots = consolidateStoryboardShots(shots, segMax, explicitTiming)

	if !explicitTiming && targetSec > 0 && targetSec <= maxSec {
		total := 0
		for _, s := range shots {
			total += s.Seconds
		}
		if total < targetSec {
			shots = expandStoryboardToTarget(shots, targetSec, segMax)
		}
	}
	shots[0].ContinuousFromPrev = false
	return shots
}

func mergeShotPrompts(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n" + b
	}
}

func consolidateStoryboardShots(shots []VideoShot, segMax int, explicitTiming bool) []VideoShot {
	if len(shots) <= 1 {
		return shots
	}
	out := append([]VideoShot(nil), shots...)
	total := 0
	for _, s := range out {
		total += s.Seconds
	}
	maxPref := maxPreferredShots(total, segMax)

	// Pass 1: always merge continuous micro-shots (< 4s) or excess continuous beats
	// within segMax so we never waste separate video turns on tiny sub-clips.
	for len(out) > 1 {
		idx := -1
		for i := 1; i < len(out); i++ {
			if !out[i].ContinuousFromPrev || out[i-1].Seconds+out[i].Seconds > segMax {
				continue
			}
			if out[i-1].Seconds < 4 || out[i].Seconds < 4 || (!explicitTiming && len(out) > maxPref) {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		out[idx-1].Seconds += out[idx].Seconds
		out[idx-1].Prompt = mergeShotPrompts(out[idx-1].Prompt, out[idx].Prompt)
		out = append(out[:idx], out[idx+1:]...)
	}

	if explicitTiming {
		return out
	}

	// Pass 2: for general prompts without explicit timestamps, merge remaining
	// micro-shots or excess segments (preferring smallest combined lengths).
	for len(out) > 1 {
		needShrink := len(out) > maxPref
		if !needShrink {
			for _, s := range out {
				if s.Seconds < 4 {
					needShrink = true
					break
				}
			}
		}
		if !needShrink {
			break
		}
		bestIdx := -1
		bestSum := segMax + 1
		for i := 1; i < len(out); i++ {
			sum := out[i-1].Seconds + out[i].Seconds
			if sum > segMax {
				continue
			}
			// Prefer merging a continuous pair over a cut on ties.
			if sum < bestSum || (sum == bestSum && bestIdx >= 0 && out[i].ContinuousFromPrev && !out[bestIdx].ContinuousFromPrev) {
				bestIdx = i
				bestSum = sum
			}
		}
		if bestIdx < 0 {
			break
		}
		out[bestIdx-1].Seconds += out[bestIdx].Seconds
		out[bestIdx-1].Prompt = mergeShotPrompts(out[bestIdx-1].Prompt, out[bestIdx].Prompt)
		out = append(out[:bestIdx], out[bestIdx+1:]...)
	}
	return out
}

func expandStoryboardToTarget(shots []VideoShot, targetSec, segMax int) []VideoShot {
	out := append([]VideoShot(nil), shots...)
	total := 0
	for _, s := range out {
		total += s.Seconds
	}
	for total < targetSec {
		best := -1
		for i := range out {
			if out[i].Seconds < segMax && (best < 0 || out[i].Seconds < out[best].Seconds) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		out[best].Seconds++
		total++
	}
	if total < targetSec && len(out) > 0 {
		tailPrompt := out[len(out)-1].Prompt
		for _, sec := range SplitVideoSeconds(targetSec-total, segMax) {
			out = append(out, VideoShot{
				Seconds:            sec,
				ContinuousFromPrev: true,
				Prompt:             tailPrompt,
			})
		}
	}
	return out
}

// parseStoryboard reads the director's reply: an object with "characters"
// and "shots" (or "storyboard"), or a bare array of shots. It returns the
// shots and the shared character description.
func parseStoryboard(text string, maxSec, segMax int) ([]VideoShot, string, error) {
	if segMax <= 0 || segMax > MaxVideoSegmentSeconds {
		segMax = MaxVideoSegmentSeconds
	}
	raw, err := extractStoryboardJSON(text)
	if err != nil {
		return nil, "", err
	}
	var parsed []VideoShot
	var characters string
	if err := json.Unmarshal(raw, &parsed); err != nil {
		var wrapper struct {
			Characters json.RawMessage `json:"characters"`
			Shots      []VideoShot     `json:"shots"`
			Storyboard []VideoShot     `json:"storyboard"`
		}
		if err2 := json.Unmarshal(raw, &wrapper); err2 != nil {
			return nil, "", err
		}
		if len(wrapper.Shots) > 0 {
			parsed = wrapper.Shots
		} else {
			parsed = wrapper.Storyboard
		}
		characters = characterText(wrapper.Characters)
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
		return nil, "", fmt.Errorf("empty storyboard")
	}
	out[0].ContinuousFromPrev = false
	return out, characters, nil
}

// characterText accepts "characters" as a string, or as a list or object of
// per-character descriptions, which models sometimes return instead.
func characterText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		var lines []string
		for _, item := range list {
			if t := characterText(item); t != "" {
				lines = append(lines, t)
			}
		}
		return strings.Join(lines, "\n")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		var lines []string
		for _, k := range keys {
			if t := characterText(obj[k]); t != "" {
				lines = append(lines, k+": "+t)
			}
		}
		return strings.Join(lines, "\n")
	}
	return ""
}

// extractStoryboardJSON returns the outermost JSON value in text, trying the
// bracket that opens first so an object's inner "shots" array is not taken
// for the whole reply.
func extractStoryboardJSON(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	pairs := [][2]byte{{'[', ']'}, {'{', '}'}}
	if o, a := strings.IndexByte(text, '{'), strings.IndexByte(text, '['); o >= 0 && (a < 0 || o < a) {
		pairs[0], pairs[1] = pairs[1], pairs[0]
	}
	for _, p := range pairs {
		start := strings.IndexByte(text, p[0])
		end := strings.LastIndexByte(text, p[1])
		if start >= 0 && end > start {
			candidate := text[start : end+1]
			if json.Valid([]byte(candidate)) {
				return []byte(candidate), nil
			}
		}
	}
	return nil, fmt.Errorf("no valid JSON found in response")
}
