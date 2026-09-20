.PHONY: all test clean

all: bin/hfs bin/hfsd

bin/hfs: $(shell find cmd/hfs internal -name '*.go') go.mod
	go build -o $@ ./cmd/hfs

# hfsd runs in the Space, so it is always a static linux/amd64 binary.
bin/hfsd: $(shell find cmd/hfsd internal/daemon -name '*.go') go.mod
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '-s -w' -o $@ ./cmd/hfsd

test:
	go test ./...

clean:
	rm -rf bin
