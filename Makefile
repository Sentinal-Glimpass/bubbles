.PHONY: test vet spike build bin

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
