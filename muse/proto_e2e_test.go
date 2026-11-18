package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CoolkHz/muse-protocol-go/internal/fakemuse"
)

func TestGenerateResumesCheckpointWithoutSubmittingAgain(t *testing.T) {
	for _, stopAt := range []string{"session", "media"} {
		t.Run(stopAt, func(t *testing.T) {
			site, gw := fakemuse.NewWithGateway()
			defer site.Close()
			cfg := Config{DataDir: t.TempDir(), SiteURL: site.URL, GatewayURL: "ws" + strings.TrimPrefix(site.URL, "http") + "/v1/noise"}
			eng := NewClient(cfg)
			defer eng.Close()
			acc := &Account{ID: "resume", Cookies: map[string]string{"hatch_sess": "s1"}}
			state := &GenerationState{}
			var saved []byte
			interrupted := errors.New("simulated process interruption")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := eng.Generate(ctx, acc, GenRequest{Prompt: "draw a cat", Kind: "image", State: state, SaveState: func(s *GenerationState) error {
				saved, _ = json.Marshal(s)
				if (stopAt == "session" && s.MessageID != "") || (stopAt == "media" && len(s.Media) > 0) {
					return interrupted
				}
				return nil
			}})
			if !errors.Is(err, interrupted) {
				t.Fatalf("checkpoint interruption: %v", err)
			}
			before := gw.SubmittedTurns()
			eng.Close()
			restarted := NewClient(cfg)
			defer restarted.Close()
			var restored GenerationState
			if err := json.Unmarshal(saved, &restored); err != nil {
				t.Fatal(err)
			}
			unavailable := NewClient(cfg)
			_, reconnectErr := unavailable.Generate(ctx, &Account{ID: acc.ID, Cookies: map[string]string{"hatch_sess": "nope"}}, GenRequest{Prompt: "draw a cat", Kind: "image", State: &restored})
			unavailable.Close()
			if reconnectErr == nil || Retryable(reconnectErr) {
				t.Fatalf("reconnection failure must not allow a fresh generation: %v", reconnectErr)
			}
			files, err := restarted.Generate(ctx, acc, GenRequest{Prompt: "draw a cat", Kind: "image", State: &restored})
			if err != nil || len(files) != 1 {
				t.Fatalf("resume: %v, files=%d", err, len(files))
			}
			if gw.SubmittedTurns() != before {
				t.Fatal("checkpoint recovery submitted a second generation")
			}
		})
	}
}

func TestProtoEngineAgainstFakeGateway(t *testing.T) {
	site, gw := fakemuse.NewWithGateway()
	defer site.Close()
	previousTextSettle, previousMediaSettle := textSettle, mediaSettle
	textSettle, mediaSettle = 300*time.Millisecond, time.Second
	t.Cleanup(func() { textSettle, mediaSettle = previousTextSettle, previousMediaSettle })

	cfg := &Config{DataDir: t.TempDir(), SiteURL: site.URL, GatewayURL: "ws" + strings.TrimPrefix(site.URL, "http") + "/v1/noise",
		ChatTimeout: 20 * time.Second, IdleClose: time.Minute, ApprovalPolicy: "deny", Timezone: "UTC"}
	eng := NewProtoEngine(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer eng.Close()
	var mu sync.Mutex
	var rotated map[string]string
	eng.SetHooks(func(_ string, c map[string]string, _ map[string]int64) { mu.Lock(); rotated = c; mu.Unlock() }, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	acc := &Account{ID: "a1", Label: "t", Cookies: map[string]string{"hatch_sess": "s1"}, CookieExpires: map[string]int64{}}

	want := "Hello **world**, refs=0. Here is code:\n\n```go\nfmt.Println(\"hi\")\n```"
	var deltas []string
	res, err := eng.Chat(ctx, acc, "say hello", nil, func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if res.Text != want || strings.Join(deltas, "") != want || len(deltas) < 3 {
		t.Fatalf("text %q, %d deltas %q", res.Text, len(deltas), deltas)
	}
	mu.Lock()
	if rotated["hatch_vml"] != "rotated" {
		t.Fatalf("rotated cookies not reported: %v", rotated)
	}
	mu.Unlock()
	if !eng.IsWarm("a1") {
		t.Fatal("connection should stay warm")
	}

	// Several turns multiplexed over the same encrypted connection.
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := eng.Chat(ctx, acc, "describe", []RefImage{{Data: fakemuse.PNG(), Mime: "image/png"}}, nil)
			if err == nil && !strings.Contains(r.Text, "refs=1") {
				err = io.ErrUnexpectedEOF
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("parallel chat: %v", err)
		}
	}

	media, err := eng.Generate(ctx, acc, GenRequest{Prompt: "draw a cat", Kind: "image", Timeout: 20 * time.Second})
	if err != nil || len(media) != 1 || media[0].Kind != "image" || media[0].Mime != "image/png" {
		t.Fatalf("generate: %v %+v", err, media)
	}
	saved, _ := os.ReadFile(cfg.MediaDir() + "/" + media[0].Filename)
	if !bytes.Equal(saved, fakemuse.PNG()) {
		t.Fatal("downloaded media differs")
	}

	start := time.Now()
	if _, err := eng.Generate(ctx, acc, GenRequest{Prompt: "refuse please", Kind: "image", Timeout: 20 * time.Second}); KindOf(err) != KindRefusal {
		t.Fatalf("refusal: %v", err)
	} else if time.Since(start) > 5*time.Second {
		t.Fatalf("refusal took %s; should not wait for media", time.Since(start))
	}

	// The agent blocks on a permission prompt until we deny it.
	if _, err := eng.Chat(ctx, acc, "please fetch the web", nil, nil); err != nil {
		t.Fatalf("approval chat: %v", err)
	}
	if len(gw.Decisions) != 1 || !strings.HasSuffix(gw.Decisions[0], "=deny") {
		t.Fatalf("decisions %v", gw.Decisions)
	}

	diag, _ := eng.Diagnose(ctx, acc)
	if diag["vm_assigned"] != true || diag["notary_token"] != true {
		t.Fatalf("diagnose %v", diag)
	}

	bad := &Account{ID: "a2", Cookies: map[string]string{"hatch_sess": "nope"}, CookieExpires: map[string]int64{}}
	if _, err := eng.Chat(ctx, bad, "x", nil, nil); KindOf(err) != KindAuth {
		t.Fatalf("bad cookie should be KindAuth, got %v", err)
	}
}
