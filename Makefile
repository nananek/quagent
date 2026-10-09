# quagent は VM に自分自身を持ち込むので、静的リンク (CGO_ENABLED=0) でビルドする。
GO      ?= go
PREFIX  ?= $(HOME)/.local
BIN     := bin/quagent

.PHONY: build install test vet coverage clean

build:
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/quagent

install: build
	install -Dm755 $(BIN) $(DESTDIR)$(PREFIX)/bin/quagent

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

clean:
	rm -rf bin coverage.out
