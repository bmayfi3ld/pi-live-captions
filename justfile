set dotenv-load := true

run *args:
    go run ./cmd/livecaption {{args}}

build:
    mkdir -p ./bin
    go build -o ./bin ./cmd/livecaption
