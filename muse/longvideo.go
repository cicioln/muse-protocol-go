package muse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MaxVideoSegmentSeconds is the longest clip Muse makes in one turn. Longer
// videos are generated as consecutive segments and joined.
const MaxVideoSegmentSeconds = 10

// MaxLongVideoSeconds caps LongVideoRequest.Seconds.
const MaxLongVideoSeconds = 300

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
		return "", BadRequest("ffmpeg is required to join long videos: %v", err)
	}
	return path, nil
}

// GenerateLongVideo makes a video of r.Seconds by generating segments in turn,
// each starting from the last frame of the one before, and joins them with
// ffmpeg. Segments longer than planned are cut to their planned length. The
// joined video is saved in Config.MediaDir(); the segments are removed.
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
	out, err := joinVideos(ctx, ffmpeg, dir, segments, lengths)
	if err != nil {
		return nil, &NoRetry{Err: wrap(KindTransient, "拼接视频失败", err)}
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

// joinVideos cuts each segment to its length, scales it to the first
// segment's size, and concatenates them into one H.264/AAC MP4. Segments
// without sound get silence so the audio track stays in sync.
func joinVideos(ctx context.Context, ffmpeg, dir string, segments []Media, lengths []float64) (*Media, error) {
	w, h := segments[0].Width, segments[0].Height
	if w <= 0 || h <= 0 {
		w, h = 1280, 720
	}
	w, h = w&^1, h&^1
	withAudio := make([]bool, len(segments))
	anyAudio := false
	for i, m := range segments {
		withAudio[i] = hasAudio(ctx, ffmpeg, filepath.Join(dir, m.Filename))
		anyAudio = anyAudio || withAudio[i]
	}
	var args []string
	var graph strings.Builder
	var joined strings.Builder
	for i, m := range segments {
		args = append(args, "-i", filepath.Join(dir, m.Filename))
		d := seconds(lengths[i])
		fmt.Fprintf(&graph, "[%d:v]trim=duration=%s,setpts=PTS-STARTPTS,scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,setsar=1,format=yuv420p[v%d];", i, d, w, h, w, h, i)
		fmt.Fprintf(&joined, "[v%d]", i)
		if !anyAudio {
			continue
		}
		if withAudio[i] {
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
	fmt.Fprintf(&graph, "%sconcat=n=%d:v=1:a=%d[v]", joined.String(), len(segments), audio)
	if anyAudio {
		graph.WriteString("[a]")
	}
	tmp := filepath.Join(dir, ".join-"+randHex(8)+".mp4")
	defer os.Remove(tmp)
	args = append(args, "-filter_complex", graph.String(), "-map", "[v]")
	if anyAudio {
		args = append(args, "-map", "[a]", "-c:a", "aac", "-b:a", "160k")
	}
	args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p", "-movflags", "+faststart", tmp)
	if err := runFFmpeg(ctx, ffmpeg, args...); err != nil {
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
	return saved, nil
}

// hasAudio reports whether ffmpeg lists an audio stream in the file.
func hasAudio(ctx context.Context, ffmpeg, path string) bool {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-i", path)
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
	cmd := exec.CommandContext(ctx, ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y"}, args...)...)
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

func seconds(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }
