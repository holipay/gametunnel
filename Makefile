# GameTunnel Makefile
#
# Server: Linux (公网 VPS)
# Client: Windows only (星际争霸1玩家)

.PHONY: all server client clean install-server

BINARY_DIR := bin
SERVER := $(BINARY_DIR)/gtunnel-server
CLIENT := $(BINARY_DIR)/gtunnel-client.exe

all: server client

# ── Server (Linux) ─────────────────────────────────────────────

server:
	@mkdir -p $(BINARY_DIR)
	go build -o $(SERVER) ./cmd/server

# 交叉编译 Linux 服务器（在 macOS/Windows 上编译用）
server-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY_DIR)/gtunnel-server-linux-amd64 ./cmd/server

server-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -o $(BINARY_DIR)/gtunnel-server-linux-arm64 ./cmd/server

install-server: server
	install -m 755 $(SERVER) /usr/local/bin/gtunnel-server

# ── Client (Windows only) ─────────────────────────────────────

client:
	@mkdir -p $(BINARY_DIR)
	GOOS=windows GOARCH=amd64 go build -ldflags -H=windowsgui -o $(CLIENT) ./cmd/client

client-all: client client-arm64

client-arm64:
	@mkdir -p $(BINARY_DIR)
	GOOS=windows GOARCH=arm64 go build -ldflags -H=windowsgui -o $(BINARY_DIR)/gtunnel-client-arm64.exe ./cmd/client

# ── Dev / Test ─────────────────────────────────────────────────

run-server: server
	sudo $(SERVER) -addr :4700 -subnet 10.10.0.0/24

clean:
	rm -rf $(BINARY_DIR)
