.PHONY: generate generate-check build build-prod test test-coverage clean watch install-templ setup fmt lint run help

# Regenerate internal/web/views/*_templ.go AFTER editing a .templ file.
# The generated code is committed on purpose: a clean checkout must build with
# plain `go build` (no CLI, no network). Commit .templ + generated code together.
generate:
	@which templ > /dev/null || (echo "Error: templ not installed. Run: make install-templ" && exit 1)
	templ generate

# CI parity: the committed generated code must match the .templ sources.
generate-check: generate
	@git diff --exit-code -- internal/web/views > /dev/null || (echo "Error: internal/web/views is stale — commit the regenerated code" && exit 1)
	@echo "generated templ code is up to date"

# Build the project (needs no templ CLI: generated code is committed)
build:
	@echo "Building agent-relay..."
	go build -o bin/agent-relay ./cmd/agent-relay

# Run tests
test:
	go test ./...

# Run tests with coverage
test-coverage:
	go test -cover ./...

# Clean generated files
clean:
	@echo "Cleaning generated files..."
	find internal/web/views -name "*_templ.go" -delete
	rm -rf bin/

# Development: watch for changes and regenerate (dev only, needs templ)
watch:
	@which templ > /dev/null || (echo "Error: templ not installed. Run: go install github.com/a-h/templ/cmd/templ@v0.3.1001" && exit 1)
	templ generate --watch

# Install templ
install-templ:
	@echo "Installing templ..."
	go install github.com/a-h/templ/cmd/templ@v0.3.1001
	@echo "Templ installed successfully"

# Quick dev setup
setup: install-templ generate
	@echo "Setup complete!"

# Format code
fmt:
	go fmt ./...
	templ fmt internal/web/views

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
	@echo "  setup          - Install templ and generate files (first time setup)"
	@echo "  generate       - Regenerate templ files (after editing .templ)"
	@echo "  generate-check - Verify committed generated code matches .templ"
	@echo "  build          - Build the project"
	@echo "  build-prod     - Build optimized binary for production"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage"
	@echo "  clean          - Clean generated files"
	@echo "  watch          - Watch for changes and regenerate"
	@echo "  fmt            - Format code"
	@echo "  run            - Run the server"
	@echo "  install-templ  - Install templ CLI"
