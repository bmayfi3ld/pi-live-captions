package mdns

import (
	"log/slog"
	"os/exec"
	"testing"
)

func TestStartEmptyNameDisabled(t *testing.T) {
	if p := Start("", slog.Default()); p != nil {
		t.Fatalf("Start(\"\", ...) = %v, want nil", p)
	}
}

func TestOutboundIP(t *testing.T) {
	ip, err := outboundIP()
	if err != nil {
		t.Fatalf("outboundIP() error = %v", err)
	}
	if ip == nil {
		t.Fatal("outboundIP() returned nil IP")
	}
	if ip.IsLoopback() {
		t.Fatalf("outboundIP() = %v, want a non-loopback address", ip)
	}
}

func TestStopNilReceiver(t *testing.T) {
	var p *Publisher
	p.Stop() // must not panic
}

func TestStartAndStop(t *testing.T) {
	if _, err := exec.LookPath("avahi-publish"); err != nil {
		t.Skip("avahi-publish not available")
	}

	p := Start("mdnstest-livecaption", slog.Default())
	if p == nil {
		t.Fatal("Start() = nil, want a *Publisher")
	}
	p.Stop()
}
