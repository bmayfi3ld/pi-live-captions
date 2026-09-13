## 1. Correct eviction accounting

- [x] 1.1 Add deterministic regression coverage in `internal/stt/ring_test.go` using synthetic gate transitions and a full paused buffer; assert zero buffer drops and clean resumed health when only paused-period chunks are evicted, then counted drops and degradation once active-period chunks are evicted. Verify the new resume assertion fails against the existing implementation with `go test ./internal/stt -run TestRing`.
- [x] 1.2 Record admission-time STT gate activity on each private ring chunk in `internal/stt/ring.go` and count evictions using the discarded chunk's classification; replace the eviction comment to explain the transition-safe rule. Preserve existing locking, byte accounting, capacity, notification, and PCM/timestamps. Verify `go test ./internal/stt -run TestRing` passes.
- [x] 1.3 Extend the same ring regression coverage for paused rotation, active-period chunks evicted after pause, disabled auto-pause, retained payload order/timestamps, and preservation of unrelated degradation and accumulated counters. Verify exact counter/health assertions with `go test ./internal/stt -run TestRing`.

## 2. Document and verify

- [x] 2.1 Add an application `CHANGELOG.md` entry describing false degradation from paused-buffer eviction on resume; verify the entry does not claim to fix the separate browser-refresh/server-restart symptom and the diff contains no browser, deployment, or kiosk changes.
- [x] 2.2 Format touched Go files and run `go build ./...`, `go test ./...`, `go test -race ./internal/stt/...`, and `golangci-lint run ./...`; fix findings in touched code and record results. Do not run the application binary or start an app server; browser behavior remains unverified and outside this change.
