package muse

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClientRejectsInvalidInputsBeforeConnecting(t *testing.T) {
	client := NewClient(Config{})
	defer client.Close()
	if _, err := client.Chat(context.Background(), nil, "hello", nil, nil); KindOf(err) != KindBadRequest {
		t.Fatalf("nil account: %v", err)
	}
	account := &Account{ID: "synthetic-account", Cookies: map[string]string{"hatch_sess": "synthetic-cookie"}}
	if _, err := client.Generate(context.Background(), account, GenRequest{Prompt: "hello", Kind: "audio"}); KindOf(err) != KindBadRequest {
		t.Fatalf("invalid kind: %v", err)
	}
	if _, err := client.Chat(context.Background(), account, "", nil, nil); KindOf(err) != KindBadRequest {
		t.Fatalf("empty prompt: %v", err)
	}
	if _, err := client.Chat(context.Background(), account, "hello", []RefImage{{Data: []byte("x"), Mime: "text/plain"}}, nil); KindOf(err) != KindBadRequest {
		t.Fatalf("invalid image: %v", err)
	}
	tenMaxImages := make([]RefImage, MaxRefImages)
	for i := range tenMaxImages {
		tenMaxImages[i] = RefImage{Data: make([]byte, MaxRefImageBytes), Mime: "image/png"}
	}
	if err := validateRequest("hello", tenMaxImages); err != nil {
		t.Fatalf("10x10MiB reference images rejected: %v", err)
	}
}

func TestInvalidEndpointsFailBeforeRequestConstruction(t *testing.T) {
	for _, site := range []string{"http://example.com", "https://user:secret@example.com", "://bad", "https://example.com?token=secret", "https://example.com/path"} {
		if err := (Config{SiteURL: site}).Validate(); err == nil {
			t.Errorf("accepted site %q", site)
		}
		if _, err := RenewSession(context.Background(), site, nil, "direct", false); err == nil {
			t.Errorf("renew accepted site %q", site)
		}
	}
	for _, gateway := range []string{"ws://example.com/noise", "https://example.com/noise", "wss://example.com/noise?token=secret"} {
		if err := (Config{GatewayURL: gateway}).Validate(); err == nil {
			t.Errorf("accepted gateway %q", gateway)
		}
	}
	if err := (Config{SiteURL: "http://127.0.0.1:1234", GatewayURL: "ws://localhost:1234/noise"}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsAndUpstreamBodiesStayOutOfJSONAndErrorText(t *testing.T) {
	const secret = "synthetic-private-value"
	account := &Account{ID: "account", Cookies: map[string]string{"hatch_sess": secret}, CookieExpires: map[string]int64{secret: 1}, Proxy: "http://user:" + secret + "@example.com"}
	encoded, err := json.Marshal(account)
	if err != nil || strings.Contains(string(encoded), secret) {
		t.Fatalf("unsafe account serialization: %s %v", encoded, err)
	}
	cause := errors.New(secret)
	wrapped := wrap(KindTransient, "request failed", cause)
	if strings.Contains(wrapped.Error(), secret) || !errors.Is(wrapped, cause) {
		t.Fatal("error leaked detail or lost cause")
	}
	upstream := classifyHTTPError("/test", 429, `{"secret":"`+secret+`"}`, nil)
	if strings.Contains(upstream.Error(), secret) || KindOf(upstream) != KindRateLimit {
		t.Fatal("upstream body leaked or classification lost")
	}
}

func TestClientCopiesAndDefaultsConfig(t *testing.T) {
	cfg := Config{}
	client := NewProtoEngine(&cfg, nil)
	defer client.Close()
	cfg.SiteURL = "http://example.com"
	if client.cfg.SiteURL != "https://muse.ai" || client.cfg.ChatTimeout != 3*time.Minute || client.cfg.GenerateTimeout != 10*time.Minute || client.cfg.ApprovalPolicy != "deny" {
		t.Fatalf("incorrect defaults: %+v", client.cfg)
	}
}
