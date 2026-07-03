# GameTunnel Makefile
#
# Server: Linux / OpenWrt
# Client: Windows

.PHONY: all server client client-all clean install-server release release-client release-server release-openwrt test bench server-openwrt server-openwrt-arm64 server-openwrt-armv7 server-linux-arm64 vendor vendor-check

BINARY_DIR := bin
SERVER := $(BINARY_DIR)/gtunnel-server-linux-amd64
VERSION       := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT        := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME    := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
GEN_VERSIONINFO := scripts/gen-versioninfo.sh
LDFLAGS    := -ldflags "-s -w \
	-X main.Version=$(VERSION) \
	-X main.Commit=$(COMMIT) \
	-X main.BuildTime=$(BUILD_TIME)"

all: server client server-openwrt

# ── Server ────────────────────────────────────────────────────

server:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build -mod=vendor $(LDFLAGS) -o $(SERVER) ./cmd/server

server-linux-amd64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-linux-amd64 ./cmd/server

server-linux-arm64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-linux-arm64 ./cmd/server

server-linux-armv7:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-linux-armv7 ./cmd/server

server-windows-amd64:
	@mkdir -p $(BINARY_DIR)
	$(GEN_VERSIONINFO) cmd/server "gtunnel-server-windows-amd64.exe" "GameTunnel Server" $(VERSION) 64
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-windows-amd64.exe ./cmd/server
	@rm -f cmd/server/versioninfo.syso

server-windows-x86:
	@mkdir -p $(BINARY_DIR)
	$(GEN_VERSIONINFO) cmd/server "gtunnel-server-windows-x86.exe" "GameTunnel Server (x86)" $(VERSION) 32
	CGO_ENABLED=0 GOOS=windows GOARCH=386 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-windows-x86.exe ./cmd/server
	@rm -f cmd/server/versioninfo.syso

# ── Server (OpenWrt) ──────────────────────────────────────────
# 中高端 OpenWrt 设备：NanoPi R2S/R4S/R5S, 树莓派 4/5, GL.iNet 等

server-openwrt-arm64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-openwrt-arm64 ./cmd/server

server-openwrt-armv7:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-server-openwrt-armv7 ./cmd/server

# 所有 OpenWrt 架构
server-openwrt: server-openwrt-arm64 server-openwrt-armv7

install-server: server
	install -m 755 $(SERVER) /usr/local/bin/gtunnel-server

# ── Client (Windows) ─────────────────────────────────────────

client:
	@mkdir -p $(BINARY_DIR)
	$(GEN_VERSIONINFO) cmd/client "gtunnel-client.exe" "GameTunnel Client" $(VERSION) 64
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client.exe ./cmd/client
	@rm -f cmd/client/versioninfo.syso

client-windows-x86:
	@mkdir -p $(BINARY_DIR)
	$(GEN_VERSIONINFO) cmd/client "gtunnel-client.exe" "GameTunnel Client (x86)" $(VERSION) 32
	CGO_ENABLED=0 GOOS=windows GOARCH=386 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-windows-x86.exe ./cmd/client
	@rm -f cmd/client/versioninfo.syso

# 所有平台客户端
client-all: client client-windows-x86 client-linux-amd64

client-linux-amd64:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor $(LDFLAGS) -o $(BINARY_DIR)/gtunnel-client-linux-amd64 ./cmd/client

# ── Dev / Test ─────────────────────────────────────────────────

test:
	go test -v -count=1 -mod=vendor ./...

bench:
	go test -bench=. -benchmem -count=3 -mod=vendor ./...

vet:
	go vet -mod=vendor ./...

# ── Vendor (dependency lock) ────────────────────────────────────
# Locks all dependencies into vendor/ for reproducible builds.
# Commit vendor/ after running this.

vendor:
	go mod vendor
	@echo "  Dependencies vendored. Commit the vendor/ directory."

vendor-check:
	@go mod verify
	@echo "  All dependencies verified."

run-server: server
	sudo $(SERVER) -addr :4700 -subnet 10.10.0.0/24

# ── Release ─────────────────────────────────────────────────────

release: release-client release-server release-openwrt

release-client: client client-windows-x86 client-linux-amd64
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
	@mkdir -p $(BINARY_DIR)/release-x86
	cp $(BINARY_DIR)/gtunnel-client-windows-x86.exe $(BINARY_DIR)/release-x86/gtunnel-client.exe
	@if [ -f $(BINARY_DIR)/wintun.dll ]; then \
		cp $(BINARY_DIR)/wintun.dll $(BINARY_DIR)/release-x86/; \
	fi
	cp configs/config.ini $(BINARY_DIR)/release-x86/config.ini
	cd $(BINARY_DIR)/release-x86 && zip -9 ../GameTunnel-Client-windows-x86.zip ./*
	rm -rf $(BINARY_DIR)/release-x86
	@echo "  Created $(BINARY_DIR)/GameTunnel-Client-windows-x86.zip"
	@mkdir -p $(BINARY_DIR)/release-linux
	cp $(BINARY_DIR)/gtunnel-client-linux-amd64 $(BINARY_DIR)/release-linux/gtunnel-client
	cp configs/config.ini $(BINARY_DIR)/release-linux/config.ini
	cd $(BINARY_DIR)/release-linux && tar czf ../GameTunnel-Client-linux-amd64.tar.gz ./*
	rm -rf $(BINARY_DIR)/release-linux
	@echo "  Created $(BINARY_DIR)/GameTunnel-Client-linux-amd64.tar.gz"

release-server: server-linux-amd64
	@echo "  Created $(BINARY_DIR)/gtunnel-server-linux-amd64"

# ── Release (OpenWrt) ──────────────────────────────────────────

release-openwrt: server-openwrt-arm64 server-openwrt-armv7
	@mkdir -p $(BINARY_DIR)/release-openwrt-arm64
	cp $(BINARY_DIR)/gtunnel-server-openwrt-arm64 $(BINARY_DIR)/release-openwrt-arm64/gtunnel-server
	cp scripts/install-openwrt.sh $(BINARY_DIR)/release-openwrt-arm64/install-openwrt.sh
	chmod +x $(BINARY_DIR)/release-openwrt-arm64/install-openwrt.sh
	cd $(BINARY_DIR)/release-openwrt-arm64 && tar czf ../GameTunnel-Server-openwrt-arm64.tar.gz ./*
	rm -rf $(BINARY_DIR)/release-openwrt-arm64
	@echo "  Created $(BINARY_DIR)/GameTunnel-Server-openwrt-arm64.tar.gz"
	@mkdir -p $(BINARY_DIR)/release-openwrt-armv7
	cp $(BINARY_DIR)/gtunnel-server-openwrt-armv7 $(BINARY_DIR)/release-openwrt-armv7/gtunnel-server
	cp scripts/install-openwrt.sh $(BINARY_DIR)/release-openwrt-armv7/install-openwrt.sh
	chmod +x $(BINARY_DIR)/release-openwrt-armv7/install-openwrt.sh
	cd $(BINARY_DIR)/release-openwrt-armv7 && tar czf ../GameTunnel-Server-openwrt-armv7.tar.gz ./*
	rm -rf $(BINARY_DIR)/release-openwrt-armv7
	@echo "  Created $(BINARY_DIR)/GameTunnel-Server-openwrt-armv7.tar.gz"

clean:
	rm -rf $(BINARY_DIR)
	rm -f cmd/client/versioninfo.syso cmd/server/versioninfo.syso
