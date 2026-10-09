.PHONY: build build-prod test test-ui-js test-coverage clean setup fmt lint run help

# Build the project
build:
	@echo "Building agent-relay..."
	go build -o bin/agent-relay ./cmd/agent-relay

# Run tests
test: test-ui-js
	go test ./...

# Console behaviour that only exists in the browser JS (the update panel that
# follows an update across the restart it performs).
test-ui-js:
	@command -v node >/dev/null || (echo "Error: node not installed (needed for test-ui-js)" && exit 1)
	@for f in internal/web/ui/_*.test.mjs; do echo "node $$f"; node "$$f" || exit 1; done

# Run tests with coverage
test-coverage:
	go test -cover ./...

# Clean build artifacts
clean:
	@echo "Cleaning build artifacts..."
	rm -rf bin/

# Quick dev setup
setup:
	@echo "Setup complete!"

# Format code
fmt:
	go fmt ./...

# Lint
lint:
	golangci-lint run

# Run the server (dev)
run:
	go run ./cmd/agent-relay

# Build for production
build-prod:
	CGO_ENABLED=0 go build -ldflags="-w -s" -o bin/agent-relay ./cmd/agent-relay

help:
	@echo "Available targets:"
	@echo "  setup          - Quick dev setup"
	@echo "  build          - Build the project"
	@echo "  build-prod     - Build optimized binary for production"
	@echo "  test           - Run tests"
	@echo "  test-ui-js     - Run console UI update panel logic tests"
	@echo "  test-coverage  - Run tests with coverage"
	@echo "  clean          - Clean build artifacts"
	@echo "  fmt            - Format code"
	@echo "  run            - Run the server"
