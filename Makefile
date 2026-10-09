.PHONY: generate build test clean htmx-ui

# Generate templ files
generate:
	@echo "Generating templ files..."
	@which templ > /dev/null || (echo "Error: templ not installed. Run: go install github.com/a-h/templ/cmd/templ@latest" && exit 1)
	templ generate

# Build the project
build: generate
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

# Development: watch for changes and regenerate
watch:
	@which templ > /dev/null || (echo "Error: templ not installed. Run: go install github.com/a-h/templ/cmd/templ@latest" && exit 1)
	templ generate --watch

# Install templ
install-templ:
	@echo "Installing templ..."
	go install github.com/a-h/templ/cmd/templ@latest
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
run: generate
	go run ./cmd/agent-relay

# Build for production
build-prod: generate
	CGO_ENABLED=0 go build -ldflags="-w -s" -o bin/agent-relay ./cmd/agent-relay

help:
	@echo "Available targets:"
	@echo "  setup          - Install templ and generate files (first time setup)"
	@echo "  generate       - Generate templ files"
	@echo "  build          - Build the project"
	@echo "  build-prod     - Build optimized binary for production"
	@echo "  test           - Run tests"
	@echo "  test-coverage  - Run tests with coverage"
	@echo "  clean          - Clean generated files"
	@echo "  watch          - Watch for changes and regenerate"
	@echo "  fmt            - Format code"
	@echo "  run            - Run the server"
	@echo "  install-templ  - Install templ CLI"
