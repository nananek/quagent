# quagent は VM に自分自身を持ち込むので、静的リンク (CGO_ENABLED=0) でビルドする。
GO      ?= go
PREFIX  ?= $(HOME)/.local
BIN     := bin/quagent

# Web 遠隔操作 (hub) のブラウザ資産 (xterm.js) の版。改行や改変を避けるため固定する。
HUB_ASSETS      := internal/hub/assets
XTERM_VERSION   := 5.5.0
XTERM_FIT_VERSION := 0.10.0
XTERM_CDN       := https://cdn.jsdelivr.net/npm

.PHONY: build install test vet coverage clean docker-test hub-assets install-hub-unit

build:
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/quagent

install: build
	install -Dm755 $(BIN) $(DESTDIR)$(PREFIX)/bin/quagent

# hub の systemd user unit を配置する (有効化は README を参照)。
install-hub-unit:
	install -Dm644 contrib/systemd/quagent-hub.service \
		$(DESTDIR)$(HOME)/.config/systemd/user/quagent-hub.service

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

# hub のブラウザ資産 (xterm.js / addon-fit / CSS) を取得して同梱する。
# 初回と更新時だけ実行する (ネットワークが必要)。取得物は MIT ライセンス。
hub-assets:
	curl -fsSL $(XTERM_CDN)/@xterm/xterm@$(XTERM_VERSION)/lib/xterm.js -o $(HUB_ASSETS)/xterm.js
	curl -fsSL $(XTERM_CDN)/@xterm/xterm@$(XTERM_VERSION)/css/xterm.css -o $(HUB_ASSETS)/xterm.css
	curl -fsSL $(XTERM_CDN)/@xterm/addon-fit@$(XTERM_FIT_VERSION)/lib/addon-fit.js -o $(HUB_ASSETS)/addon-fit.js
	curl -fsSL $(XTERM_CDN)/@xterm/xterm@$(XTERM_VERSION)/LICENSE -o $(HUB_ASSETS)/XTERM-LICENSE

coverage:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out

docker-test:
	docker build -f Dockerfile.test -t quagent-test .
	docker run $(DOCKER_ARGS) --rm -v $(CURDIR):/work quagent-test

clean:
	rm -rf bin coverage.out
