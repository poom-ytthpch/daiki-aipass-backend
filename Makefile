APP=daiki-ai-passport-backend

.PHONY: fmt test build run
fmt:
	gofmt -w ./cmd ./internal 2>/dev/null || true

test:
	go test ./...

build:
	CGO_ENABLED=0 go build -trimpath -o bin/$(APP) ./cmd/api

run:
	go run ./cmd/api
