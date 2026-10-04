.PHONY: build run test fmt vet tidy

build:
	go build -o bin/app ./cmd

run:
	go run ./cmd

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

tidy:
	go mod tidy
