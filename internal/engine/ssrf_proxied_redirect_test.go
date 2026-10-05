package engine

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// GitHub #52: through a proxy this process never dials a redirect hop, so the
// redirect policy is the only place a hop can be judged — and it judged IP
// literals only. A redirect to a name (localhost, or anything that resolves to
// a private address) went to the proxy, body and all.
func TestRedirectToANameIsJudgedLikeTheFirstHop(t *testing.T) {
	check := deliveryCheckRedirect(ipPolicy(isBlockedIP), ChannelPolicy{})
	for _, target := range []string{
		"http://localhost/private",       // resolves to loopback here
		"http://LOCALHOST./private",      // case and trailing dot
		"http://admin.localhost/private", // RFC 6761: loopback whoever resolves it
		"http://localhost:8080/x",        // port does not matter
	} {
		req, _ := http.NewRequest(http.MethodPost, target, nil)
		if err := check(req, []*http.Request{{}}); err == nil {
			t.Errorf("redirect to %s was followed", target)
		}
	}
	// No local answer is not a verdict: the proxy resolves it, exactly as for
	// the first hop (guardProxiedHost).
	req, _ := http.NewRequest(http.MethodPost, "http://only-the-proxy-knows.invalid/next", nil)
	if err := check(req, []*http.Request{{}}); err != nil {
		t.Errorf("redirect to a name with no local answer refused: %v", err)
	}
}

// The issue's own reproduction: a proxied webhook to a public address answers
// 307 to http://localhost/private. The proxy must never be asked for the
// second URL, and the page must not be reported delivered.
func TestProxiedWebhookDoesNotFollowARedirectToLocalhost(t *testing.T) {
	var seen recorder
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.add(r.Host + r.URL.Path)
		if r.URL.Path == "/start" {
			w.Header().Set("Location", "http://localhost/private")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(proxySrv.Close)

	s := buildProxySettings("", map[string]string{"webhook": proxySrv.URL}, "")
	client := newDeliveryClients(5*time.Second, ipPolicy(isBlockedIP), ChannelPolicy{}, s)["webhook"]
	out := postWebhookGuarded(t.Context(), client, "http://93.184.216.34/start",
		map[string]any{"title": "synthetic"}, guardProxied, nil)

	if got := seen.list(); len(got) != 1 {
		t.Fatalf("proxy was asked for %v, want only the first hop", got)
	}
	if out.Status == deliveryDelivered {
		t.Fatalf("a page redirected to localhost was reported delivered: %+v", out)
	}
}
