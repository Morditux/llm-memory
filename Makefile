BINARY   := llm-memory
PKG      := ./cmd/llm-memory
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
GOLANGCI     ?= $(shell command -v golangci-lint 2>/dev/null || echo $(shell go env GOPATH)/bin/golangci-lint)
GOLANGCI_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
PLATFORMS := linux/amd64 linux/arm64 darwin/arm64 windows/amd64

.PHONY: all build test cover lint fmt release clean

all: fmt lint test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BINARY) $(PKG)

test:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

lint:
	go vet ./...
	@if [ ! -x "$(GOLANGCI)" ]; then \
		echo "installing $(GOLANGCI_PKG)..."; \
		go install $(GOLANGCI_PKG); \
	fi
	$(GOLANGCI) run ./...

fmt:
	gofmt -s -w .

release:
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' \
			-o dist/$(BINARY)-$$os-$$arch$$ext $(PKG) || exit 1; \
	done

clean:
	rm -rf bin dist coverage.out
