# GameTunnel Makefile
#
# Server: Linux (公网 VPS)
# Client: Windows / Linux / macOS

.PHONY: all server client client-linux client-darwin clean install-server release release-client release-server test

BINARY_DIR := bin
SERVER := $(BINARY_DIR)/gtunnel-server
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-s -w -X main.Version=$(VERSION)"

all: server client

# ── Server (Linux) ─────────────────────────────────────────────

server:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(SERVER) ./cmd/server

server-linux-amd64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-linux-amd64 ./cmd/server

server-linux-arm64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-linux-arm64 ./cmd/server

install-server: server
	install -m 755 $(SERVER) /usr/local/bin/gtunnel-server

# ── Client (跨平台) ───────────────────────────────────────────

# Windows 客户端（默认，可在任意平台交叉编译）
client:
	@mkdir -p $(BINARY_DIR)
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client.exe ./cmd/client

client-windows-arm64:
	@mkdir -p $(BINARY_DIR)
	GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-windows-arm64.exe ./cmd/client

# Linux 客户端（需在 Linux 上编译，依赖 gtk3 + libayatana-appindicator3）
client-linux:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=1 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-linux ./cmd/client

client-linux-amd64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-linux-amd64 ./cmd/client

client-linux-arm64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-linux-arm64 ./cmd/client

# macOS 客户端（需在 macOS 上编译，或用 CGO 交叉编译工具链）
client-darwin:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=1 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-darwin ./cmd/client

client-darwin-amd64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-darwin-amd64 ./cmd/client

client-darwin-arm64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-darwin-arm64 ./cmd/client

# 所有平台客户端
client-all: client client-windows-arm64 client-linux-amd64 client-darwin-amd64 client-darwin-arm64

# ── Dev / Test ─────────────────────────────────────────────────

test:
	go test -v -count=1 ./...

run-server: server
	sudo $(SERVER) -addr :4700 -subnet 10.10.0.0/24

# ── Release ─────────────────────────────────────────────────────

release: release-client release-client-linux release-client-darwin release-server

release-client: client
	@mkdir -p $(BINARY_DIR)/release
	cp $(BINARY_DIR)/gtunnel-client.exe $(BINARY_DIR)/release/
	@if [ -f $(BINARY_DIR)/wintun.dll ]; then \
		cp $(BINARY_DIR)/wintun.dll $(BINARY_DIR)/release/; \
		echo "  Included wintun.dll"; \
	else \
		echo "  [WARN] $(BINARY_DIR)/wintun.dll not found"; \
	fi
	cp configs/config.ini $(BINARY_DIR)/release/config.ini
	cd $(BINARY_DIR)/release && zip -9 ../GameTunnel-Client-windows-amd64.zip ./*
	rm -rf $(BINARY_DIR)/release
	@echo "  Created $(BINARY_DIR)/GameTunnel-Client-windows-amd64.zip"

release-client-linux: client-linux-amd64
	@mkdir -p $(BINARY_DIR)/release-linux
	cp $(BINARY_DIR)/gtunnel-client-linux-amd64 $(BINARY_DIR)/release-linux/gtunnel-client
	cp configs/config.ini $(BINARY_DIR)/release-linux/config.ini
	cd $(BINARY_DIR)/release-linux && tar czf ../GameTunnel-Client-linux-amd64.tar.gz gtunnel-client config.ini
	rm -rf $(BINARY_DIR)/release-linux
	@echo "  Created $(BINARY_DIR)/GameTunnel-Client-linux-amd64.tar.gz"

release-client-darwin: client-darwin-amd64 client-darwin-arm64
	@mkdir -p $(BINARY_DIR)/release-darwin
	cp $(BINARY_DIR)/gtunnel-client-darwin-amd64 $(BINARY_DIR)/release-darwin/gtunnel-client
	cp $(BINARY_DIR)/gtunnel-client-darwin-arm64 $(BINARY_DIR)/release-darwin/gtunnel-client-arm64
	cp configs/config.ini $(BINARY_DIR)/release-darwin/config.ini
	cd $(BINARY_DIR)/release-darwin && tar czf ../GameTunnel-Client-darwin.tar.gz gtunnel-client gtunnel-client-arm64 config.ini
	rm -rf $(BINARY_DIR)/release-darwin
	@echo "  Created $(BINARY_DIR)/GameTunnel-Client-darwin.tar.gz"

release-server: server-linux-amd64
	@mkdir -p $(BINARY_DIR)/release-server
	cp $(BINARY_DIR)/gtunnel-server-linux-amd64 $(BINARY_DIR)/release-server/gtunnel-server
	cd $(BINARY_DIR)/release-server && tar czf ../GameTunnel-Server-linux-amd64.tar.gz gtunnel-server
	rm -rf $(BINARY_DIR)/release-server
	@echo "  Created $(BINARY_DIR)/GameTunnel-Server-linux-amd64.tar.gz"

clean:
	rm -rf $(BINARY_DIR)
