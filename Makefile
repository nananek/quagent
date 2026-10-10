# quagent は VM に自分自身を持ち込むので、静的リンク (CGO_ENABLED=0) でビルドする。
GO      ?= go
PREFIX  ?= $(HOME)/.local
BIN     := bin/quagent

.PHONY: build install test vet coverage clean docker-test

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

docker-test:
	docker build -f Dockerfile.test -t quagent-test .
	docker run --net=host -e GOPROXY="http://127.0.0.1:5000/go,direct" --rm -v $(CURDIR):/work quagent-test

clean:
	rm -rf bin coverage.out
