.PHONY: test vet spike build bin install bootstrap

# Version stamped into the binary: the exact tag if HEAD is one, else
# "<tag>-<n>-g<sha>" (git describe), else "dev". Used by `bubbles version` and
# by auto-update to know whether a newer release exists.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

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
	go build $(LDFLAGS) -o bin/bubbles ./cmd/bubbles
	@echo "built bin/bubbles ($(VERSION))"

# Install the bubbles command to ~/.local/bin (must be on your PATH).
install:
	go build $(LDFLAGS) -o $(HOME)/.local/bin/bubbles ./cmd/bubbles
	@echo "installed -> $(HOME)/.local/bin/bubbles ($(VERSION))  (run 'bubbles' from any project dir)"
