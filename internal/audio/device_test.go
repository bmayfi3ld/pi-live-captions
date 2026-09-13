package audio

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
}

func TestDeviceDescribe(t *testing.T) {
	s := NewDeviceSource(DeviceConfig{Device: "hw:2,0", Backend: "alsa"})
	if got, want := s.Describe(), "alsa:hw:2,0 (-> 16000 Hz mono)"; got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}

func TestDeviceFFmpegArgsRegenerateOutputTimestamps(t *testing.T) {
	s := NewDeviceSource(DeviceConfig{Device: "hw:2,0", Backend: "alsa", Stream: NewBroadcaster(nil)})
	args, aux := s.ffmpegArgs()
	if !aux {
		t.Fatal("streaming capture did not enable the auxiliary output")
	}
	if got := strings.Count(strings.Join(args, " "), "-af asetpts=N/SR/TB"); got != 2 {
		t.Errorf("timestamp filter count = %d, want 2; args: %v", got, args)
	}
}

// TestSetCallbacksWiresThrough guards the metric hooks that make the live
// hardening story (ffmpeg restarts, xruns, stderr) actually observable —
// a hook that silently fails to register would leave those counters dark.
func TestSetCallbacksWiresThrough(t *testing.T) {
	s := NewDeviceSource(DeviceConfig{Device: "x"})
	s.SetCallbacks(DeviceCallbacks{
		OnFrame:        func(int, time.Duration) {},
		OnXrun:         func() {},
		OnRestart:      func() {},
		OnStderr:       func(string) {},
		OnAvailability: func(string, string) {},
	})
	if s.cfg.OnFrame == nil || s.cfg.OnXrun == nil || s.cfg.OnRestart == nil || s.cfg.OnStderr == nil || s.cfg.OnAvailability == nil {
		t.Error("SetCallbacks did not wire every hook through to cfg")
	}
}

func fakeCapture(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestBlockedDeviceDoesNotLaunchCapture(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	fakeCapture(t, fmt.Sprintf("echo launched > %q\nexit 1\n", runs))

	ctx, cancel := context.WithCancel(context.Background())
	out, err := NewDeviceSource(DeviceConfig{
		Device: "missing", Backend: "pulse", BlockedReason: "not found",
	}).Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
		t.Fatal("blocked source produced a frame")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := os.Stat(runs); !os.IsNotExist(err) {
		t.Fatalf("blocked source launched ffmpeg: stat = %v", err)
	}
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Fatal("blocked source channel yielded a value")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked source channel did not close on cancellation")
	}
}

func TestCaptureFailureRetriesAndRecovers(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	fakeCapture(t, fmt.Sprintf(`if [ ! -f %q ]; then
  touch %q
  exit 1
fi
exec dd if=/dev/zero bs=3200 2>/dev/null
`, runs, runs))

	states := make(chan string, 4)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := NewDeviceSource(DeviceConfig{Device: "same", Backend: "alsa", Log: slog.New(slog.NewTextHandler(&logs, nil))})
	s.SetCallbacks(DeviceCallbacks{
		OnAvailability: func(state, reason string) { states <- state + ":" + reason },
	})
	started := time.Now()
	out, err := s.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case state := <-states:
		if !strings.HasPrefix(state, "unavailable:") {
			t.Fatalf("first availability = %q, want unavailable", state)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture failure was not reported")
	}
	select {
	case state := <-states:
		if !strings.HasPrefix(state, "capturing:") {
			t.Fatalf("recovery availability = %q, want capturing", state)
		}
		if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
			t.Errorf("capture retried after %v, want bounded 250ms cadence", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture retry did not recover")
	}
	logText := logs.String()
	for _, want := range []string{"backend=alsa", "device=same", "diagnostic=", "action=retry", "capture ended unexpectedly"} {
		if !strings.Contains(logText, want) {
			t.Errorf("capture log %q missing %q", logText, want)
		}
	}
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("recovered capture produced no frame")
	}
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			for range out {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture channel did not close on cancellation")
	}
}
