.PHONY: build build-arm64 run run-prompt bench test test-race fmt vet tidy

build:
	go build -o bin/app ./cmd
	go build -o bin/bench ./cmd/bench

# For the Jetson Orin (linux/arm64); stdlib only, so no cgo and no toolchain needed.
build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/app-linux-arm64 ./cmd
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o bin/bench-linux-arm64 ./cmd/bench

# Shadow mode: ask the local model on every event and log what it would do.
run:
	go run ./cmd -config config/dog.json

# No model: print the prompt of every tick.
run-prompt:
	go run ./cmd -config config/dog.json -no-llm

# Score the model behind config/dog.json on the hand-written cases.
# Add `-log shadow.jsonl` to replay recorded decisions too.
bench:
	go run ./cmd/bench -config config/dog.json -cases config/bench_cases.json

test:
	go test ./...

test-race:
	go test -race ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy
