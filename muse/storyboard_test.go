package muse

import (
	"slices"
	"strings"
	"testing"
)

func TestParseStoryboardRespectsCustom15sUnder20sCeiling(t *testing.T) {
	reply := "```json\n" + `[
  {"seconds": 7, "continuous_from_prev": true, "prompt": "1-7s: 镜头一，男主在雨夜街头驻足"},
  {"seconds": 8, "continuous_from_prev": false, "prompt": "7-15s: 镜头二，切到霓虹招牌特写"}
]` + "\n```"
	shots, _, err := parseStoryboard(reply, 20, 10)
	if err != nil {
		t.Fatalf("parseStoryboard: %v", err)
	}
	if len(shots) != 2 {
		t.Fatalf("len(shots) = %d, want 2", len(shots))
	}
	if shots[0].Seconds != 7 || shots[0].ContinuousFromPrev {
		t.Fatalf("shot[0] = %+v, want 7s continuous=false", shots[0])
	}
	if shots[1].Seconds != 8 || shots[1].ContinuousFromPrev {
		t.Fatalf("shot[1] = %+v, want 8s continuous=false", shots[1])
	}
}

func TestParseStoryboardSplitsOver10sShotAndCapsTotal(t *testing.T) {
	reply := `{"shots":[
		{"seconds": 14, "continuous_from_prev": false, "prompt": "long opening tracking shot"},
		{"seconds": 10, "continuous_from_prev": true, "prompt": "closing shot"}
	]}`
	shots, _, err := parseStoryboard(reply, 20, 10)
	if err != nil {
		t.Fatalf("parseStoryboard: %v", err)
	}
	var gotSecs []int
	for _, s := range shots {
		gotSecs = append(gotSecs, s.Seconds)
	}
	if !slices.Equal(gotSecs, []int{7, 7, 6}) {
		t.Fatalf("got durations %v, want [7 7 6] (capped at 20s)", gotSecs)
	}
	if shots[0].ContinuousFromPrev || !shots[1].ContinuousFromPrev || !shots[2].ContinuousFromPrev {
		t.Fatalf("unexpected continuity flags: %+v", shots)
	}
}

func TestParseStoryboardReadsCharacters(t *testing.T) {
	reply := "好的，分镜如下：\n```json\n" + `{
  "characters": "女孩A：约20岁，黑色齐肩短发，红色连帽卫衣，蓝色牛仔裤",
  "shots": [
    {"seconds": 10, "continuous_from_prev": false, "prompt": "女孩A走进雨夜街头"},
    {"seconds": 10, "continuous_from_prev": true, "prompt": "女孩A抬头看霓虹招牌"}
  ]
}` + "\n```"
	shots, characters, err := parseStoryboard(reply, 20, 10)
	if err != nil {
		t.Fatalf("parseStoryboard: %v", err)
	}
	if len(shots) != 2 || shots[1].Prompt != "女孩A抬头看霓虹招牌" {
		t.Fatalf("shots = %+v, want the two shots from the object", shots)
	}
	if !strings.Contains(characters, "红色连帽卫衣") {
		t.Fatalf("characters = %q, want the description", characters)
	}
}

func TestCharacterTextAcceptsListAndObject(t *testing.T) {
	if got := characterText([]byte(`["A: red hoodie", {"B": "grey coat"}]`)); got != "A: red hoodie\nB: grey coat" {
		t.Fatalf("list: got %q", got)
	}
	if got := characterText([]byte(`{"Mia": "red hoodie", "Leo": "grey coat"}`)); got != "Leo: grey coat\nMia: red hoodie" {
		t.Fatalf("object: got %q", got)
	}
	if got := characterText([]byte(`null`)); got != "" {
		t.Fatalf("null: got %q", got)
	}
}

func TestWithCharactersPrefixesEveryShotOnce(t *testing.T) {
	desc := "Mia: about 20, black bob, red hoodie"
	shots := finalizeStoryboard("Mia walks through rain", []VideoShot{
		{Seconds: 3, Prompt: "Mia walks"},
		{Seconds: 3, ContinuousFromPrev: true, Prompt: "Mia stops"},
		{Seconds: 8, Prompt: "Mia looks up"},
	}, 20, 20, 10)
	shots = withCharacters(shots, desc)
	for i, s := range shots {
		if strings.Count(s.Prompt, desc) != 1 || !strings.HasPrefix(s.Prompt, "Recurring characters") {
			t.Fatalf("shot %d prompt = %q, want the description once at the start", i, s.Prompt)
		}
	}
	if got := withCharacters([]VideoShot{{Seconds: 5, Prompt: "p"}}, " "); got[0].Prompt != "p" {
		t.Fatalf("empty description changed the prompt: %q", got[0].Prompt)
	}
}

func TestBuildStoryboardPromptIncludesBackgroundAndFixedCharacters(t *testing.T) {
	got := buildStoryboardPrompt(StoryboardRequest{
		Prompt:     "Mia finds the lantern",
		Background: "A flooded city in 2090",
		Characters: "- Mia: about 20, red hoodie",
	}, 20, 20, 10)
	for _, want := range []string{"A flooded city in 2090", "Fixed characters and places", "- Mia: about 20, red hoodie"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, got)
		}
	}
}

func TestHasExplicitShotTiming(t *testing.T) {
	pos := []string{
		"1-7s 男主站在街头，7-15s 切到女主特写",
		"0-5秒开场空镜，5-15秒主角步行",
		"00:00-00:05 清晨街道，00:05-00:15 咖啡馆内",
		"前7秒铺垫氛围，后8秒高潮爆发",
		"镜头1(5s)远景，镜头2(8s)特写",
		"总时长15秒的故事，讲述宇航员重返地球",
		"做一个15秒的视频，主角在雨夜奔跑",
	}
	for _, p := range pos {
		if !hasExplicitShotTiming(p) {
			t.Errorf("hasExplicitShotTiming(%q) = false, want true", p)
		}
	}

	neg := []string{
		"一只橘猫在屋顶奔跑，停顿2秒后轻盈跃下",
		"赛博朋克雨夜街头，霓虹闪烁，镜头缓慢推进",
		"第3秒可能会下雨的场景",
	}
	for _, p := range neg {
		if hasExplicitShotTiming(p) {
			t.Errorf("hasExplicitShotTiming(%q) = true, want false", p)
		}
	}
}

func TestBuildStoryboardPromptMatchesTargetDuration(t *testing.T) {
	got := buildStoryboardPrompt(StoryboardRequest{Prompt: "一只橘猫在屋顶奔跑"}, 30, 30, 10)
	if !strings.Contains(got, "Required default total duration: 30 seconds") {
		t.Fatalf("missing 30s target instruction in prompt:\n%s", got)
	}
	if strings.Contains(got, "15 seconds") || strings.Contains(got, "15s total") || strings.Contains(got, `"seconds": 7`) {
		t.Fatalf("prompt should not anchor 30s tier to hardcoded 15s example:\n%s", got)
	}
	if strings.Count(got, `"seconds": 10`) != 3 {
		t.Fatalf("expected 3x 10s example shots for 30s prompt:\n%s", got)
	}
}

func TestFinalizeStoryboardRestoresGeneralTargetsAndSuppressesMicroShots(t *testing.T) {
	shotSecs := func(shots []VideoShot) []int {
		out := make([]int, len(shots))
		for i, s := range shots {
			out[i] = s.Seconds
		}
		return out
	}

	// Regression for task_qLuoo9Hrr1VIC7KMTDQNs6DGyK6Mb02z: 30s tier returning [6 5 4] = 15s.
	got30 := finalizeStoryboard("霓虹雨夜城市街头，主角撑伞走过", []VideoShot{
		{Seconds: 6, ContinuousFromPrev: false, Prompt: "镜头1"},
		{Seconds: 5, ContinuousFromPrev: false, Prompt: "镜头2"},
		{Seconds: 4, ContinuousFromPrev: false, Prompt: "镜头3"},
	}, 30, 30, 10)
	if secs := shotSecs(got30); !slices.Equal(secs, []int{10, 10, 10}) {
		t.Fatalf("30s general storyboard = %v, want [10 10 10]", secs)
	}

	// Regression for task_yLMr9kS8iryoAS1G13XsSbt8iRxGxxRw: 20s tier chopped into [2 3 3 3 2 2] = 15s.
	got20 := finalizeStoryboard("宇航员在火星沙尘暴中前行，抬头望向基地信号塔", []VideoShot{
		{Seconds: 2, ContinuousFromPrev: false, Prompt: "p1"},
		{Seconds: 3, ContinuousFromPrev: true, Prompt: "p2"},
		{Seconds: 3, ContinuousFromPrev: true, Prompt: "p3"},
		{Seconds: 3, ContinuousFromPrev: false, Prompt: "p4"},
		{Seconds: 2, ContinuousFromPrev: true, Prompt: "p5"},
		{Seconds: 2, ContinuousFromPrev: true, Prompt: "p6"},
	}, 20, 20, 10)
	if len(got20) > 3 {
		t.Fatalf("expected <= 3 consolidated shots for 20s tier, got %d (%v)", len(got20), shotSecs(got20))
	}
	sum20 := 0
	for i, s := range got20 {
		sum20 += s.Seconds
		if s.Seconds < 4 {
			t.Fatalf("shot %d still micro-fragmented (%ds): %+v", i, s.Seconds, got20)
		}
	}
	if sum20 != 20 {
		t.Fatalf("20s consolidated storyboard summed to %ds (%v), want 20s", sum20, shotSecs(got20))
	}
	if got20[0].ContinuousFromPrev {
		t.Fatalf("first shot must not be continuous_from_prev")
	}

	// Explicit custom 15s timestamps under 20s ceiling must remain preserved.
	gotCustom := finalizeStoryboard("1-7s 男主站在街头，7-15s 切到女主特写", []VideoShot{
		{Seconds: 7, ContinuousFromPrev: false, Prompt: "男主"},
		{Seconds: 8, ContinuousFromPrev: false, Prompt: "女主"},
	}, 20, 20, 10)
	if secs := shotSecs(gotCustom); !slices.Equal(secs, []int{7, 8}) {
		t.Fatalf("explicit 15s storyboard = %v, want [7 8]", secs)
	}

	// Explicit total still merges continuous <4s micro-shots without altering total duration.
	gotExplicitMicro := finalizeStoryboard("总时长15秒的视频，连续推进镜头", []VideoShot{
		{Seconds: 2, ContinuousFromPrev: false, Prompt: "起"},
		{Seconds: 3, ContinuousFromPrev: true, Prompt: "承"},
		{Seconds: 5, ContinuousFromPrev: false, Prompt: "转"},
		{Seconds: 5, ContinuousFromPrev: false, Prompt: "合"},
	}, 20, 20, 10)
	if secs := shotSecs(gotExplicitMicro); !slices.Equal(secs, []int{5, 5, 5}) {
		t.Fatalf("explicit continuous micro-shots = %v, want [5 5 5]", secs)
	}
}
