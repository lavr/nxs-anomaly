package engine

import (
	"bufio"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// smtpStub accepts connections and hands each to serve. It returns the
// configuration that points sendEmail at it.
func smtpStub(t *testing.T, timeout time.Duration, useTLS bool, serve func(net.Conn)) SMTPConfig {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				serve(conn)
				<-done // hold the connection open, silent, until the test ends
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return SMTPConfig{
		Host: "127.0.0.1", Port: p, Sender: "oncall@example.com", From: "On-call",
		UseTLS: useTLS, Timeout: timeout,
	}
}

// smtpDialog answers the commands of one session from the script, in order,
// and goes silent when the script runs out.
func smtpDialog(script ...string) func(net.Conn) {
	return func(conn net.Conn) {
		r := bufio.NewReader(conn)
		_, _ = conn.Write([]byte("220 stub ESMTP\r\n"))
		inData := false
		for _, reply := range script {
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if inData && strings.TrimRight(line, "\r\n") != "." {
					continue
				}
				inData = false
				break
			}
			_, _ = conn.Write([]byte(reply + "\r\n"))
			if strings.HasPrefix(reply, "354") {
				inData = true
			}
		}
	}
}

func sendBounded(t *testing.T, ctx context.Context, cfg SMTPConfig, within time.Duration) (string, string) {
	t.Helper()
	start := time.Now()
	status, errMsg, _ := sendEmail(ctx, "sre@example.com", "subj", "body", cfg)
	if took := time.Since(start); took > within {
		t.Fatalf("send took %v, want under %v: the stalled server held it", took, within)
	}
	return status, errMsg
}

// A server that accepts the connection and never greets used to hold the
// delivery goroutine forever, on the plain path and on the TLS one alike.
func TestSMTPServerThatNeverGreetsTimesOut(t *testing.T) {
	for _, useTLS := range []bool{false, true} {
		cfg := smtpStub(t, 300*time.Millisecond, useTLS, func(net.Conn) {})
		if status, _ := sendBounded(t, context.Background(), cfg, 5*time.Second); status != "failed" {
			t.Errorf("tls=%v: status = %q, want failed", useTLS, status)
		}
	}
}

// The deadline covers the whole exchange, not only the greeting.
func TestSMTPServerThatStallsMidExchangeTimesOut(t *testing.T) {
	cfg := smtpStub(t, 300*time.Millisecond, false, smtpDialog("250 stub")) // answers EHLO, then nothing
	if status, _ := sendBounded(t, context.Background(), cfg, 5*time.Second); status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
}

// A cancelled worker context ends the send without waiting out the timeout.
func TestSMTPSendObservesContext(t *testing.T) {
	cfg := smtpStub(t, time.Minute, false, func(net.Conn) {})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if status, _ := sendBounded(t, ctx, cfg, 5*time.Second); status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
}

// The bounded path still delivers: greeting, EHLO, MAIL, RCPT, DATA, QUIT-less close.
func TestSMTPPlainExchangeDelivers(t *testing.T) {
	cfg := smtpStub(t, 5*time.Second, false,
		smtpDialog("250 stub", "250 ok", "250 ok", "354 go ahead", "250 queued"))
	if status, errMsg := sendBounded(t, context.Background(), cfg, 5*time.Second); status != "delivered" {
		t.Errorf("status = %q (%s), want delivered", status, errMsg)
	}
}

// Through the dispatcher a stalled server is a retryable failure, so the
// delivery stage finishes and the notification is tried again.
func TestStalledSMTPIsARetryableDeliveryFailure(t *testing.T) {
	cfg := DeliveryConfig{SMTP: smtpStub(t, 300*time.Millisecond, false, func(net.Conn) {})}
	e := honestyEngine(newMemStore(), cfg)
	start := time.Now()
	out := e.deliverNotificationViaAdapter(context.Background(), notificationFor("email", "sre@example.com", map[string]any{"title": "x"}))
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("delivery took %v", took)
	}
	if out.Status != deliveryFailed {
		t.Errorf("status = %q (%s), want %q", out.Status, out.Err, deliveryFailed)
	}
}
