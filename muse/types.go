// Package muse implements the Muse HTTPS bootstrap and browser-free chat/media
// protocol over a multiplexed Noise/Hatch connection.
package muse

import (
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Config configures the protocol client. Its zero value uses the public Muse
// endpoints, UTC, local ./data/media output, and automatic denial of approvals.
// Custom endpoints are intended for trusted integrations and local tests.
type Config struct {
	SiteURL         string
	GatewayURL      string
	Proxy           string
	DataDir         string
	Timezone        string
	ApprovalPolicy  string // "deny" (default), "allow", or "off"
	ChatTimeout     time.Duration
	GenerateTimeout time.Duration
	IdleClose       time.Duration
	FFmpegPath      string // used by GenerateLongVideo; empty looks up "ffmpeg" on PATH
	// FFmpegConcurrency caps local ffmpeg processes across the client.
	// Defaults to 1.
	FFmpegConcurrency int
}

func (c Config) withDefaults() Config {
	if c.SiteURL == "" {
		c.SiteURL = "https://muse.ai"
	}
	c.SiteURL = strings.TrimRight(c.SiteURL, "/")
	if c.GatewayURL == "" {
		c.GatewayURL = "wss://hatch.metaaivm.com/v1/noise"
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if c.ApprovalPolicy == "" {
		c.ApprovalPolicy = "deny"
	}
	if c.ChatTimeout <= 0 {
		c.ChatTimeout = 3 * time.Minute
	}
	if c.GenerateTimeout <= 0 {
		c.GenerateTimeout = 10 * time.Minute
	}
	if c.IdleClose <= 0 {
		c.IdleClose = 10 * time.Minute
	}
	if c.FFmpegConcurrency <= 0 {
		c.FFmpegConcurrency = 1
	}
	return c
}

// MediaDir returns the directory in which Chat and Generate save downloaded
// media. Directories and files are created with owner-only permissions.
func (c Config) MediaDir() string { return filepath.Join(c.withDefaults().DataDir, "media") }

// NewClient creates a protocol client with no logging or persistence of account
// credentials. Call Close when finished; run Janitor(ctx) to evict idle accounts.
func NewClient(cfg Config) *ProtoEngine { return NewProtoEngine(&cfg, nil) }

// Account is a caller-owned credential snapshot. Use a stable unique ID for each
// account. Do not mutate the snapshot or its maps while calls are in progress;
// apply cookie rotations to a new snapshot via SetHooks.
type Account struct {
	ID            string            `json:"id"`
	Label         string            `json:"label,omitempty"`
	Cookies       map[string]string `json:"-"`
	CookieExpires map[string]int64  `json:"-"`
	Proxy         string            `json:"-"` // empty inherits Config.Proxy; "direct" bypasses proxies
}

const (
	MaxRefImages     = 10
	MaxRefImageBytes = 10 << 20
	MaxRefTotalBytes = 24 << 20
)

type RefImage struct {
	Data []byte
	Mime string
}

// GenRequest describes one generation turn. N caps returned outputs; it is not
// an upstream native count parameter. Express the desired count in Prompt too.
// Timeout defaults to Config.GenerateTimeout.
type GenRequest struct {
	Prompt     string
	Kind       string // "image" or "video"
	Refs       []RefImage
	N          int
	Timeout    time.Duration
	OnProgress func(pct int)
}

// Media describes an output saved in Config.MediaDir(). Measurements describe
// the actual file and may differ from dimensions requested in the prompt.
type Media struct {
	Filename string  `json:"filename"`
	Kind     string  `json:"kind"`
	Mime     string  `json:"mime"`
	Bytes    int64   `json:"bytes"`
	Width    int     `json:"width,omitempty"`
	Height   int     `json:"height,omitempty"`
	Seconds  float64 `json:"seconds,omitempty"`
	// Source is the file's path in the account's VM workspace, when the
	// file was downloaded from there.
	Source string `json:"source,omitempty"`
}

type ChatResult struct {
	Text  string
	Media []Media
}

func validateRequest(prompt string, refs []RefImage) error {
	if strings.TrimSpace(prompt) == "" {
		return BadRequest("prompt is required")
	}
	if len(refs) > MaxRefImages {
		return BadRequest("too many reference images")
	}
	total := 0
	for _, r := range refs {
		if len(r.Data) == 0 || len(r.Data) > MaxRefImageBytes || !strings.HasPrefix(r.Mime, "image/") {
			return BadRequest("invalid reference image MIME type or size")
		}
		total += len(r.Data)
	}
	if total > MaxRefTotalBytes {
		return BadRequest("reference images exceed total size limit")
	}
	return nil
}

// Validate checks endpoint syntax and configuration without contacting upstream.
func (c Config) Validate() error {
	c = c.withDefaults()
	if err := validateEndpoint(c.SiteURL, false); err != nil {
		return err
	}
	if err := validateEndpoint(c.GatewayURL, true); err != nil {
		return err
	}
	switch c.ApprovalPolicy {
	case "deny", "allow", "off":
	default:
		return BadRequest("invalid approval policy")
	}
	return nil
}

func validateEndpoint(raw string, gateway bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
		return BadRequest("invalid protocol endpoint")
	}
	secure, insecure := "https", "http"
	if gateway {
		secure, insecure = "wss", "ws"
	} else if u.Path != "" && u.Path != "/" {
		return BadRequest("site URL must be an origin")
	}
	if u.Scheme == secure {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == insecure && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return BadRequest("protocol endpoints require TLS except on loopback")
}
