// GameTunnel Host — Combined server + client for LAN gaming.
//
// One player runs this to host a game. Other players connect to the
// host's public IP. No separate server setup required.
//
// Usage:
//
//	gtunnel-host -name Player1 -password myroom
//	gtunnel-host -addr :4700 -subnet 10.10.0.0/24 -name Host -room mygame
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/paths"
	"github.com/holipay/gametunnel/internal/server"
	"github.com/holipay/gametunnel/internal/singleinstance"
)

// Build info, set at build time via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	// Server flags
	addr := flag.String("addr", ":4700", "server listen address (UDP)")
	subnetStr := flag.String("subnet", "10.10.0.0/24", "virtual subnet (CIDR)")
	maxPlayers := flag.Int("max", 10, "max players")
	roomPass := flag.String("password", "", "room password (empty = no auth)")
	tcpAddr := flag.String("tcp-addr", "", "TCP listen address for fallback (e.g. :4700), empty = disabled")
	verboseFlag := flag.Bool("verbose", false, "enable verbose relay logging")

	// Client flags
	nameFlag := flag.String("name", "", "player name (default: hostname)")
	roomFlag := flag.String("room", "default", "room ID")
	langFlag := flag.String("lang", "zh", "language (zh or en)")

	// Common flags
	versionFlag := flag.Bool("version", false, "show version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-host %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	// Set language
	i18n.Set(i18n.ParseLang(*langFlag))

	// Single-instance check
	lock, err := singleinstance.Acquire("GameTunnel-Host")
	if err != nil {
		log.Fatalf("single instance: %v", err)
	}
	defer lock.Close()

	// Parse subnet
	_, subnet, err := net.ParseCIDR(*subnetStr)
	if err != nil {
		log.Fatalf("invalid subnet %q: %v", *subnetStr, err)
	}

	// Password strength warning
	if _, warnings := auth.CheckPasswordStrength(*roomPass); len(warnings) > 0 {
		for _, w := range warnings {
			log.Printf("[auth] %s", w)
		}
	}

	// Resolve player name
	playerName := *nameFlag
	if playerName == "" {
		hostname, _ := os.Hostname()
		playerName = hostname
	}

	// Setup logging
	logFile := setupLog()
	defer func() {
		if logFile != os.Stderr {
			logFile.Close()
		}
	}()

	// Print banner
	printBanner(*addr, subnet, *maxPlayers, *roomPass, playerName, *roomFlag)

	// Graceful shutdown context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// ====== Start server ======
	s, err := server.New(server.Config{
		Addr:       *addr,
		Subnet:     subnet,
		MaxPlayers: *maxPlayers,
		RoomPass:   *roomPass,
		Version:    Version,
		Lang:       i18n.ParseLang(*langFlag),
		TCPAddr:    *tcpAddr,
		Verbose:    *verboseFlag,
	})
	if err != nil {
		log.Fatalf("server start: %v", err)
	}

	go s.Run(ctx)
	log.Printf("[host] server started on %s", *addr)

	// Wait for server to bind the UDP port
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("udp", "127.0.0.1"+*addr, 10*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// ====== Start client ======
	serverAddr := "127.0.0.1" + extractPort(*addr)
	serverPublicIP := net.ParseIP("127.0.0.1")

	clientCfg := &client.Config{
		ServerAddr:   serverAddr,
		PlayerName:   playerName,
		RoomID:       *roomFlag,
		RoomPassword: *roomPass,
		Lang:         *langFlag,
	}

	app := client.NewApp(clientCfg)
	app.SetTUNFactory(newTUNFactory(serverPublicIP))

	log.Printf("[host] connecting client to %s as %q (room: %s)", serverAddr, playerName, *roomFlag)
	app.Connect(clientCfg)

	// ====== Wait for signal ======
	sig := <-sigCh
	fmt.Printf("\nReceived %v, shutting down...\n", sig)

	// Shutdown order: client first, then server
	app.Disconnect()
	log.Printf("[host] client disconnected")
	cancel()
	s.Close()
	log.Printf("[host] server stopped")
	fmt.Println("GameTunnel Host stopped.")
}

// extractPort extracts the port from an address like ":4700" or "127.0.0.1:4700".
func extractPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ":4700"
	}
	return ":" + port
}

func printBanner(addr string, subnet *net.IPNet, maxPlayers int, password, playerName, room string) {
	authStatus := "No auth"
	if password != "" {
		authStatus = "HMAC auth"
	}

	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("  GameTunnel Host %s", Version)
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("  Server:  %s (subnet: %s, max: %d, auth: %s)", addr, subnet, maxPlayers, authStatus)
	log.Printf("  Client:  %s (room: %s)", playerName, room)
	log.Printf("  Version: %s (commit: %s)", Version, Commit)
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("  Other players connect to your public IP on port %s", extractPort(addr))
	log.Printf("════════════════════════════════════════════════════════════")
}

const maxLogSize = 1 * 1024 * 1024

func setupLog() *os.File {
	logDir := paths.GameTunnelDir()
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.SetOutput(os.Stderr)
		log.Printf("create log dir: %v", err)
		return os.Stderr
	}
	logPath := filepath.Join(logDir, "gametunnel.log")
	logBackup := filepath.Join(logDir, "gametunnel.log.1")

	if info, err := os.Stat(logPath); err == nil && info.Size() > maxLogSize {
		os.Remove(logBackup)
		if err := os.Rename(logPath, logBackup); err != nil {
			log.Printf("rotate log: %v", err)
		}
	}

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.SetOutput(os.Stderr)
		return os.Stderr
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	return f
}
