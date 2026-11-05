package muse

import "testing"

func TestChatNoticeIgnoresOrdinaryAnswers(t *testing.T) {
	for _, text := range []string{
		"A rate limit caps how many requests a client may send per minute.",
		"API 的 rate limit 是指单位时间内允许的请求数。",
		"The model has a token limit of 128k tokens.",
		"When the usage limit is reached, the API returns HTTP 429.",
		"如果达到上限，服务会返回错误。",
		"Sending too many requests in a short time can trigger throttling.",
		"你好，请求过于频繁通常是因为没有做退避。",
		"关于额度不足的问题，可以联系管理员。",
	} {
		if err := classifyChatNotice(text); err != nil {
			t.Fatalf("%q classified as %v", text, err)
		}
	}
}

func TestChatNoticeClassifiesLimitMessages(t *testing.T) {
	cases := []struct {
		text string
		want Kind
	}{
		{"You've been rate limited. Please try again later.", KindRateLimit},
		{"Too many requests. Please slow down.", KindRateLimit},
		{"请求过于频繁，请稍后再试", KindRateLimit},
		{"You've reached your daily limit. Come back tomorrow.", KindQuota},
		{"Out of credits.", KindQuota},
		{"Weekly usage limit reached", KindQuota},
		{"额度不足，请明天再来", KindQuota},
	}
	for _, c := range cases {
		err := classifyChatNotice(c.text)
		if err == nil || KindOf(err) != c.want {
			t.Fatalf("%q: got %v, want kind %v", c.text, err, c.want)
		}
	}
}

func TestGatewayProxyResolvesAgainstGateway(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.test:3128")
	t.Setenv("NO_PROXY", "gateway.test")
	e := NewProtoEngine(&Config{SiteURL: "https://site.test", GatewayURL: "wss://gateway.test/v1/noise"}, nil)
	acc := &Account{ID: "a"}
	if got := e.wsProxy(acc); got != "direct" {
		t.Fatalf("gateway route = %q, want direct", got)
	}
	if got := e.proxyFor(acc); got != "http://proxy.test:3128" {
		t.Fatalf("site route = %q", got)
	}
	acc.Proxy = "socks5://127.0.0.1:1080"
	if got := e.wsProxy(acc); got != "socks5://127.0.0.1:1080" {
		t.Fatalf("explicit gateway route = %q", got)
	}
}
