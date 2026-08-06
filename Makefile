build:
	go build -o bin/rune-claude ./cmd/rune-claude
	go build -o bin/claude-changes ./extension

install: build
	install -m 755 bin/rune-claude $(HOME)/.local/bin/rune-claude

test:
	go test ./...

.PHONY: build install test
