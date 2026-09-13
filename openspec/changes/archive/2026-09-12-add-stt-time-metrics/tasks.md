## 1. STT Duration Metrics

- [x] 1.1 Add `minutes_sent_total` and `hours_sent_total` to the STT snapshot, deriving both from `BytesSent` with `audio.PipelineFormat.Duration`, and verify zero and known-byte conversions in `internal/metrics/metrics_test.go`.
- [x] 1.2 Verify the new fields serialize through `/api/stats` with their exact JSON names while `bytes_sent_total` remains unchanged by running the focused metrics and web tests.

## 2. Admin Dashboard

- [x] 2.1 Add minute and hour rows to the STT card in `internal/web/static/admin.html`, update them from the existing stats refresh with compact unit-labelled formatting, and verify the static markup and field references are present without starting the application.

## 3. Release and Verification

- [x] 3.1 Add an Unreleased changelog entry for duration-based STT usage metrics and verify it describes both API and dashboard behavior.
- [x] 3.2 Run `gofmt` on changed Go files, `golangci-lint run ./...`, `go test ./...`, and `go build ./...`; leave browser rendering verification to a running user instance as required by project guidance.
