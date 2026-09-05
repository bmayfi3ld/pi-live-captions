set dotenv-load := true

run *args:
    go run ./cmd/livecaption {{args}}

build:
    mkdir -p ./bin
    go build -ldflags "-X livecaption/internal/cli.Version=$(./deploy/version.sh)~dev" -o ./bin ./cmd/livecaption
    @echo ""
    @echo "To serve on port 80 (needed for http://livecaptions.local with no :port) without running as root:"
    @echo "  sudo setcap 'cap_net_bind_service=+ep' ./bin/livecaption"

lint:
    golangci-lint run ./...

test *args:
    go test ./... {{args}}

# Build a .deb for one architecture. Version comes from CHANGELOG.md.
deb arch="amd64":
    ./deploy/build-deb.sh {{arch}}
