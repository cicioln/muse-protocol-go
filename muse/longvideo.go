package muse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// MaxVideoSegmentSeconds is the longest clip Muse makes in one turn. Longer
// videos are generated as consecutive segments and joined.
const MaxVideoSegmentSeconds = 10

// MaxLongVideoSeconds caps LongVideoRequest.Seconds.
const MaxLongVideoSeconds = 300

// Ways GenerateLongVideo joins segments.
const (
	JoinAuto  = ""      // on the account's VM, falling back to local ffmpeg
	JoinVM    = "vm"    // on the account's VM only
	JoinLocal = "local" // with local ffmpeg only
)

// vmJoinTimeout bounds the turn in which the VM joins the segments.
var vmJoinTimeout = 8 * time.Minute

// VideoSegment describes one segment of a long video.
type VideoSegment struct {
	Index   int // 0-based
	Count   int
	Start   int // seconds into the finished video
	Seconds int
	// LastFrame is the final frame of the previous segment, nil for the first
	// segment. Attach it so the segment starts where the previous one ended.
	LastFrame *RefImage
}

// LongVideoRequest describes a video longer than one Muse clip. Segment
// returns the prompt and reference images of each segment; it is called once
// per segment, in order, after the previous segment has finished.
type LongVideoRequest struct {
	Seconds        int
	SegmentSeconds int // defaults to MaxVideoSegmentSeconds
	Segment        func(VideoSegment) (prompt string, refs []RefImage)
	Timeout        time.Duration // per segment; defaults to Config.GenerateTimeout
	OnProgress     func(pct int)
	// Join selects where segments are joined: JoinAuto (default), JoinVM or
	// JoinLocal.
	Join string
	// OnJoin, when set, reports where the segments were joined ("vm" or
	// "local") and, after a fallback, why the VM join failed.
	OnJoin func(where string, vmErr error)
}

// SplitVideoSeconds splits total seconds into the fewest segments of at most
// segment seconds, as evenly as possible: 25 with segment 10 is [9 8 8].
func SplitVideoSeconds(total, segment int) []int {
	if total <= 0 {
		return nil
	}
	if segment <= 0 || segment > MaxVideoSegmentSeconds {
		segment = MaxVideoSegmentSeconds
	}
	n := (total + segment - 1) / segment
	out := make([]int, n)
	for i := range out {
		out[i] = total / n
		if i < total%n {
			out[i]++
		}
	}
	return out
}

// FFmpeg returns the ffmpeg binary GenerateLongVideo uses: Config.FFmpegPath,
// or "ffmpeg" looked up on PATH.
func (c Config) FFmpeg() (string, error) {
	name := c.FFmpegPath
	if name == "" {
		name = "ffmpeg"
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", BadRequest("ffmpeg is required for long videos: %v", err)
	}
	return path, nil
}

// GenerateLongVideo makes a video of r.Seconds by generating segments in turn,
// each starting from the last frame of the one before, and joins them. Local
// ffmpeg takes each segment's last frame. The join itself runs on the
// account's VM unless r.Join says otherwise; when the VM cannot produce a
// video of the expected length, local ffmpeg joins instead (with JoinAuto).
// Local joins run at low priority, Config.FFmpegConcurrency at a time.
// Segments longer than planned are cut to their planned length. The joined
// video is saved in Config.MediaDir(); the segments are removed.
//
// A failure after the first segment is wrapped in NoRetry, because earlier
// segments already used the account's media quota.
func (e *ProtoEngine) GenerateLongVideo(ctx context.Context, acc *Account, r LongVideoRequest) (*Media, error) {
	if r.Segment == nil {
		return nil, BadRequest("segment callback is required")
	}
	if r.Seconds < 1 || r.Seconds > MaxLongVideoSeconds {
		return nil, BadRequest("seconds must be between 1 and %d", MaxLongVideoSeconds)
	}
	switch r.Join {
	case JoinAuto, JoinVM, JoinLocal:
	default:
		return nil, BadRequest("join must be empty, %q or %q", JoinVM, JoinLocal)
	}
	ffmpeg, err := e.cfg.FFmpeg()
	if err != nil {
		return nil, err
	}
	plan := SplitVideoSeconds(r.Seconds, r.SegmentSeconds)
	progress := func(p int) {
		if r.OnProgress != nil {
			r.OnProgress(p)
		}
	}
	dir := e.cfg.MediaDir()
	var segments []Media
	var lengths []float64
	var temp []string
	defer func() {
		for _, m := range segments {
			_ = os.Remove(filepath.Join(dir, m.Filename))
		}
		for _, path := range temp {
			_ = os.Remove(path)
		}
	}()
	var frame *RefImage
	start := 0
	for i, seconds := range plan {
		prompt, refs := r.Segment(VideoSegment{Index: i, Count: len(plan), Start: start, Seconds: seconds, LastFrame: frame})
		files, err := e.Generate(ctx, acc, GenRequest{
			Prompt:  prompt,
			Kind:    "video",
			Refs:    refs,
			N:       1,
			Timeout: r.Timeout,
			OnProgress: func(p int) {
				progress((i*100 + p) * 90 / (len(plan) * 100))
			},
		})
		if err == nil && len(files) == 0 {
			err = errf(KindTransient, "muse returned no video")
		}
		if err != nil {
			return nil, segmentError(err, i, len(plan))
		}
		for _, extra := range files[1:] {
			_ = os.Remove(filepath.Join(dir, extra.Filename))
		}
		segment := files[0]
		segments = append(segments, segment)
		length := float64(seconds)
		if segment.Seconds > 0 && segment.Seconds < length {
			length = segment.Seconds
		}
		lengths = append(lengths, length)
		start += seconds
		if i == len(plan)-1 {
			break
		}
		path := filepath.Join(dir, ".frame-"+randHex(8)+".png")
		temp = append(temp, path)
		data, err := lastFrame(ctx, ffmpeg, filepath.Join(dir, segment.Filename), length, path)
		if err != nil {
			return nil, &NoRetry{Err: wrap(KindTransient, fmt.Sprintf("第 %d/%d 段视频取末帧失败", i+1, len(plan)), err)}
		}
		frame = &RefImage{Data: data, Mime: "image/png"}
	}
	progress(92)
	if len(segments) == 1 && segments[0].Seconds <= float64(plan[0])+0.5 {
		out := segments[0]
		segments = nil
		progress(100)
		return &out, nil
	}
	job := newJoinJob(ctx, ffmpeg, dir, segments, lengths)
	var vmErr error
	if r.Join != JoinLocal {
		out, err := e.joinOnVM(ctx, acc, job)
		if err == nil {
			if r.OnJoin != nil {
				r.OnJoin("vm", nil)
			}
			progress(100)
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, &NoRetry{Err: wrap(KindTransient, "拼接视频中断", ctx.Err())}
		}
		vmErr = err
		if r.Join == JoinVM {
			return nil, &NoRetry{Err: wrap(KindTransient, "VM 拼接视频失败", err)}
		}
	}
	select {
	case e.ffmpegGate <- struct{}{}:
	case <-ctx.Done():
		return nil, &NoRetry{Err: wrap(KindTransient, "等待本地拼接中断", ctx.Err())}
	}
	out, err := joinLocally(ctx, ffmpeg, dir, job)
	<-e.ffmpegGate
	if err != nil {
		return nil, &NoRetry{Err: wrap(KindTransient, "拼接视频失败", err)}
	}
	if r.OnJoin != nil {
		r.OnJoin("local", vmErr)
	}
	progress(100)
	return out, nil
}

func segmentError(err error, index, count int) error {
	if count == 1 {
		return err
	}
	labeled := &Error{Kind: KindOf(err), Msg: fmt.Sprintf("第 %d/%d 段视频生成失败：%s", index+1, count, err.Error()), Err: err, RetryAfter: RetryAfterOf(err)}
	if index == 0 && Retryable(err) {
		return labeled
	}
	return &NoRetry{Err: labeled}
}

// lastFrame writes the frame shown at length seconds (the clip's end, or the
// point it will be cut at) to out as PNG and returns it.
func lastFrame(ctx context.Context, ffmpeg, in string, length float64, out string) ([]byte, error) {
	seek := max(length-1, 0)
	if err := runFFmpeg(ctx, ffmpeg,
		"-ss", seconds(seek), "-i", in, "-t", seconds(length-seek),
		"-an", "-update", "1", "-f", "image2", "-c:v", "png", out,
	); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > MaxRefImageBytes {
		return nil, fmt.Errorf("frame is %d bytes", len(data))
	}
	return data, nil
}

// joinJob is what both join paths need: each segment's length, whether it
// has sound, and the frame size of the joined video.
type joinJob struct {
	segments []Media
	lengths  []float64
	audio    []bool
	w, h     int
}

func newJoinJob(ctx context.Context, ffmpeg, dir string, segments []Media, lengths []float64) *joinJob {
	j := &joinJob{segments: segments, lengths: lengths, audio: make([]bool, len(segments)), w: segments[0].Width, h: segments[0].Height}
	if j.w <= 0 || j.h <= 0 {
		j.w, j.h = 1280, 720
	}
	j.w, j.h = j.w&^1, j.h&^1
	for i, m := range segments {
		j.audio[i] = hasAudio(ctx, ffmpeg, filepath.Join(dir, m.Filename))
	}
	return j
}

func (j *joinJob) total() float64 {
	sum := 0.0
	for _, l := range j.lengths {
		sum += l
	}
	return sum
}

// args returns ffmpeg arguments (without the binary) that cut each input to
// its length, scale it to the job's frame size, and concatenate them into one
// H.264/AAC MP4. Segments without sound get silence so the audio stays in
// sync.
func (j *joinJob) args(inputs []string, out string) []string {
	anyAudio := false
	for _, a := range j.audio {
		anyAudio = anyAudio || a
	}
	var args []string
	var graph, joined strings.Builder
	for i, in := range inputs {
		args = append(args, "-i", in)
		d := seconds(j.lengths[i])
		fmt.Fprintf(&graph, "[%d:v]trim=duration=%s,setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,format=yuv420p[v%d];", i, d, j.w, j.h, j.w, j.h, i)
		fmt.Fprintf(&joined, "[v%d]", i)
		if !anyAudio {
			continue
		}
		if j.audio[i] {
			fmt.Fprintf(&graph, "[%d:a]atrim=duration=%s,asetpts=PTS-STARTPTS,aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,apad,atrim=duration=%s[a%d];", i, d, d, i)
		} else {
			fmt.Fprintf(&graph, "anullsrc=r=48000:cl=stereo,atrim=duration=%s,aformat=sample_fmts=fltp[a%d];", d, i)
		}
		fmt.Fprintf(&joined, "[a%d]", i)
	}
	audio := 0
	if anyAudio {
		audio = 1
	}
	fmt.Fprintf(&graph, "%sconcat=n=%d:v=1:a=%d[v]", joined.String(), len(inputs), audio)
	if anyAudio {
		graph.WriteString("[a]")
	}
	args = append(args, "-filter_complex", graph.String(), "-map", "[v]")
	if anyAudio {
		args = append(args, "-map", "[a]", "-c:a", "aac", "-b:a", "160k")
	}
	return append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p", "-movflags", "+faststart", out)
}

// joinLocally joins the job's segments with local ffmpeg.
func joinLocally(ctx context.Context, ffmpeg, dir string, j *joinJob) (*Media, error) {
	inputs := make([]string, len(j.segments))
	for i, m := range j.segments {
		inputs[i] = filepath.Join(dir, m.Filename)
	}
	tmp := filepath.Join(dir, ".join-"+randHex(8)+".mp4")
	defer os.Remove(tmp)
	if err := runFFmpeg(ctx, ffmpeg, j.args(inputs, tmp)...); err != nil {
		return nil, err
	}
	f, err := os.Open(tmp)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	saved := describeMedia(f, info.Size(), "video/mp4", mediaRef{Kind: "video", Path: tmp})
	f.Close()
	if err := os.Chmod(tmp, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, filepath.Join(dir, saved.Filename)); err != nil {
		return nil, err
	}
	saved.Source = ""
	return saved, nil
}

// joinOnVM asks the account's VM to run the join with its own ffmpeg, then
// downloads the result. The segments were generated on the same VM, so their
// workspace paths are known (Media.Source). The turn uses the "allow"
// approval policy, because the agent needs a shell; the prompt contains only
// the fixed command, never user text.
func (e *ProtoEngine) joinOnVM(ctx context.Context, acc *Account, j *joinJob) (*Media, error) {
	inputs := make([]string, len(j.segments))
	for i, m := range j.segments {
		if m.Source == "" {
			return nil, errf(KindTransient, "第 %d 段视频没有 VM 路径", i+1)
		}
		inputs[i] = "/" + m.Source
	}
	name := "joined-" + randHex(8) + ".mp4"
	out := path.Join(path.Dir(inputs[0]), name)
	prompt := vmJoinPrompt(append([]string{"ffmpeg", "-hide_banner", "-y"}, j.args(inputs, out)...), inputs, out)

	a, err := e.acquire(ctx, acc)
	if err != nil {
		return nil, err
	}
	defer e.release(acc.ID, a)
	ctx, cancel := context.WithTimeout(ctx, vmJoinTimeout)
	defer cancel()
	turn, err := e.runTurn(ctx, a, prompt, nil, "video", "allow", nil, nil)
	e.persist(acc.ID, a.protoAcct, nil)
	if err != nil {
		return nil, err
	}
	var result *mediaRef
	for i := range turn.media {
		if path.Base(turn.media[i].Path) == name || strings.HasSuffix(turn.media[i].URL, "/"+name) {
			result = &turn.media[i]
		}
	}
	if result == nil {
		return nil, errf(KindTransient, "VM 没有返回拼接后的视频：%s", clip(turn.text, 200))
	}
	files, err := e.fetchAll(ctx, a.conn, acc, []mediaRef{*result})
	if len(files) == 0 {
		return nil, wrap(KindTransient, "下载拼接后的视频失败", err)
	}
	joined := files[0]
	if err := checkJoined(joined, j); err != nil {
		_ = os.Remove(filepath.Join(e.cfg.MediaDir(), joined.Filename))
		return nil, err
	}
	return &joined, nil
}

// checkJoined rejects a joined video whose length or size is not what the
// command should have produced.
func checkJoined(m Media, j *joinJob) error {
	want := j.total()
	if m.Kind != "video" || m.Seconds <= 0 || math.Abs(m.Seconds-want) > max(1, want*0.05) {
		return errf(KindTransient, "拼接后的视频时长 %.1f 秒，应为 %.1f 秒", m.Seconds, want)
	}
	if m.Width != j.w || m.Height != j.h {
		return errf(KindTransient, "拼接后的视频尺寸 %dx%d，应为 %dx%d", m.Width, m.Height, j.w, j.h)
	}
	return nil
}

func vmJoinPrompt(command, inputs []string, out string) string {
	var b strings.Builder
	b.WriteString("Run one shell command in your Linux sandbox to join video files. Do not use your video generation tool and do not create any new footage.\n\n")
	b.WriteString("Input files, in order:\n")
	for i, in := range inputs {
		fmt.Fprintf(&b, "%d. %s\n", i+1, in)
	}
	b.WriteString("\nIf an input is not at that path, find the file with the same name in your workspace and use its path instead. If ffmpeg is missing, install it first.\n\n")
	b.WriteString("Run exactly this command:\n\n```sh\n")
	b.WriteString(shellJoin(command))
	b.WriteString("\n```\n\n")
	fmt.Fprintf(&b, "When it finishes, reply with only this link to the result: sandbox://%s\n", strings.TrimPrefix(out, "/"))
	b.WriteString("Do not mention or link the input files. If the command fails, reply with its error message.")
	return b.String()
}

// shellJoin quotes each argument for a POSIX shell.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./+=:", r))
		}) < 0 {
			quoted[i] = arg
			continue
		}
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// hasAudio reports whether ffmpeg lists an audio stream in the file.
func hasAudio(ctx context.Context, ffmpeg, path string) bool {
	cmd := ffmpegCommand(ctx, ffmpeg, "-hide_banner", "-i", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // exits non-zero without an output file
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(line, "Stream #") && strings.Contains(line, "Audio:") {
			return true
		}
	}
	return false
}

func runFFmpeg(ctx context.Context, ffmpeg string, args ...string) error {
	cmd := ffmpegCommand(ctx, ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 500 {
			msg = msg[len(msg)-500:]
		}
		return errors.Join(err, errors.New(msg))
	}
	return nil
}

// ffmpegCommand runs ffmpeg at low CPU priority where nice exists, so a join
// does not slow down the service that hosts the SDK.
func ffmpegCommand(ctx context.Context, ffmpeg string, args ...string) *exec.Cmd {
	if runtime.GOOS != "windows" {
		if nice, err := exec.LookPath("nice"); err == nil {
			return exec.CommandContext(ctx, nice, append([]string{"-n", "15", ffmpeg}, args...)...)
		}
	}
	return exec.CommandContext(ctx, ffmpeg, args...)
}

func seconds(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
