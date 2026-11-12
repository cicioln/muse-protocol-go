package muse

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

func TestSplitVideoSeconds(t *testing.T) {
	cases := []struct {
		total, segment int
		want           []int
	}{
		{5, 10, []int{5}},
		{10, 10, []int{10}},
		{20, 10, []int{10, 10}},
		{25, 10, []int{9, 8, 8}},
		{60, 10, []int{10, 10, 10, 10, 10, 10}},
		{30, 8, []int{8, 8, 7, 7}},
		{12, 0, []int{6, 6}},
		{0, 10, nil},
	}
	for _, c := range cases {
		if got := SplitVideoSeconds(c.total, c.segment); !slices.Equal(got, c.want) {
			t.Errorf("SplitVideoSeconds(%d, %d) = %v, want %v", c.total, c.segment, got, c.want)
		}
	}
}

func TestSegmentErrorKeepsKindAndBlocksRetryAfterFirstSegment(t *testing.T) {
	quota := errf(KindQuota, "out of credits")
	first := segmentError(quota, 0, 3)
	if KindOf(first) != KindQuota || !Retryable(first) {
		t.Fatalf("first segment error = %v (kind %v, retryable %v)", first, KindOf(first), Retryable(first))
	}
	later := segmentError(quota, 1, 3)
	if KindOf(later) != KindQuota || Retryable(later) {
		t.Fatalf("later segment error = %v (kind %v, retryable %v)", later, KindOf(later), Retryable(later))
	}
	if single := segmentError(quota, 0, 1); single != quota {
		t.Fatalf("single segment error was relabeled: %v", single)
	}
}

func TestJoinVideosAndLastFrame(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	newClip := func(name string, seconds string, audio bool) Media {
		path := filepath.Join(dir, name)
		args := []string{"-f", "lavfi", "-i", "testsrc=size=320x180:rate=24:duration=" + seconds}
		if audio {
			args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration="+seconds, "-c:a", "aac")
		}
		args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-shortest", path)
		if err := runFFmpeg(ctx, ffmpeg, args...); err != nil {
			t.Fatalf("make %s: %v", name, err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		info, _ := f.Stat()
		m := describeMedia(f, info.Size(), "video/mp4", mediaRef{Kind: "video", Path: path})
		m.Filename = name
		return *m
	}
	a := newClip("a.mp4", "3", true)
	b := newClip("b.mp4", "2.5", false)
	if a.Width != 320 || a.Height != 180 {
		t.Fatalf("probe a = %dx%d", a.Width, a.Height)
	}

	frame, err := lastFrame(ctx, ffmpeg, filepath.Join(dir, a.Filename), 2, filepath.Join(dir, "frame.png"))
	if err != nil {
		t.Fatal(err)
	}
	if w, h, _ := probeMedia(frame); w != 320 || h != 180 {
		t.Fatalf("frame = %dx%d", w, h)
	}

	out, err := joinVideos(ctx, ffmpeg, dir, []Media{a, b}, []float64{2, 2})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != "video" || out.Mime != "video/mp4" || out.Width != 320 || out.Height != 180 {
		t.Fatalf("joined = %+v", out)
	}
	if math.Abs(out.Seconds-4) > 0.25 {
		t.Fatalf("joined length = %.2fs, want about 4s", out.Seconds)
	}
	if !hasAudio(ctx, ffmpeg, filepath.Join(dir, out.Filename)) {
		t.Fatal("joined video lost its audio track")
	}
}
