package engine

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/nixys/nxs-anomaly/internal/model"
	"github.com/nixys/nxs-anomaly/internal/utils"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// stubPreflightLookup replaces the pre-flight resolver for one test. The
// returned setter switches what it answers.
func stubPreflightLookup(t *testing.T) func(ips []net.IPAddr, err error) {
	t.Helper()
	orig := preflightLookup
	t.Cleanup(func() { preflightLookup = orig })
	var answer []net.IPAddr
	var failure error
	preflightLookup = func(_ context.Context, _ string) ([]net.IPAddr, error) {
		return answer, failure
	}
	return func(ips []net.IPAddr, err error) { answer, failure = ips, err }
}

func okClient() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: http.Header{}}, nil
	})}
}

// A resolver that fails for a moment is not a verdict about the destination.
// The notification has to go to the retry queue and reach the receiver once DNS
// answers again; it used to be skipped as a blocked destination, terminally.
func TestTransientDNSFailureIsRetriedThenDelivered(t *testing.T) {
	answer := stubPreflightLookup(t)
	answer(nil, &net.DNSError{Err: "server misbehaving", Name: "hooks.example.com", IsTemporary: true})
	const url = "https://hooks.example.com/x"

	out := postWebhookGuarded(context.Background(), okClient(), url, map[string]any{"title": "x"}, guardDirect, nil)
	if out.Status != deliveryFailed {
		t.Fatalf("status = %q (%s), want %q (retryable)", out.Status, out.ProviderStatus, deliveryFailed)
	}

	e := honestyEngine(newMemStore(), DeliveryConfig{MaxRetries: 3, RetryDelays: []int{1}})
	n := model.WrapNotification(notificationFor("webhook", url, map[string]any{"title": "x"}))
	e.applyOutcome(n, out, utils.ToISO(utils.UTCNow()))
	if !n.IsRetryScheduled() {
		t.Fatalf("notification is %q after a failed lookup, want %q", n.Status(), model.NotificationRetryScheduled)
	}

	answer([]net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil)
	if out := postWebhookGuarded(context.Background(), okClient(), url, map[string]any{"title": "x"}, guardDirect, nil); out.Status != deliveryDelivered {
		t.Errorf("after DNS recovered: status = %q (%s), want %q", out.Status, out.Err, deliveryDelivered)
	}
}

// Telling the two apart must not loosen the guard: a name that resolves to a
// non-public address is still refused, terminally.
func TestNameResolvingToPrivateAddressStaysBlocked(t *testing.T) {
	answer := stubPreflightLookup(t)
	answer([]net.IPAddr{{IP: net.ParseIP("10.0.0.5")}}, nil)

	out := postWebhookGuarded(context.Background(), okClient(), "https://hooks.example.com/x", map[string]any{"title": "x"}, guardDirect, nil)
	if out.Status != deliverySkipped || out.ProviderStatus != skipBlockedDestination {
		t.Errorf("outcome = %q/%q, want %q/%q", out.Status, out.ProviderStatus, deliverySkipped, skipBlockedDestination)
	}
}

// The issue channel runs the same pre-flight and had the same terminal skip.
func TestIssueDestinationDNSFailureIsRetryable(t *testing.T) {
	answer := stubPreflightLookup(t)
	answer(nil, &net.DNSError{Err: "i/o timeout", Name: "tracker.example.com", IsTimeout: true})
	e := honestyEngine(newMemStore(), DeliveryConfig{BlockPrivateWebhooks: true})

	status, errMsg, _ := e.deliverIssue(context.Background(), map[string]any{"url": "https://tracker.example.com/issues.json"})
	if status != "failed" {
		t.Errorf("status = %q (%s), want failed (retryable)", status, errMsg)
	}
}
