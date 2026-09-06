package web

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"livecaption/internal/audio"
	"livecaption/internal/metrics"
)

func TestNoiseGateAPI(t *testing.T) {
	const valid = `{"threshold_dbfs":-35,"release_sec":10}`
	for _, tc := range []struct {
		name, body, contentType, origin, site, password string
		enabled                                         bool
		status                                          int
	}{
		{"valid", valid, "application/json", "", "", "secret", true, 200},
		{"zero", `{"threshold_dbfs":0,"release_sec":0}`, "application/json", "", "", "secret", true, 200},
		{"disabled", valid, "application/json", "", "", "secret", false, 503},
		{"unauthorized", valid, "application/json", "", "", "wrong", true, 401},
		{"origin", valid, "application/json", "https://evil.example", "", "secret", true, 403},
		{"fetch-site", valid, "application/json", "", "cross-site", "secret", true, 403},
		{"form", valid, "application/x-www-form-urlencoded", "", "", "secret", true, 415},
		{"missing", `{"threshold_dbfs":-30}`, "application/json", "", "", "secret", true, 400},
		{"null", `{"threshold_dbfs":null,"release_sec":5}`, "application/json", "", "", "secret", true, 400},
		{"malformed", `{`, "application/json", "", "", "secret", true, 400},
		{"threshold", `{"threshold_dbfs":1,"release_sec":5}`, "application/json", "", "", "secret", true, 400},
		{"release", `{"threshold_dbfs":-30,"release_sec":61}`, "application/json", "", "", "secret", true, 400},
		{"negative", `{"threshold_dbfs":-30,"release_sec":-1}`, "application/json", "", "", "secret", true, 400},
		{"nonfinite", `{"threshold_dbfs":1e999,"release_sec":5}`, "application/json", "", "", "secret", true, 400},
		{"trailing", valid + ` {}`, "application/json", "", "", "secret", true, 400},
		{"unknown", `{"threshold_dbfs":-35,"release_sec":10,"oops":1}`, "application/json", "", "", "secret", true, 400},
		{"oversize", valid + strings.Repeat(" ", 1024), "application/json", "", "", "secret", true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig()
			initial := audio.NoiseSettings{ThresholdDBFS: -45, ReleaseSec: 5}
			gate, err := audio.NewNoiseGate(initial)
			if err != nil {
				t.Fatal(err)
			}
			cfg.NoiseGate, cfg.Metrics.NoiseGate = gate, gate
			if tc.enabled {
				cfg.AdminPassword = "secret"
			}
			base, _, _ := startTestServer(t, cfg)
			req, err := http.NewRequest(http.MethodPost, base+"/api/noise-gate", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.SetBasicAuth("admin", tc.password)
			req.Header.Set("Content-Type", tc.contentType)
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Sec-Fetch-Site", tc.site)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status %d want %d: %s", resp.StatusCode, tc.status, body)
			}
			if tc.status != 200 {
				if gate.Snapshot().Settings != initial {
					t.Fatal("rejected request changed settings")
				}
				return
			}
			var got audio.NoiseSnapshot
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatal(err)
			}
			var want audio.NoiseSettings
			if err := json.Unmarshal([]byte(tc.body), &want); err != nil {
				t.Fatal(err)
			}
			if got.Settings != want || got.Startup != initial || !got.Stale {
				t.Fatal(got)
			}
			stats, err := http.Get(base + "/api/stats")
			if err != nil {
				t.Fatal(err)
			}
			defer stats.Body.Close()
			var snap metrics.Snapshot
			if err := json.NewDecoder(stats.Body).Decode(&snap); err != nil {
				t.Fatal(err)
			}
			if snap.NoiseGate == nil || snap.NoiseGate.Settings != want {
				t.Fatal("stats not authoritative", snap.NoiseGate)
			}
		})
	}
}
