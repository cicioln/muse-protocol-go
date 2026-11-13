package muse

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/CoolkHz/muse-protocol-go/internal/fakemuse"
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

func TestShellJoinQuotesArguments(t *testing.T) {
	got := shellJoin([]string{"ffmpeg", "-i", "/workspace/out/a b.mp4", "-filter_complex", "[0:v]trim=duration=2[v0];", "it's"})
	want := `ffmpeg -i '/workspace/out/a b.mp4' -filter_complex '[0:v]trim=duration=2[v0];' 'it'\''s'`
	if got != want {
		t.Fatalf("shellJoin = %s\nwant %s", got, want)
	}
}

func requireFFmpeg(t *testing.T) string {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	return ffmpeg
}

// testClip makes an MP4 test pattern, with a sine tone when audio is set.
func testClip(t *testing.T, ffmpeg, path, seconds string, audio bool) []byte {
	t.Helper()
	args := []string{"-f", "lavfi", "-i", "testsrc=size=320x180:rate=24:duration=" + seconds}
	if audio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration="+seconds, "-c:a", "aac")
	}
	args = append(args, "-c:v", "libx264", "-pix_fmt", "yuv420p", "-shortest", path)
	if err := runFFmpeg(context.Background(), ffmpeg, args...); err != nil {
		t.Fatalf("make %s: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestJoinLocallyAndLastFrame(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	newClip := func(name, seconds string, audio bool) Media {
		path := filepath.Join(dir, name)
		testClip(t, ffmpeg, path, seconds, audio)
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

	job := newJoinJob(ctx, ffmpeg, dir, []Media{a, b}, []float64{2, 2})
	if !job.audio[0] || job.audio[1] {
		t.Fatalf("audio detection = %v", job.audio)
	}
	out, err := joinLocally(ctx, ffmpeg, dir, job)
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != "video" || out.Mime != "video/mp4" || out.Width != 320 || out.Height != 180 {
		t.Fatalf("joined = %+v", out)
	}
	if math.Abs(out.Seconds-4) > 0.25 {
		t.Fatalf("joined length = %.2fs, want about 4s", out.Seconds)
	}
	if err := checkJoined(*out, job); err != nil {
		t.Fatalf("checkJoined: %v", err)
	}
	if !hasAudio(ctx, ffmpeg, filepath.Join(dir, out.Filename)) {
		t.Fatal("joined video lost its audio track")
	}
}

func TestCheckJoinedRejectsWrongLength(t *testing.T) {
	job := &joinJob{lengths: []float64{10, 10}, w: 1280, h: 720}
	if err := checkJoined(Media{Kind: "video", Seconds: 10, Width: 1280, Height: 720}, job); err == nil {
		t.Fatal("a 10s result for a 20s join was accepted")
	}
	if err := checkJoined(Media{Kind: "video", Seconds: 19.6, Width: 1280, Height: 720}, job); err != nil {
		t.Fatalf("a 19.6s result was rejected: %v", err)
	}
	if err := checkJoined(Media{Kind: "video", Seconds: 20, Width: 640, Height: 360}, job); err == nil {
		t.Fatal("a result with the wrong size was accepted")
	}
}

// fakeVMJoin runs the command from a join prompt with local ffmpeg against
// the fake gateway's files, like the agent running it in its sandbox. It runs
// on a gateway goroutine, so it reports problems as reply text, not through t.
func fakeVMJoin(base, ffmpeg string) func(string, map[string][]byte) (string, []byte, string) {
	commandRe := regexp.MustCompile("(?s)```sh\n(.*?)\n```")
	return func(prompt string, files map[string][]byte) (string, []byte, string) {
		m := commandRe.FindStringSubmatch(prompt)
		if m == nil {
			return "", nil, "no command in prompt"
		}
		args, err := shellSplit(m[1])
		if err != nil || len(args) < 2 || args[0] != "ffmpeg" {
			return "", nil, "bad command"
		}
		dir, err := os.MkdirTemp(base, "vm-")
		if err != nil {
			return "", nil, err.Error()
		}
		local := func(p string) string { return filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(p, "/"))) }
		out := args[len(args)-1]
		for i := 1; i < len(args); i++ {
			if args[i-1] == "-i" {
				data, ok := files[strings.TrimPrefix(args[i], "/")]
				if !ok {
					return "", nil, "missing input " + args[i]
				}
				_ = os.MkdirAll(filepath.Dir(local(args[i])), 0o700)
				_ = os.WriteFile(local(args[i]), data, 0o600)
				args[i] = local(args[i])
			}
		}
		_ = os.MkdirAll(filepath.Dir(local(out)), 0o700)
		args[len(args)-1] = local(out)
		if err := runFFmpeg(context.Background(), ffmpeg, args[1:]...); err != nil {
			return "", nil, "ffmpeg failed: " + err.Error()
		}
		data, err := os.ReadFile(local(out))
		if err != nil {
			return "", nil, err.Error()
		}
		return strings.TrimPrefix(out, "/"), data, ""
	}
}

// shellSplit undoes shellJoin.
func shellSplit(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inArg, quoted := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted:
			if c == '\'' {
				quoted = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'':
			quoted, inArg = true, true
		case c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			inArg = true
		case c == ' ' || c == '\n':
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteByte(c)
			inArg = true
		}
	}
	if quoted {
		return nil, errors.New("unterminated quote")
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out, nil
}

func longVideoEngine(t *testing.T, configure func(*fakemuse.Gateway)) (*ProtoEngine, *fakemuse.Gateway, *Account, Config) {
	t.Helper()
	site, gw := fakemuse.NewWithGateway()
	configure(gw)
	t.Cleanup(site.Close)
	previousTextSettle, previousMediaSettle := textSettle, mediaSettle
	textSettle, mediaSettle = 300*time.Millisecond, time.Second
	t.Cleanup(func() { textSettle, mediaSettle = previousTextSettle, previousMediaSettle })
	cfg := Config{DataDir: t.TempDir(), SiteURL: site.URL, GatewayURL: "ws" + strings.TrimPrefix(site.URL, "http") + "/v1/noise",
		IdleClose: time.Minute, ApprovalPolicy: "deny", Timezone: "UTC"}
	eng := NewProtoEngine(&cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(eng.Close)
	acc := &Account{ID: "a1", Cookies: map[string]string{"hatch_sess": "s1"}, CookieExpires: map[string]int64{}}
	return eng, gw, acc, cfg
}

// longVideoRequest asks for a video of two 2-second segments.
func longVideoRequest(join string, onJoin func(string, error)) LongVideoRequest {
	return LongVideoRequest{
		Seconds:        4,
		SegmentSeconds: 2,
		Timeout:        20 * time.Second,
		Join:           join,
		OnJoin:         onJoin,
		Segment: func(s VideoSegment) (string, []RefImage) {
			var refs []RefImage
			if s.LastFrame != nil {
				refs = append(refs, *s.LastFrame)
			}
			return "Use your video generation tool. Part " + strings.Repeat("I", s.Index+1), refs
		},
	}
}

// clips returns a fake Gateway.Video that serves pre-made 2-second clips,
// odd ones with sound.
func clips(t *testing.T, ffmpeg string) func(int) []byte {
	dir := t.TempDir()
	withSound := testClip(t, ffmpeg, filepath.Join(dir, "sound.mp4"), "2", true)
	silent := testClip(t, ffmpeg, filepath.Join(dir, "silent.mp4"), "2", false)
	return func(n int) []byte {
		if n%2 == 1 {
			return withSound
		}
		return silent
	}
}

func TestGenerateLongVideoJoinsOnVM(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	video, vmDir := clips(t, ffmpeg), t.TempDir()
	eng, gw, acc, cfg := longVideoEngine(t, func(gw *fakemuse.Gateway) {
		gw.Video = video
		gw.Join = fakeVMJoin(vmDir, ffmpeg)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var where string
	var vmErr error
	out, err := eng.GenerateLongVideo(ctx, acc, longVideoRequest(JoinAuto, func(w string, e error) { where, vmErr = w, e }))
	if err != nil {
		t.Fatalf("GenerateLongVideo: %v", err)
	}
	if where != "vm" || vmErr != nil {
		t.Fatalf("joined on %q (vm error %v), want vm", where, vmErr)
	}
	if out.Width != 320 || out.Height != 180 || math.Abs(out.Seconds-4) > 0.25 {
		t.Fatalf("joined video = %+v", out)
	}
	if _, err := os.Stat(filepath.Join(cfg.MediaDir(), out.Filename)); err != nil {
		t.Fatalf("joined video not saved: %v", err)
	}
	entries, _ := os.ReadDir(cfg.MediaDir())
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("media dir should hold only the joined video, has %v", names)
	}
	allowed := false
	for _, d := range gw.Decisions {
		allowed = allowed || strings.HasSuffix(d, "=allow_once")
	}
	if !allowed {
		t.Fatalf("join turn was not allowed to run a shell: %v", gw.Decisions)
	}
}

func TestGenerateLongVideoFallsBackToLocalJoin(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	video := clips(t, ffmpeg)
	eng, _, acc, _ := longVideoEngine(t, func(gw *fakemuse.Gateway) {
		gw.Video = video
		gw.Join = func(string, map[string][]byte) (string, []byte, string) {
			return "", nil, "bash: ffmpeg: command not found"
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var where string
	var vmErr error
	out, err := eng.GenerateLongVideo(ctx, acc, longVideoRequest(JoinAuto, func(w string, e error) { where, vmErr = w, e }))
	if err != nil {
		t.Fatalf("GenerateLongVideo: %v", err)
	}
	if where != "local" || vmErr == nil {
		t.Fatalf("joined on %q (vm error %v), want local after a VM failure", where, vmErr)
	}
	if math.Abs(out.Seconds-4) > 0.25 {
		t.Fatalf("joined length = %.2fs", out.Seconds)
	}

	// JoinVM does not fall back.
	if _, err := eng.GenerateLongVideo(ctx, acc, longVideoRequest(JoinVM, nil)); err == nil || Retryable(err) {
		t.Fatalf("JoinVM with a failing VM: %v (retryable %v)", err, Retryable(err))
	}
}

func TestLocalJoinsShareFFmpegGate(t *testing.T) {
	ffmpeg := requireFFmpeg(t)
	video := clips(t, ffmpeg)
	eng, _, acc, _ := longVideoEngine(t, func(gw *fakemuse.Gateway) {
		gw.Video = video
	})
	// Hold the only slot: local joins must wait for it.
	eng.ffmpegGate <- struct{}{}
	var done atomic.Int32
	errs := make(chan error, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for i := 0; i < 2; i++ {
		accN := &Account{ID: acc.ID + string(rune('a'+i)), Cookies: acc.Cookies, CookieExpires: map[string]int64{}}
		go func() {
			_, err := eng.GenerateLongVideo(ctx, accN, longVideoRequest(JoinLocal, nil))
			done.Add(1)
			errs <- err
		}()
	}
	time.Sleep(4 * time.Second)
	if n := done.Load(); n != 0 {
		t.Fatalf("%d joins finished while the ffmpeg slot was held", n)
	}
	<-eng.ffmpegGate
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
	}
}
