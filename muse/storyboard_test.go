package muse

import (
	"slices"
	"testing"
)

func TestParseStoryboardRespectsCustom15sUnder20sCeiling(t *testing.T) {
	reply := "```json\n" + `[
  {"seconds": 7, "continuous_from_prev": true, "prompt": "1-7s: 镜头一，男主在雨夜街头驻足"},
  {"seconds": 8, "continuous_from_prev": false, "prompt": "7-15s: 镜头二，切到霓虹招牌特写"}
]` + "\n```"
	shots, err := parseStoryboard(reply, 20, 10)
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
	shots, err := parseStoryboard(reply, 20, 10)
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
