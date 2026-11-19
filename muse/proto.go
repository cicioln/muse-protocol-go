package muse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/CoolkHz/muse-protocol-go/hatch"
	"github.com/CoolkHz/muse-protocol-go/internal/netproxy"
)

// ProtoEngine talks to muse.ai without a browser: HTTPS bootstrap, then the
// Hatch gateway (Noise over WebSocket) for chat and files. Each account keeps
// one multiplexed connection while in use.
type ProtoEngine struct {
	cfg *Config
	log *slog.Logger

	mu     sync.Mutex
	accts  map[string]*protoAcct
	closed bool

	onCookies func(string, map[string]string, map[string]int64)
	onSession func(string, *SessionInfo)

	ffmpegGate chan struct{} // caps local ffmpeg processes
}

type protoAcct struct {
	connectGate chan struct{} // serialises connect, with cancellable waiting
	conn        *hatch.Conn
	site        *siteClient
	sess        *hatch.Session
	sessAt      time.Time
	nodeID      string
	cookieFP    string
	previousFP  string
	lastUsed    time.Time
	active      int
}

// protoLease pins the exact connection used by one operation. A later reconnect
// must never change the connection underneath an already submitted turn, except
// when this operation itself explicitly reconnects on disconnect.
type protoLease struct {
	*protoAcct
	acc  *Account
	mu   sync.Mutex
	conn *hatch.Conn
	sess *hatch.Session
}

func (l *protoLease) getConn() *hatch.Conn {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn
}

func (l *protoLease) setConn(conn *hatch.Conn, sess *hatch.Session) {
	l.mu.Lock()
	l.conn, l.sess = conn, sess
	l.mu.Unlock()
}

var quotaRe = regexp.MustCompile(`(?i)out of credits|usage limit|limit reached|token limit|weekly limit reached|额度不足|积分不足|达到上限|用量已达`)

var capabilities = []string{"chat_cancel", "delta_stream", "custom_reactions", "custom_reactions_facebook_thumbs_up_v1"}

func NewProtoEngine(cfg *Config, log *slog.Logger) *ProtoEngine {
	c := Config{}
	if cfg != nil {
		c = *cfg
	}
	c = c.withDefaults()
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ProtoEngine{cfg: &c, log: log, accts: map[string]*protoAcct{}, ffmpegGate: make(chan struct{}, c.FFmpegConcurrency)}
}

func (e *ProtoEngine) SetHooks(onCookies func(string, map[string]string, map[string]int64), onSession func(string, *SessionInfo)) {
	e.onCookies, e.onSession = onCookies, onSession
}

func (e *ProtoEngine) Name() string                    { return "protocol (Noise/Hatch)" }
func (e *ProtoEngine) Start(ctx context.Context) error { return ctx.Err() }

func (e *ProtoEngine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	for id, a := range e.accts {
		if a.conn != nil {
			a.conn.Close()
		}
		a.site.client.CloseIdleConnections()
		delete(e.accts, id)
	}
}

func (e *ProtoEngine) IsWarm(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.accts[id]
	return a != nil && a.conn != nil && a.conn.Alive()
}

func (e *ProtoEngine) WarmCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, a := range e.accts {
		if a.conn != nil && a.conn.Alive() {
			n++
		}
	}
	return n
}

func (e *ProtoEngine) Drop(id string) {
	e.mu.Lock()
	a := e.accts[id]
	delete(e.accts, id)
	if a != nil {
		if a.conn != nil {
			a.conn.Close()
		}
		a.site.client.CloseIdleConnections()
	}
	e.mu.Unlock()
}

func (e *ProtoEngine) Janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		e.mu.Lock()
		for id, a := range e.accts {
			if a.active == 0 && time.Since(a.lastUsed) > e.cfg.IdleClose {
				if a.conn != nil {
					a.conn.Close()
				}
				a.site.client.CloseIdleConnections()
				delete(e.accts, id)
			}
		}
		e.mu.Unlock()
	}
}

func (e *ProtoEngine) proxyFor(acc *Account) string {
	setting := netproxy.Effective(acc.Proxy, e.cfg.Proxy)
	route, err := netproxy.Resolve(setting, e.cfg.SiteURL)
	if err != nil {
		if setting == "" {
			return "invalid://proxy"
		}
		return setting // downstream validation fails closed
	}
	return route
}

// wsProxy resolves the route for the gateway itself, so NO_PROXY and
// HTTPS_PROXY rules that name the gateway host apply to the WebSocket.
func (e *ProtoEngine) wsProxy(acc *Account) string {
	setting := netproxy.Effective(acc.Proxy, e.cfg.Proxy)
	route, err := netproxy.Resolve(setting, e.cfg.GatewayURL)
	if err != nil {
		if setting == "" {
			return "invalid://proxy"
		}
		return setting // the dialer fails closed on invalid configuration
	}
	return route
}

// acquire returns the account's live gateway connection.
func (e *ProtoEngine) acquire(ctx context.Context, acc *Account) (*protoLease, error) {
	if err := e.cfg.Validate(); err != nil {
		return nil, err
	}
	if acc == nil || acc.ID == "" {
		return nil, BadRequest("account ID is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if miss := MissingCookies(acc.Cookies); len(miss) > 0 {
		return nil, errf(KindAuth, "账号缺少必要 Cookie: %s", strings.Join(miss, ", "))
	}
	fp := cookieFingerprint(acc)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, hatch.ErrClosed
	}
	a := e.accts[acc.ID]
	if a != nil && a.cookieFP != fp && !(a.active > 0 && a.previousFP == fp) {
		if a.conn != nil {
			a.conn.Close()
		}
		a.site.client.CloseIdleConnections()
		a = nil
	}
	if a == nil {
		a = &protoAcct{site: newSiteClient(e.cfg.SiteURL, e.proxyFor(acc), acc.Cookies), nodeID: newUUID(), cookieFP: fp, connectGate: make(chan struct{}, 1)}
		e.accts[acc.ID] = a
	}
	a.active++
	a.lastUsed = time.Now()
	e.mu.Unlock()
	failed := true
	defer func() {
		if failed {
			e.release(acc.ID, &protoLease{protoAcct: a})
		}
	}()
	conn, sess, err := e.ensureConn(ctx, acc.ID, acc, a)
	if err != nil {
		return nil, err
	}
	failed = false
	return &protoLease{protoAcct: a, acc: acc, conn: conn, sess: sess}, nil
}

func (e *ProtoEngine) ensureConn(ctx context.Context, id string, acc *Account, a *protoAcct) (*hatch.Conn, *hatch.Session, error) {
	select {
	case a.connectGate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-a.connectGate }()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	e.mu.Lock()
	valid := e.accts[id] == a && !e.closed
	conn := a.conn
	sess := a.sess
	e.mu.Unlock()
	if !valid {
		return nil, nil, hatch.ErrClosed
	}
	if conn != nil && conn.Alive() {
		return conn, sess, nil
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if a.sess == nil || attempt > 0 || time.Since(a.sessAt) > 3*time.Minute {
			bootSess, info, err := a.site.bootstrap(ctx)
			e.persist(id, a, info)
			if err != nil {
				return nil, nil, err
			}
			a.sess, a.sessAt = bootSess, time.Now()
		}
		e.mu.Lock()
		valid = e.accts[id] == a && !e.closed
		e.mu.Unlock()
		if !valid {
			a.site.client.CloseIdleConnections()
			return nil, nil, hatch.ErrClosed
		}
		conn, err := hatch.Connect(ctx, a.sess, hatch.Options{URL: e.cfg.GatewayURL, Proxy: e.wsProxy(acc), UserAgent: DefaultUA, Origin: e.cfg.SiteURL})
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			lastErr = err
			e.log.Warn("连接 hatch 网关失败", "attempt", attempt+1, "err", err)
			continue
		}
		e.mu.Lock()
		valid = e.accts[id] == a && !e.closed
		if valid {
			a.conn = conn
		}
		e.mu.Unlock()
		if !valid {
			conn.Close()
			a.site.client.CloseIdleConnections()
			return nil, nil, hatch.ErrClosed
		}
		e.registerNode(ctx, conn, a.nodeID)
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return conn, a.sess, nil
	}
	return nil, nil, wrap(KindTransient, "无法建立 hatch 加密连接", lastErr)
}

func (e *ProtoEngine) reconnectLease(ctx context.Context, lease *protoLease) (*hatch.Conn, error) {
	if lease == nil || lease.protoAcct == nil || lease.acc == nil {
		return nil, hatch.ErrClosed
	}
	conn, sess, err := e.ensureConn(ctx, lease.acc.ID, lease.acc, lease.protoAcct)
	if err != nil {
		return nil, err
	}
	lease.setConn(conn, sess)
	return conn, nil
}

func (e *ProtoEngine) release(id string, a *protoLease) {
	e.mu.Lock()
	a.active--
	a.lastUsed = time.Now()
	e.mu.Unlock()
}

func (e *ProtoEngine) persist(id string, a *protoAcct, info *SessionInfo) {
	e.mu.Lock()
	current := e.accts[id] == a && !e.closed
	e.mu.Unlock()
	if !current {
		return
	}
	if info != nil && e.onSession != nil {
		e.onSession(id, info)
	}
	if ck, ex, ok := a.site.Rotated(); ok && e.onCookies != nil {
		// Publish the fingerprint before the hook makes the new cookie visible.
		e.mu.Lock()
		a.previousFP = a.cookieFP
		a.cookieFP = ck["hatch_sess"] + "|" + strings.SplitN(a.cookieFP, "|", 2)[1]
		e.mu.Unlock()
		e.onCookies(id, ck, ex)
	}
}

func (e *ProtoEngine) registerNode(ctx context.Context, conn *hatch.Conn, nodeID string) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := conn.DoJSON(cctx, hatch.ServiceDaemon, http.MethodPost, "/api/nodes/register", map[string]any{
		"node_id": nodeID, "display_name": "Web", "platform": "web",
		"commands_v2": map[string]any{"ping": map[string]any{"description": "Connection liveness check"}},
	})
	if err != nil {
		e.log.Debug("节点注册失败（可忽略）", "err", err)
	}
}

// ---------------------------------------------------------------- turns

type mediaRef struct {
	Kind string
	Path string
	URL  string
	Mime string
}

func (m mediaRef) id() string {
	if m.Path != "" {
		return m.Path
	}
	return m.URL
}

type turnResult struct {
	text   string
	media  []mediaRef
	events []map[string]any
}

func imageItem(r RefImage, i int) map[string]any {
	ext := strings.TrimPrefix(pickExt(r.Mime, "", "image"), ".")
	return map[string]any{"type": "image", "mime_type": r.Mime, "data_base64": base64.StdEncoding.EncodeToString(r.Data),
		"filename": fmt.Sprintf("reference_%d.%s", i+1, ext)}
}

// runTurn sends one message in a fresh side thread and follows the reply via
// /chat/history until it settles.
// approval overrides Config.ApprovalPolicy for this turn when not empty.
func (e *ProtoEngine) runTurn(ctx context.Context, a *protoLease, prompt string, refs []RefImage, want, approval string, onDelta func(string), progress func(int)) (*turnResult, error) {
	return e.runTurnResumable(ctx, a, prompt, refs, want, approval, onDelta, progress, nil, nil)
}

func (e *ProtoEngine) runTurnResumable(ctx context.Context, a *protoLease, prompt string, refs []RefImage, want, approval string, onDelta func(string), progress func(int), state *GenerationState, save func(*GenerationState) error) (*turnResult, error) {
	conn := a.getConn()
	reconnect := func(rctx context.Context) (*hatch.Conn, error) {
		return e.reconnectLease(rctx, a)
	}
	if state != nil && state.SessionID != "" {
		result, err := e.followThreadWithReconnect(ctx, conn, state.SessionID, state.MessageID, prompt, want, approval, onDelta, progress, nil, reconnect)
		if err != nil {
			return nil, turnFailure(err)
		}
		return result, nil
	}
	refs = CompactRefImages(refs)
	var items []map[string]any
	for i, r := range refs {
		items = append(items, imageItem(r, i))
	}
	items = append(items, map[string]any{"type": "text", "text": prompt})
	sessionID := newUUID()
	if state != nil {
		state.SessionID = sessionID
		if save != nil {
			if err := save(state); err != nil {
				return nil, &NoRetry{Err: wrap(KindTransient, "保存生成会话失败", err)}
			}
		}
	}
	body, _ := json.Marshal(map[string]any{
		"items": items, "message": prompt, "node_id": a.nodeID, "capabilities": capabilities,
		"session_id": sessionID, "timezone": e.cfg.Timezone,
		"metadata": map[string]any{"thread_is_dictation_used": false},
	})
	st, err := conn.OpenContext(ctx, hatch.Request{Method: http.MethodPost, Path: "/chat/stream", Body: body})
	if err != nil {
		var notSent *hatch.NotSentError
		if errors.As(err, &notSent) && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
			if newConn, rerr := reconnect(ctx); rerr == nil {
				conn = newConn
				st, err = conn.OpenContext(ctx, hatch.Request{Method: http.MethodPost, Path: "/chat/stream", Body: body})
			}
		}
		if err != nil {
			return nil, &NoRetry{Err: wrap(KindTransient, "发送消息失败，送达状态未知", err)}
		}
	}
	defer st.Close()
	messageID := ""
	actx, cancel := context.WithTimeout(ctx, 25*time.Second)
	ack, err := st.NextJSON(actx)
	cancel()
	if code := st.Status(); code >= 400 {
		return nil, classifyHTTPError("/chat/stream", code, string(ack), st.Headers())
	}
	if err == nil {
		var m map[string]any
		_ = json.Unmarshal(ack, &m)
		messageID = str(m, "message_id")
		if messageID == "" {
			messageID = str(m, "id")
		}
	}
	if state != nil {
		state.MessageID = messageID
		if save != nil {
			if err := save(state); err != nil {
				return nil, &NoRetry{Err: wrap(KindTransient, "保存生成会话失败", err)}
			}
		}
	}
	// Drain the rest of the stream so its buffer stays small. Its records are
	// not parsed (the history is the source of truth) but they show when the
	// thread changes, which lets polling slow down while it does not.
	sig := newStreamSignal()
	go func() {
		defer sig.end()
		for {
			_, err := st.NextJSON(ctx)
			if err != nil {
				return
			}
			sig.record()
		}
	}()
	result, err := e.followThreadWithReconnect(ctx, conn, sessionID, messageID, prompt, want, approval, onDelta, progress, sig, reconnect)
	if err != nil {
		return nil, turnFailure(err)
	}
	return result, nil
}

var (
	textSettle   = 2 * time.Second
	mediaSettle  = 25 * time.Second
	pollInterval = 300 * time.Millisecond
	// Quiet time before a finished non-progress reply ends a media turn.
	imageTextOnlyGrace = 15 * time.Second
	videoTextOnlyGrace = 15 * time.Second
	// While the chat stream is open and quiet, nothing new is expected in the
	// history; a stream record wakes the poller early.
	streamQuietPoll = 1500 * time.Millisecond
	streamRecent    = time.Second
)

// streamSignal reports activity on the /chat/stream response.
type streamSignal struct {
	wake  chan struct{}
	last  atomic.Int64 // unix nanos of the latest record; 0 before the first
	ended atomic.Bool
}

func newStreamSignal() *streamSignal { return &streamSignal{wake: make(chan struct{}, 1)} }

func (s *streamSignal) record() {
	s.last.Store(time.Now().UnixNano())
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *streamSignal) end() { s.ended.Store(true) }

// pollDelay picks the wait before the next history poll. Without a live
// stream it keeps the plain backoff. With one, it polls at the base rate
// right after stream activity and slowly while the stream is quiet.
func pollDelay(idle time.Duration, s *streamSignal) time.Duration {
	d := backoff(pollInterval, idle)
	if s == nil || s.ended.Load() {
		return d
	}
	last := s.last.Load()
	if last == 0 {
		return d // the stream has not proven it carries updates
	}
	if time.Since(time.Unix(0, last)) < streamRecent {
		return pollInterval
	}
	return max(d, streamQuietPoll)
}

// waitPoll sleeps for d, returning early (but no sooner than pollInterval
// after the previous poll) when the chat stream reports activity.
func waitPoll(ctx context.Context, d time.Duration, polledAt time.Time, s *streamSignal) {
	var wake <-chan struct{}
	if s != nil {
		wake = s.wake
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
		return
	case <-wake:
	}
	if gap := pollInterval - time.Since(polledAt); gap > 0 {
		_ = sleep(ctx, gap)
	}
}

func (e *ProtoEngine) followThread(ctx context.Context, conn *hatch.Conn, sessionID, messageID, prompt, want, approval string, onDelta func(string), progress func(int), sig *streamSignal) (*turnResult, error) {
	return e.followThreadWithReconnect(ctx, conn, sessionID, messageID, prompt, want, approval, onDelta, progress, sig, nil)
}

func (e *ProtoEngine) followThreadWithReconnect(ctx context.Context, conn *hatch.Conn, sessionID, messageID, prompt, want, approval string, onDelta func(string), progress func(int), sig *streamSignal, reconnect func(context.Context) (*hatch.Conn, error)) (*turnResult, error) {
	if approval == "" {
		approval = e.cfg.ApprovalPolicy
	}
	if progress == nil {
		progress = func(int) {}
	}
	var (
		sent, last      string
		textStableAt    time.Time
		mediaSig        string
		mediaStableAt   time.Time
		nextApproval    time.Time
		res             = &turnResult{}
		start           = time.Now()
		found           bool
		consecutiveErr  int
		reconnectErrs   int
		totalReconnects int
		activitySig     string
		lastActivity    = time.Now()
		lastBody        []byte
		polledAt        time.Time
		// Derived from the latest history body; recomputed only when it changes.
		cur                             string
		done, inProgress, turnCompleted bool
		historyInProgress               bool
		commentary                      bool
		activity                        mediaActivityTracker
	)
	needle := strings.TrimSpace(prompt)
	if len(needle) > 80 {
		needle = needle[:80]
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, wrap(KindTransient, "等待回复中断", err)
		}
		if approval != "off" && time.Now().After(nextApproval) {
			nextApproval = time.Now().Add(4 * time.Second)
			e.handleApprovals(ctx, conn, sessionID, approval)
		}
		polledAt = time.Now()
		hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		resp, err := conn.Do(hctx, hatch.Request{Method: http.MethodGet,
			Path: "/chat/history?" + url.Values{"limit": {"60"}, "session_id": {sessionID}}.Encode()})
		cancel()
		if err != nil {
			// A wedged or half-open WebSocket stays Alive() while every history
			// poll times out; close it after two failures so the reconnect path
			// dials a fresh connection instead of hanging for 11 timeouts.
			if conn.Alive() && reconnect != nil && ctx.Err() == nil && consecutiveErr >= 1 {
				_ = conn.Close()
			}
			if !conn.Alive() {
				if reconnect == nil || ctx.Err() != nil {
					return nil, wrap(KindTransient, "hatch 连接中断", err)
				}
				totalReconnects++
				if totalReconnects > 20 {
					return nil, wrap(KindTransient, "hatch 连接中断", err)
				}
				newConn, rerr := reconnect(ctx)
				if rerr != nil {
					if ctx.Err() != nil {
						return nil, wrap(KindTransient, "等待回复中断", ctx.Err())
					}
					if k := KindOf(rerr); k != KindTransient && k != KindRateLimit {
						return nil, rerr
					}
					reconnectErrs++
					if reconnectErrs > 5 {
						return nil, wrap(KindTransient, "hatch 连接中断", rerr)
					}
					_ = sleep(ctx, backoff(time.Second, time.Duration(reconnectErrs)*2*time.Second))
					continue
				}
				e.log.Debug("hatch 连接中断，已重连并继续跟踪会话", "session_id", sessionID)
				conn = newConn
				sig = nil
				consecutiveErr = 0
				reconnectErrs = 0
				_ = sleep(ctx, 200*time.Millisecond)
				continue
			}
			if consecutiveErr++; consecutiveErr > 10 {
				_ = conn.Close()
				return nil, wrap(KindTransient, "读取对话历史失败", err)
			}
			_ = sleep(ctx, time.Second)
			continue
		}
		if resp.Status >= 500 || resp.Status == http.StatusNotFound {
			if consecutiveErr++; consecutiveErr > 12 {
				return nil, classifyHTTPError("/chat/history", resp.Status, string(resp.Body), resp.Headers)
			}
			_ = sleep(ctx, backoff(time.Second, time.Duration(consecutiveErr)*2*time.Second))
			continue
		}
		if resp.Status >= 400 {
			return nil, classifyHTTPError("/chat/history", resp.Status, string(resp.Body), resp.Headers)
		}
		consecutiveErr = 0
		reconnectErrs = 0
		// An unchanged body yields the same events, so only the time-based
		// settle checks below need to run again.
		if !found || !bytes.Equal(resp.Body, lastBody) {
			var data any
			_ = json.Unmarshal(resp.Body, &data)
			events := historyEvents(data)
			userIdx := -1
			for i, ev := range events {
				if messageID != "" && str(ev, "message_id") == messageID {
					userIdx = i
					break
				}
			}
			if userIdx < 0 {
				for i, ev := range events {
					if eventName(ev) == "message.user" && strings.Contains(eventText(ev), needle) {
						userIdx = i
					}
				}
			}
			if userIdx < 0 {
				if !found && time.Since(start) > 60*time.Second {
					return nil, errf(KindTransient, "消息未出现在对话记录中（可能未送达 VM）")
				}
				waitPoll(ctx, pollInterval, polledAt, sig)
				continue
			}
			if !found {
				progress(20) // the VM accepted the message
			}
			found = true
			// Only cache a body whose anchor was found. A truncated history
			// window must be rechecked, never paired with the previous reply.
			lastBody = resp.Body
			after := events[userIdx+1:]
			for i, ev := range after {
				if eventName(ev) == "message.user" {
					after = after[:i]
					break
				}
			}
			if act := fmt.Sprintf("%d|%s", len(after), eventsFingerprint(after)); act != activitySig {
				activitySig, lastActivity = act, time.Now()
			}
			res.events = after
			cur, done, inProgress, turnCompleted = "", false, false, false
			commentary = false
			historyInProgress = mediaHistoryInProgress(after)
			for _, ev := range after {
				switch eventName(ev) {
				case "message.assistant":
					p, _ := ev["payload"].(map[string]any)
					if str(ev, "reply_to_message_id") != "" || str(p, "reply_to_message_id") != "" {
						continue
					}
					if t := eventText(ev); want != "" {
						// A new root assistant can start streaming before it has
						// text. Advance its lifecycle and discard the older answer
						// instead of treating that answer as this turn's final text.
						cur, turnCompleted = t, false
						inProgress, done = mediaLifecycle(p)
						commentary = assistantCommentary(p)
					} else if t != "" {
						cur = t
						turnCompleted = false
						// While generating, metadata.streaming is true; a finished
						// message drops it and gains payload.status.
						done = assistantFinished(p)
						inProgress = assistantInProgress(p)
					}
				case "turn.completed":
					done, inProgress = true, false
					turnCompleted = true
				case "delta.message_done", "message.done":
					// A message boundary is not proof that a media tool or a
					// still-streaming/unknown assistant has finished the turn.
					if want == "" {
						done, inProgress = true, false
					}
				}
			}
			if want != "" {
				activity.observe(after, sessionID)
				res.text = cur // Includes clearing text replaced by an empty assistant.
				all := extractMedia(after)
				res.media = filterMedia(all, want)
			}
			if len(res.media) > 0 {
				progress(70)
			} else if cur != "" {
				progress(35)
			}
			if cur != "" {
				res.text = cur
				if onDelta != nil && strings.HasPrefix(cur, sent) && len(cur) > len(sent) {
					onDelta(cur[len(sent):])
					sent = cur
				}
				if cur != last {
					last, textStableAt = cur, time.Now()
				}
			}
		}
		textSettled := cur != "" && (done || (!inProgress && time.Since(textStableAt) > textSettle))

		if want == "" {
			if textSettled {
				return res, nil
			}
		} else {
			if len(res.media) == 0 {
				activity.poll(ctx, conn)
			}
			quietSince := lastActivity
			if sig != nil {
				if lastStream := time.Unix(0, sig.last.Load()); lastStream.After(quietSince) {
					quietSince = lastStream
				}
			}
			// An exhausted tool result is authoritative even when the assistant
			// never writes a final message. Do not mistake an intermediate tool
			// failure for a final one while another tool is still running.
			if len(res.media) == 0 && !historyInProgress && !activity.active && time.Since(quietSince) >= textSettle {
				if activity.failure != nil {
					return nil, activity.failure.error(want)
				}
				if failure := findToolFailure(res.events); failure != nil && failure.exhausted {
					return nil, failure.error(want)
				}
				if turnCompleted && cur == "" && !inProgress && !commentary {
					return res, nil
				}
			}
			if len(res.media) == 0 && mediaTextOnlySettled(cur, want, done, inProgress || commentary, historyInProgress || activity.active, turnCompleted, time.Since(quietSince)) {
				return res, nil
			}
			key := ""
			for _, m := range res.media {
				key += m.id() + ";"
			}
			if key != "" {
				if key != mediaSig {
					mediaSig, mediaStableAt = key, time.Now()
				}
				if textSettled || time.Since(mediaStableAt) > mediaSettle {
					return res, nil
				}
			}
		}
		delay := pollDelay(time.Since(lastActivity), sig)
		if want == "video" && pollInterval >= 300*time.Millisecond {
			delay = max(delay, time.Second)
		}
		waitPoll(ctx, delay, polledAt, sig)
		if want == "video" && pollInterval >= 300*time.Millisecond {
			if gap := time.Second - time.Since(polledAt); gap > 0 {
				_ = sleep(ctx, gap)
			}
		}
	}
}

// Text silence is not a terminal signal for media generation. In particular,
// video tools can run for minutes without changing the assistant's progress
// text. Unknown or active turns remain bounded by the caller's deadline.
func mediaTextOnlySettled(text, want string, done, assistantActive, historyActive, turnCompleted bool, quietFor time.Duration) bool {
	if text == "" || !done || assistantActive || historyActive || quietFor < textSettle {
		return false
	}
	if isRefusal(text) || classifyTurnErr(text) != nil {
		return true
	}
	// An assistant turn can finish after acknowledging delegated media work.
	// Its completion does not mean the video tool has produced a result.
	if isWorking(text) {
		return false
	}
	// A completed question/non-progress answer gets a grace period for tool
	// events or presentations to arrive; any history or stream update resets it.
	return turnCompleted || quietFor > textOnlyGrace(want)
}

// textOnlyGrace is how long a finished, non-progress reply must stay quiet
// before it counts as the whole answer. A video tool can render for minutes
// after a short acknowledgement without touching the history.
func textOnlyGrace(want string) time.Duration {
	if want == "video" {
		return videoTextOnlyGrace
	}
	return imageTextOnlyGrace
}

// Inspect explicit lifecycle fields rather than guessing tool names or URLs.
// Events with an identity share a lifecycle; otherwise only the same event
// type can clear its active state. An explicit turn completion clears earlier
// activity, while later active events reopen the wait.
func mediaHistoryInProgress(events []map[string]any) bool {
	active := make(map[string]bool)
	for _, ev := range events {
		name := eventName(ev)
		if name == "turn.completed" {
			clear(active)
			continue
		}
		p, _ := ev["payload"].(map[string]any)
		inProgress, completed := mediaLifecycle(p)
		if name == "message.assistant" {
			text := eventText(ev)
			// A final root answer can close older tool activity even when
			// history retains a stale pending entry and has no turn.completed.
			// A completed progress message or a subagent reply cannot do so.
			if completed && !inProgress && !assistantCommentary(p) && text != "" && (isRefusal(text) || classifyTurnErr(text) != nil || !isWorking(text)) &&
				str(ev, "reply_to_message_id") == "" && str(p, "reply_to_message_id") == "" {
				clear(active)
			}
			continue // The current assistant lifecycle is handled separately.
		}
		if name == "message.done" || name == "delta.message_done" {
			continue
		}
		key := toolEventID(ev)
		walkToolFailures(p, 0, func(f *toolFailure) {
			if f.exhausted {
				inProgress, completed = false, true
			}
		})
		switch {
		case inProgress:
			active[key] = true
		case completed:
			delete(active, key)
		}
	}
	return len(active) != 0
}

func mediaLifecycle(payload map[string]any) (active, completed bool) {
	md, _ := payload["metadata"].(map[string]any)
	if streaming, _ := md["streaming"].(bool); streaming {
		return true, false
	}
	state := strings.ToLower(str(payload, "status"))
	if state == "" {
		state = strings.ToLower(str(payload, "state"))
	}
	switch state {
	case "pending", "queued", "running", "in_progress", "streaming":
		return true, false
	case "completed", "done", "failed", "cancelled", "canceled", "error", "success", "succeeded":
		return false, true
	default:
		return false, false // An unknown status is not proof of completion.
	}
}

// backoff polls quickly while the thread changes and slows down to 2s while
// a long job (video) runs without visible progress.
func backoff(base, idle time.Duration) time.Duration {
	if idle < 10*time.Second {
		return base
	}
	d := base + (idle-10*time.Second)/10
	return min(d, 2*time.Second)
}

// Include tool payload changes and same-length text updates as activity too.
func eventsFingerprint(evs []map[string]any) string {
	body, _ := json.Marshal(evs) // Events came from JSON, so marshaling cannot fail.
	return fmt.Sprintf("%x", sha256.Sum256(body))
}

var (
	refusalMarkersRe = regexp.MustCompile(`(?i)(could ?n.?t|can.?t|cannot|unable to|not able to|won.?t|will not|fail(ed)? to)\s+(generate|create|make|depict|fulfill)|content policy|safety (policy|guidelines)|community (standards|guidelines)|real (people|individuals)|not available (for your account|in your)|country or region|out of credits|limit reached|tool is (currently )?unavailable|无法生成|生成失败|没生成成|未生成成功|不支持`)
	// Progress and acknowledgement replies ("Got it — animating the cat")
	// precede the media; they are not the final answer.
	workingRe = regexp.MustCompile(`(?i)search|looking|checking|finding|generating|creating|drawing|rendering|animating|producing|preparing|processing|composing|wait|working on|got it|on it|one moment|just a (moment|sec)|hang tight|coming (right )?up|let me|i('ll| will)|正在|生成中|制作中|处理中|稍等|请稍候|马上|这就|收到|我来|我会|我将`)
)

func assistantFinished(p map[string]any) bool {
	md, _ := p["metadata"].(map[string]any)
	if v, ok := md["streaming"].(bool); ok && v {
		return false
	}
	st := str(p, "status")
	return st != "" && st != "streaming" && st != "pending" && st != "in_progress"
}

func assistantInProgress(p map[string]any) bool {
	md, _ := p["metadata"].(map[string]any)
	if active, _ := md["streaming"].(bool); active {
		return true
	}
	switch str(p, "status") {
	case "streaming", "pending", "in_progress":
		return true
	}
	return false
}

// summarizeEvents describes event structure without message contents.
func summarizeEvents(evs []map[string]any) string {
	var parts []string
	for _, ev := range evs {
		p, _ := ev["payload"].(map[string]any)
		md, _ := p["metadata"].(map[string]any)
		pk := keysOf(p)
		sort.Strings(pk)
		parts = append(parts, fmt.Sprintf("{%s len=%d streaming=%v status=%v state=%v kind=%v payload_keys=%v md_keys=%v}",
			eventName(ev), len(eventText(ev)), md["streaming"], p["status"], p["state"], p["kind"], pk, sortedKeys(md)))
	}
	return strings.Join(parts, " ")
}

func sortedKeys(m map[string]any) []string {
	k := keysOf(m)
	sort.Strings(k)
	return k
}

func isRefusal(s string) bool { return refusalMarkersRe.MatchString(s) }
func isWorking(s string) bool { return workingRe.MatchString(s) }

func eventName(ev map[string]any) string {
	if n := str(ev, "event_name"); n != "" {
		return n
	}
	return str(ev, "event")
}

func asText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, it := range t {
			b.WriteString(asText(it))
		}
		return b.String()
	case map[string]any:
		for _, k := range []string{"text", "display_text", "content"} {
			if x, ok := t[k]; ok {
				return asText(x)
			}
		}
	}
	return ""
}

func eventText(ev map[string]any) string {
	for _, k := range []string{"display_text", "text"} {
		if t := asText(ev[k]); t != "" {
			return t
		}
	}
	if p, ok := ev["payload"].(map[string]any); ok {
		for _, k := range []string{"display_text", "text", "content", "delta"} {
			if t := asText(p[k]); t != "" {
				return t
			}
		}
	}
	return ""
}

func historyEvents(data any) []map[string]any {
	m, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	if r, ok := m["result"].(map[string]any); ok {
		m = r
	}
	for _, k := range []string{"chat_events", "events", "history"} {
		if list, ok := m[k].([]any); ok {
			out := make([]map[string]any, 0, len(list))
			for _, it := range list {
				if ev, ok := it.(map[string]any); ok {
					out = append(out, ev)
				}
			}
			return out
		}
	}
	return nil
}

var (
	sandboxRe  = regexp.MustCompile(`sandbox://[^\s)"'\]]+`)
	imageExtRe = regexp.MustCompile(`\.(png|jpe?g|webp|gif)(\?|$)`)
)

// extractMedia walks events for image/video artifacts (paths in the VM
// workspace or URLs).
func extractMedia(events []map[string]any) []mediaRef {
	seen := map[string]bool{}
	var out []mediaRef
	add := func(m mediaRef) {
		if m.id() != "" && !seen[m.id()] {
			seen[m.id()] = true
			out = append(out, m)
		}
	}
	var walk func(v any, hint string)
	walk = func(v any, hint string) {
		switch t := v.(type) {
		case map[string]any:
			kind := hint
			if k := str(t, "kind"); k == "image" || k == "video" {
				kind = k
			}
			path := str(t, "path")
			if path == "" {
				path = str(t, "image_path")
			}
			if path == "" {
				path = str(t, "video_path")
			}
			u := ""
			if vs, ok := t["variants"].(map[string]any); ok {
				u = str(vs, "original")
			}
			if u == "" {
				u = str(t, "url")
			}
			mime := str(t, "mime")
			if mime == "" {
				mime = str(t, "mime_type")
			}
			if mime == "" {
				mime = str(t, "mimeType")
			}
			mk := kind
			if strings.HasPrefix(mime, "video/") {
				mk = "video"
			} else if strings.HasPrefix(mime, "image/") {
				mk = "image"
			}
			if mk == "" {
				if p := strings.ToLower(path + u); isVideoURL(p) {
					mk = "video"
				} else if imageExtRe.MatchString(p) {
					mk = "image"
				}
			}
			if (path != "" || u != "") && (mk == "image" || mk == "video") {
				add(mediaRef{Kind: mk, Path: normPath(path), URL: u, Mime: mime})
			}
			for k, x := range t {
				switch k {
				case "path", "image_path", "video_path", "url", "variants", "mime", "mime_type", "mimeType":
					continue
				}
				walk(x, kind)
			}
		case []any:
			for _, x := range t {
				walk(x, hint)
			}
		case string:
			for _, m := range sandboxRe.FindAllString(t, -1) {
				walk(map[string]any{"path": m}, hint)
			}
		}
	}
	for _, ev := range events {
		p, _ := ev["payload"].(map[string]any)
		hint := ""
		if k := str(p, "kind"); k == "image" || k == "video" {
			hint = k
		}
		if eventName(ev) == "delta.presentation" || p["images"] != nil || p["videos"] != nil {
			walk(p, hint)
		}
		if r, ok := ev["resources"]; ok {
			walk(r, hint)
		}
		if eventName(ev) == "message.assistant" {
			walk(eventText(ev), hint)
		}
	}
	return out
}

func filterMedia(list []mediaRef, kind string) []mediaRef {
	var out []mediaRef
	for _, m := range list {
		if kind == "" || m.Kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func normPath(p string) string {
	p = strings.TrimPrefix(strings.TrimSpace(p), "sandbox://")
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return strings.TrimLeft(p, "/")
}

// handleApprovals answers permission prompts of this thread according to policy.
func (e *ProtoEngine) handleApprovals(ctx context.Context, conn *hatch.Conn, sessionID, policy string) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := conn.Do(cctx, hatch.Request{Method: http.MethodGet, Path: "/approvals", Service: hatch.ServiceSentinel})
	if err != nil || resp.Status >= 400 {
		return
	}
	var data any
	_ = json.Unmarshal(resp.Body, &data)
	var list []any
	switch t := data.(type) {
	case []any:
		list = t
	case map[string]any:
		for _, k := range []string{"approvals", "items", "pending", "result"} {
			if l, ok := t[k].([]any); ok {
				list = l
				break
			}
		}
	}
	for _, it := range list {
		rec, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if s := str(rec, "status"); s != "" && s != "pending" {
			continue
		}
		scope, _ := rec["scope"].(map[string]any)
		if scope == nil {
			scope = rec
		}
		if str(scope, "thread_id") != sessionID && str(scope, "session_scope_id") != sessionID {
			continue
		}
		id := str(rec, "approval_id")
		if id == "" {
			id = str(rec, "id")
		}
		decision := pickDecision(rec, policy)
		if id == "" || decision == "" {
			continue
		}
		r, err := conn.DoJSON(cctx, hatch.ServiceSentinel, http.MethodPost, "/approvals/"+url.PathEscape(id)+"/decide",
			map[string]any{"approval_id": id, "decision": decision})
		if err == nil && r.Status < 300 {
			e.log.Info("已处理权限请求", "decision", decision)
		}
	}
}

func pickDecision(rec map[string]any, policy string) string {
	offered := map[string]bool{}
	if opts, ok := rec["decision_options"].([]any); ok {
		for _, o := range opts {
			switch t := o.(type) {
			case string:
				offered[t] = true
			case map[string]any:
				offered[str(t, "kind")] = true
			}
		}
	}
	prefs := []string{"deny", "deny_for_this_task"}
	if policy == "allow" {
		prefs = []string{"allow_once", "allow", "allow_for_this_task", "allow_session"}
	}
	for _, p := range prefs {
		if len(offered) == 0 || offered[p] {
			return p
		}
	}
	return ""
}

const maxMediaBytes = 512 << 20

// fetchAll downloads artifacts a few at a time, keeping their order. It
// returns what succeeded and the last error.
func (e *ProtoEngine) fetchAll(ctx context.Context, lease *protoLease, list []mediaRef) ([]Media, error) {
	saved := make([]*Media, len(list))
	errs := make([]error, len(list))
	gate := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, m := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			saved[i], errs[i] = e.fetchMedia(ctx, lease, m)
		}()
	}
	wg.Wait()
	var out []Media
	var lastErr error
	for i := range list {
		if errs[i] != nil {
			lastErr = errs[i]
			continue
		}
		out = append(out, *saved[i])
	}
	return out, lastErr
}

// fetchMedia downloads one artifact straight into the media directory, so a
// the caller consumes chunks instead of retaining a second whole-file copy.
func (e *ProtoEngine) fetchMedia(ctx context.Context, lease *protoLease, m mediaRef) (*Media, error) {
	dir := e.cfg.MediaDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if tmp != "" {
			_ = os.Remove(tmp)
		}
	}()
	mime, size, err := e.download(ctx, lease, m, f)
	if err != nil {
		return nil, err
	}
	saved := describeMedia(f, size, mime, m)
	if err := f.Chmod(0o600); err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	f = nil
	if err := os.Rename(tmp, filepath.Join(dir, saved.Filename)); err != nil {
		return nil, err
	}
	tmp = ""
	return saved, nil
}

// download writes a VM workspace file fetched over the gateway (or a plain
// URL) into f, replacing what an earlier failed attempt left there.
func (e *ProtoEngine) download(ctx context.Context, lease *protoLease, m mediaRef, f *os.File) (string, int64, error) {
	reset := func() error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return f.Truncate(0)
	}
	var lastErr error
	if m.Path != "" {
		esc := (&url.URL{Path: m.Path}).EscapedPath()
		for _, prefix := range []string{"/media/raw/", "/fs/raw/"} {
			for dlAttempt := 0; dlAttempt < 3; dlAttempt++ {
				if err := reset(); err != nil {
					return "", 0, err
				}
				conn := lease.getConn()
				if conn == nil || !conn.Alive() {
					reconn, rerr := e.reconnectLease(ctx, lease)
					if rerr != nil {
						lastErr = rerr
						break
					}
					conn = reconn
				}
				ct, n, err := copyGatewayFile(ctx, conn, prefix+esc, f)
				if err == nil && n > 0 {
					return ct, n, nil
				}
				if err == nil {
					err = fmt.Errorf("%s -> empty body", prefix+m.Path)
				}
				lastErr = err
				if ctx.Err() != nil || strings.HasSuffix(err.Error(), "-> HTTP 404") {
					break
				}
				// A stalled or congested WebSocket connection can crawl at a
				// few KB/s; close it so the next attempt dials a fresh socket.
				_ = conn.Close()
			}
		}
	}
	if strings.HasPrefix(m.URL, "http") {
		if err := reset(); err != nil {
			return "", 0, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.URL, nil)
		if err != nil {
			return "", 0, err
		}
		req.Header.Set("User-Agent", DefaultUA)
		client := httpClient(e.proxyFor(lease.acc), 5*time.Minute)
		defer client.CloseIdleConnections()
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == 200 {
				n, err := io.Copy(f, io.LimitReader(resp.Body, maxMediaBytes+1))
				if err == nil && n > maxMediaBytes {
					err = errors.New("media file exceeds 512 MiB")
				}
				if err != nil {
					return "", 0, err
				}
				return resp.Header.Get("Content-Type"), n, nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no path or url")
	}
	return "", 0, lastErr
}

// copyGatewayFile streams one gateway response body into w.
func copyGatewayFile(ctx context.Context, conn *hatch.Conn, path string, w io.Writer) (string, int64, error) {
	openCtx, openCancel := context.WithTimeout(ctx, 30*time.Second)
	st, err := conn.OpenContext(openCtx, hatch.Request{Method: http.MethodGet, Path: path})
	openCancel()
	if err != nil {
		return "", 0, err
	}
	defer st.Close()
	var n int64
	for {
		chunkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		b, err := st.NextChunk(chunkCtx)
		cancel()
		if code := st.Status(); code >= 300 {
			return "", 0, fmt.Errorf("%s -> HTTP %d", path, code)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", 0, err
		}
		if n += int64(len(b)); n > maxMediaBytes {
			return "", 0, errors.New("media file exceeds 512 MiB")
		}
		if _, err := w.Write(b); err != nil {
			return "", 0, err
		}
	}
	ct := ""
	for _, h := range st.Headers() {
		if strings.EqualFold(h.Key, "content-type") {
			ct = h.Value
		}
	}
	return ct, n, nil
}

// describeMedia names and measures a downloaded file from its content.
func describeMedia(f io.ReaderAt, size int64, mime string, m mediaRef) *Media {
	head := make([]byte, min(size, 512))
	n, _ := f.ReadAt(head, 0)
	sniff := http.DetectContentType(head[:n])
	if sniff != "application/octet-stream" && !strings.HasPrefix(mime, "image/") && !strings.HasPrefix(mime, "video/") {
		mime = sniff
	}
	if mime == "" {
		mime = m.Mime
	}
	ext := pickExt(mime, m.Path+m.URL, m.Kind)
	kind := "image"
	if ext == ".mp4" || ext == ".webm" || ext == ".mov" {
		kind = "video"
	}
	if mime == "" || mime == "application/octet-stream" {
		mime = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif", ".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime"}[ext]
	}
	w, h, secs := probeMediaFile(f, size)
	return &Media{Filename: randHex(16) + ext, Kind: kind, Mime: mime, Bytes: size, Width: w, Height: h, Seconds: secs, Source: m.Path}
}

// classifyTurnErr classifies the text of a media turn that produced no media.
// The model often explains a refusal in words that mention limits ("the
// prompt exceeds the token limit", "达到上限"), so, as for chat, only a brief
// reply that starts like an upstream limit notice counts as one. The reply is
// kept on the error as a *ModelReply.
func classifyTurnErr(text string) error {
	// Media replies may summarize this structured tool code in a longer
	// failure report. Do not apply this exception to ordinary chat answers.
	if strings.Contains(text, "video_daily_budget_exhausted") || mediaQuotaNoticeRe.MatchString(text) {
		return &Error{Kind: KindQuota, Msg: "视频每日额度已用尽", Err: &ModelReply{Text: text}}
	}
	err := classifyChatNotice(text)
	if err == nil {
		return nil
	}
	e := err.(*Error)
	e.Err = &ModelReply{Text: text}
	return e
}

// ModelReply is the text the model answered with instead of media. Callers
// find it with errors.As to log what upstream actually said.
type ModelReply struct{ Text string }

func (r *ModelReply) Error() string { return r.Text }

// Chat replies are ordinary model text, so a keyword anywhere in a short
// answer ("what is an API rate limit?") must not turn it into an error. Only
// a brief reply that starts like an upstream limit notice is classified.
var (
	// Media agents often report a daily/account video quota rejection in a
	// longer answer. Require an affirmative account quota clause at a sentence
	// boundary, not conditional advice, content limits, or prompt duration.
	mediaQuotaNoticeRe = regexp.MustCompile(`(?i)(?:^|[。！？.!?\n，,:：;；])\s*(?:(?:(?:今天|今日|当天|每日|本日)(?:的)?|(?:当前|这个|该|本|你的)?(?:账号|账户)(?:的)?)(?:视频|影片)(?:生成)?(?:额度|配额|预算|次数)(?:已经全部|已全部|已经|已|都|全部)?(?:用完|用尽|耗尽|不足|达到上限|达上限)|(?:this|your|the|current) account (?:has )?(?:reached|hit|exhausted) (?:its |the |your )?(?:daily |24[- ]hour )?video (?:limit|quota|budget))`)
	chatRateNoticeRe   = regexp.MustCompile(`(?i)^[\s\p{P}\p{S}]*(you('ve| have| are)? (been )?(rate[- ]limited|sending (requests )?too (fast|quickly))|too many requests|rate limit(ed)? (exceeded|reached|hit)|请求过于频繁|触发频率限制)`)
	chatQuotaNoticeRe  = regexp.MustCompile(`(?i)^[\s\p{P}\p{S}]*(you('ve| have)? (run out of|used up|used all( of)? your|reached your|hit your) (credits|usage|messages|daily|weekly|monthly|limit)|out of credits|(daily |weekly |monthly )?usage limit reached|(daily |weekly |monthly )?limit reached|额度不足|积分不足|用量已达|今日额度)`)
)

const chatNoticeMaxRunes = 200

func classifyChatNotice(text string) error {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > chatNoticeMaxRunes {
		return nil
	}
	if chatRateNoticeRe.MatchString(text) {
		return errf(KindRateLimit, "上游限流")
	}
	if chatQuotaNoticeRe.MatchString(text) {
		return errf(KindQuota, "账号额度不足")
	}
	return nil
}

// ---------------------------------------------------------------- Backend

func (e *ProtoEngine) Chat(ctx context.Context, acc *Account, prompt string, refs []RefImage, onDelta func(string)) (*ChatResult, error) {
	if err := validateRequest(prompt, refs); err != nil {
		return nil, err
	}
	a, err := e.acquire(ctx, acc)
	if err != nil {
		return nil, err
	}
	defer e.release(acc.ID, a)
	ctx, cancel := context.WithTimeout(ctx, e.cfg.ChatTimeout)
	defer cancel()
	turn, err := e.runTurn(ctx, a, prompt, refs, "", "", onDelta, nil)
	e.persist(acc.ID, a.protoAcct, nil)
	if err != nil {
		return nil, err
	}
	if turn.text == "" {
		return nil, errf(KindTransient, "muse 返回了空回复")
	}
	if err := classifyChatNotice(turn.text); err != nil {
		return nil, err
	}
	res := &ChatResult{Text: turn.text}
	// Images the assistant produced inside a normal chat are delivered too.
	if refs := extractMedia(turn.events); len(refs) > 0 {
		res.Media, _ = e.fetchAll(ctx, a, refs)
	}
	return res, nil
}

func (e *ProtoEngine) Generate(ctx context.Context, acc *Account, r GenRequest) ([]Media, error) {
	if r.State != nil && r.State.AccountID != "" && (acc == nil || r.State.AccountID != acc.ID) {
		return nil, BadRequest("generation checkpoint belongs to another account")
	}
	if err := validateRequest(r.Prompt, r.Refs); err != nil {
		return nil, err
	}
	if r.Kind != "image" && r.Kind != "video" {
		return nil, BadRequest("kind must be image or video")
	}
	if r.Timeout <= 0 {
		r.Timeout = e.cfg.GenerateTimeout
	}
	if r.N < 1 {
		r.N = 1
	}
	progress := func(p int) {
		if r.OnProgress != nil {
			r.OnProgress(p)
		}
	}
	stage := func(name string) {
		if r.OnStage != nil {
			r.OnStage(name)
		}
	}
	stage("connecting")
	a, err := e.acquire(ctx, acc)
	if err != nil {
		if r.State != nil && (r.State.SessionID != "" || len(r.State.Media) > 0) {
			return nil, &NoRetry{Err: err}
		}
		return nil, err
	}
	defer e.release(acc.ID, a)
	progress(10)
	state := r.State
	if state == nil {
		state = &GenerationState{}
	}
	state.AccountID = acc.ID
	var turn *turnResult
	if len(state.Media) > 0 {
		turn = &turnResult{media: state.Media}
	} else {
		stage("generating")
		if state.Deadline.IsZero() {
			state.Deadline = time.Now().Add(r.Timeout)
		}
		turnCtx, cancel := context.WithDeadline(ctx, state.Deadline)
		turn, err = e.runTurnResumable(turnCtx, a, r.Prompt, r.Refs, r.Kind, "", nil, progress, state, r.SaveState)
		cancel()
		e.persist(acc.ID, a.protoAcct, nil)
		if err != nil {
			return nil, &StageError{Stage: "generating", Err: err}
		}
	}
	if len(turn.media) == 0 {
		// A failed media tool call names the real cause; the assistant's
		// text afterwards usually only paraphrases it.
		if f := findToolFailure(turn.events); f != nil {
			return nil, f.error(r.Kind)
		}
		if err := classifyTurnErr(turn.text); err != nil {
			return nil, err
		}
		if turn.text == "" {
			return nil, &NoRetry{Err: errf(KindTransient, "生成结束但未返回%s", kindName(r.Kind))}
		}
		return nil, wrap(KindRefusal, fmt.Sprintf("模型未生成%s，仅返回文本", kindName(r.Kind)), &ModelReply{Text: turn.text})
	}
	state.Media = turn.media
	if r.SaveState != nil {
		if err := r.SaveState(state); err != nil {
			return nil, &NoRetry{Err: wrap(KindTransient, "保存媒体引用失败", err)}
		}
	}
	progress(80)
	stage("downloading")
	list := turn.media
	if len(list) > r.N {
		list = list[len(list)-r.N:]
	}
	dlCtx, dlCancel := context.WithTimeout(ctx, 6*time.Minute)
	defer dlCancel()
	out, lastErr := e.fetchAll(dlCtx, a, list)
	if len(out) == 0 {
		return nil, &StageError{Stage: "downloading", Err: &NoRetry{Err: wrap(KindTransient, "已生成但未能取回文件", lastErr)}}
	}
	progress(100)
	return out, nil
}

// Diagnose checks every protocol layer for one account and reports timings.
func (e *ProtoEngine) Diagnose(ctx context.Context, acc *Account) (map[string]any, error) {
	out := map[string]any{"driver": e.Name()}
	t := time.Now()
	a, err := e.acquire(ctx, acc)
	out["connect_ms"] = time.Since(t).Milliseconds()
	if err != nil {
		out["error"] = err.Error()
		return out, nil
	}
	defer e.release(acc.ID, a)
	out["vm_assigned"] = a.sess.VMID != ""
	out["notary_token"] = a.sess.NotaryToken != ""
	for _, probe := range []struct{ name, path string }{{"model", "/model"}, {"sessions", "/api/session/list"}} {
		t = time.Now()
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		resp, err := a.conn.Do(cctx, hatch.Request{Method: http.MethodGet, Path: probe.path})
		cancel()
		if err != nil {
			out[probe.name] = map[string]any{"error": err.Error()}
			continue
		}
		out[probe.name] = map[string]any{"status": resp.Status, "ms": time.Since(t).Milliseconds()}
	}
	return out, nil
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
