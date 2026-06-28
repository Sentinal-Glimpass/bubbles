.PHONY: test vet spike build

test:
	go test ./...

vet:
	go vet ./...

# Run the PTY delivery spike (use -cmd claude on macOS to validate interrupt).
spike:
	go run ./cmd/spike $(ARGS)

build:
	go build ./...
