.PHONY: test vet spike build bin install bootstrap

# One command that installs every dependency (Go, Claude Code, ngrok) and builds
# bubbles. Pass ARGS="--no-ngrok" to skip the optional ngrok install.
bootstrap:
	@bash install.sh $(ARGS)

test:
	go test ./...

vet:
	go vet ./...

# Run the PTY delivery spike (use -cmd claude on macOS to validate interrupt).
spike:
	go run ./cmd/spike $(ARGS)

build:
	go build ./...

# Build the single bubbles binary into bin/.
bin:
	go build -o bin/bubbles ./cmd/bubbles
	@echo "built bin/bubbles"

# Install the bubbles command to ~/.local/bin (must be on your PATH).
install:
	go build -o $(HOME)/.local/bin/bubbles ./cmd/bubbles
	@echo "installed -> $(HOME)/.local/bin/bubbles  (run 'bubbles' from any project dir)"
