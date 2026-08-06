build:
	go build -o bin/rune-claude ./cmd/rune-claude
	go build -o bin/claude-changes ./extension

test:
	go test ./...

.PHONY: build test
