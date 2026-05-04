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
	go build $(LDFLAGS) -o $(SERVER) ./cmd/server

server-linux-amd64:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-linux-amd64 ./cmd/server

server-linux-arm64:
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-linux-arm64 ./cmd/server

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

clean:
	rm -rf $(BINARY_DIR)
