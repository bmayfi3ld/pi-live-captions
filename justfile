set dotenv-load := true

run *args:
    go run ./cmd/livecaption {{args}}

build:
    mkdir -p ./bin
    go build -o ./bin ./cmd/livecaption
    @echo ""
    @echo "To serve on port 80 (needed for http://livecaptions.local with no :port) without running as root:"
    @echo "  sudo setcap 'cap_net_bind_service=+ep' ./bin/livecaption"

lint:
    golangci-lint run ./...
