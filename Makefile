# GameTunnel Makefile
#
# Server: Linux (公网 VPS)
# Client: Windows CLI tool

.PHONY: all server client clean install-server

BINARY_DIR := bin
SERVER := $(BINARY_DIR)/gtunnel-server
CLIENT := $(BINARY_DIR)/gtunnel-client.exe
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

# ── Client (Windows CLI) ───────────────────────────────────────

client:
	@mkdir -p $(BINARY_DIR)
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(CLIENT) ./cmd/client

client-all: client client-arm64

client-arm64:
	@mkdir -p $(BINARY_DIR)
	GOOS=windows GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-arm64.exe ./cmd/client

# ── Dev / Test ─────────────────────────────────────────────────

run-server: server
	sudo $(SERVER) -addr :4700 -subnet 10.10.0.0/24

# ── Release ─────────────────────────────────────────────────────

release: client
	@mkdir -p $(BINARY_DIR)/release
	cp $(CLIENT) $(BINARY_DIR)/release/
	cp start.bat $(BINARY_DIR)/release/
	@# Copy wintun.dll from Go module cache if available
	@WINTUN=$$(find $$(go env GOMODCACHE) -path '*/wintun@*/dll/wintun_amd64.dll' 2>/dev/null | head -1); \
	if [ -n "$$WINTUN" ]; then \
		cp "$$WINTUN" $(BINARY_DIR)/release/wintun.dll; \
		echo "  Included wintun.dll"; \
	else \
		echo "  [WARN] wintun.dll not found in module cache"; \
	fi
	cd $(BINARY_DIR)/release && zip -9 ../GameTunnel-windows-amd64.zip ./*
	rm -rf $(BINARY_DIR)/release
	@echo "  Created $(BINARY_DIR)/GameTunnel-windows-amd64.zip"

clean:
	rm -rf $(BINARY_DIR)
