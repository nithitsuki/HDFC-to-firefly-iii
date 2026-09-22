BINARY := hdfc2ff
PKG    := ./cmd/hdfc2ff

.PHONY: build test vet check samples clean

## build: static binary, no cgo, copyable to any machine of the same architecture
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

check: vet test

## samples: report what the parser makes of the real emails in testdata/samples
samples:
	go test ./internal/hdfcmail/ -run TestSamples -v

clean:
	rm -f $(BINARY)

help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
