VERSION ?= dev
PREFIX ?= $(HOME)/.local

.PHONY: build install test check release

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/tuna ./cmd/tuna

install: build
	install -d "$(PREFIX)/bin"
	install -m 755 bin/tuna "$(PREFIX)/bin/tuna"

test:
	go test -race ./...

check: test
	go vet ./...

release:
	mkdir -p dist
	@for os in darwin linux; do \
		for arch in arm64 amd64; do \
			CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o dist/tuna_$${os}_$${arch} ./cmd/tuna || exit 1; \
		done; \
	done
	cd dist && shasum -a 256 tuna_* > checksums.txt
