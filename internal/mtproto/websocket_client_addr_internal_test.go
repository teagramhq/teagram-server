package mtproto

import (
	"bytes"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestIgnoredCFConnectingIPWarningIsSampledOncePerMinute(t *testing.T) {
	var logs bytes.Buffer
	server := &Server{log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))}
	peer := netip.MustParseAddr("192.0.2.77")
	forwarded := netip.MustParseAddr("203.0.113.7")
	logIgnored := func() {
		t.Helper()
		server.logCFConnectingIPTrust(peer, cfConnectingIPHeader, forwarded, false)
	}

	logIgnored()
	if warnings := strings.Count(logs.String(), "level=WARN"); warnings != 1 {
		t.Fatalf("emitted %d warnings on the first ignored address, want one:\n%s", warnings, logs.String())
	}
	for _, want := range []string{"public WebSocket", "peer_addr=192.0.2.77"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("warning does not name %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), forwarded.String()) {
		t.Errorf("warning exposed the untrusted header value:\n%s", logs.String())
	}

	server.clientAddrIgnoreLog.last.Store(time.Now().Add(-30 * time.Second).UnixNano())
	logIgnored()
	if warnings := strings.Count(logs.String(), "level=WARN"); warnings != 1 {
		t.Fatalf("emitted %d warnings within one minute, want one:\n%s", warnings, logs.String())
	}

	server.clientAddrIgnoreLog.last.Store(time.Now().Add(-time.Minute).UnixNano())
	logIgnored()
	if warnings := strings.Count(logs.String(), "level=WARN"); warnings != 2 {
		t.Fatalf("emitted %d warnings after the sampling minute, want two:\n%s", warnings, logs.String())
	}
}
