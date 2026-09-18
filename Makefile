BINARY := phonellm

.PHONY: build test vet lint run echo clean install

build:
	go build -o bin/$(BINARY) ./cmd/phonellm

test:
	go test -race ./...

vet:
	go vet ./...

check: vet test

# Milestone 0: prove SIP registration and RTP in both directions with no LLM.
echo:
	go run ./cmd/phonellm -echo

run:
	go run ./cmd/phonellm

# Cross-compile for a Raspberry Pi / ARM64 Linux box from macOS.
build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -o bin/$(BINARY)-linux-arm64 ./cmd/phonellm

build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -o bin/$(BINARY)-linux-amd64 ./cmd/phonellm

clean:
	rm -rf bin/
