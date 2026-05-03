# GameTunnel Makefile

.PHONY: all server client clean install-server

BINARY_DIR := bin
SERVER := $(BINARY_DIR)/gtunnel-server
CLIENT := $(BINARY_DIR)/gtunnel-client

# Build for current platform
all: server client

server:
	@mkdir -p $(BINARY_DIR)
	go build -o $(SERVER) ./cmd/server

client:
	@mkdir -p $(BINARY_DIR)
	go build -o $(CLIENT) ./cmd/client

# Cross-compile client for common platforms
client-all: client-linux-amd64 client-linux-arm64 client-darwin-amd64 client-darwin-arm64 client-windows

client-linux-amd64:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY_DIR)/gtunnel-client-linux-amd64 ./cmd/client

client-linux-arm64:
	GOOS=linux GOARCH=arm64 go build -o $(BINARY_DIR)/gtunnel-client-linux-arm64 ./cmd/client

client-darwin-amd64:
	GOOS=darwin GOARCH=amd64 go build -o $(BINARY_DIR)/gtunnel-client-darwin-amd64 ./cmd/client

client-darwin-arm64:
	GOOS=darwin GOARCH=arm64 go build -o $(BINARY_DIR)/gtunnel-client-darwin-arm64 ./cmd/client

client-windows:
	GOOS=windows GOARCH=amd64 go build -o $(BINARY_DIR)/gtunnel-client-windows.exe ./cmd/client

# Install server binary to /usr/local/bin
install-server: server
	install -m 755 $(SERVER) /usr/local/bin/gtunnel-server

# Clean build artifacts
clean:
	rm -rf $(BINARY_DIR)

# Run server locally (for testing)
run-server: server
	sudo $(SERVER) -addr :4700 -subnet 10.10.0.0/24

# Run client locally (for testing)
run-client: client
	sudo $(CLIENT) -server 127.0.0.1:4700 -room test -name $(USER)
