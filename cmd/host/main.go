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
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/hostconfig"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/logfile"
	"github.com/holipay/gametunnel/internal/server"
	"github.com/holipay/gametunnel/internal/singleinstance"
)

// Build info, set at build time via -ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

const defaultPort = ":4700"

// hostFlags holds the parsed CLI flags for both server and client sides.
type hostFlags struct {
	addr       string
	subnetStr  string
	maxPlayers int
	roomPass   string
	tcpAddr    string
	verbose    bool
	nameFlag   string
	roomFlag   string
	langFlag   string
	// Server-side flags
	maxPerIP    int
	bandwidth   int
	stateDir    string
	statusAddr  string
	statusToken string
	maxRooms    int
	logFile     string
}

// parseAndStart parses CLI flags (overriding config file values), creates
// the server, and starts it. Returns the client config, TUN factory, and server.
func parseAndStart() (*client.Config, func(client.TunConfig) (client.TunDevice, error), *server.Server) {
	// Load config file first — CLI flags override file values
	cfg := hostconfig.LoadHostConfig()

	var f hostFlags

	flag.StringVar(&f.addr, "addr", cfg.Addr, "server listen address (UDP)")
	flag.StringVar(&f.subnetStr, "subnet", cfg.Subnet, "virtual subnet (CIDR)")
	flag.IntVar(&f.maxPlayers, "max", cfg.MaxPlayers, "max players")
	flag.StringVar(&f.roomPass, "password", cfg.RoomPass, "room password (empty = no auth)")
	flag.StringVar(&f.tcpAddr, "tcp-addr", cfg.TCPAddr, "TCP listen address for fallback (e.g. :4700), empty = disabled")
	flag.BoolVar(&f.verbose, "verbose", cfg.Verbose, "enable verbose relay logging")
	flag.StringVar(&f.nameFlag, "name", cfg.PlayerName, "player name (default: hostname)")
	flag.StringVar(&f.roomFlag, "room", cfg.RoomID, "room ID")
	flag.StringVar(&f.langFlag, "lang", cfg.Lang, "language (zh or en)")
	flag.IntVar(&f.maxPerIP, "max-per-ip", cfg.MaxPerIP, "max connections per IP")
	flag.IntVar(&f.bandwidth, "bandwidth", cfg.Bandwidth, "per-client bandwidth limit (bytes/sec, 0 = unlimited)")
	flag.StringVar(&f.stateDir, "state-dir", cfg.StateDir, "state persistence directory (empty = disabled)")
	flag.StringVar(&f.statusAddr, "status-addr", cfg.StatusAddr, "status page HTTP address (empty = disabled)")
	flag.StringVar(&f.statusToken, "status-token", cfg.StatusToken, "status page access token (empty = no auth)")
	flag.IntVar(&f.maxRooms, "max-rooms", cfg.MaxRooms, "max auto-created rooms in multi-room mode")
	flag.StringVar(&f.logFile, "log-file", cfg.LogFile, "log file path (empty = stderr only)")

	versionFlag := flag.Bool("version", false, "show version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-host %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	i18n.Set(i18n.ParseLang(f.langFlag))

	// Parse subnet
	_, subnet, err := net.ParseCIDR(f.subnetStr)
	if err != nil {
		log.Fatalf("invalid subnet %q: %v", f.subnetStr, err)
	}

	// Password strength warning
	if _, warnings := auth.CheckPasswordStrength(f.roomPass); len(warnings) > 0 {
		for _, w := range warnings {
			log.Printf("[auth] %s", w)
		}
	}

	// Resolve player name
	playerName := f.nameFlag
	if playerName == "" {
		hostname, _ := os.Hostname()
		playerName = hostname
	}

	// Single-instance check
	lock, err := singleinstance.Acquire("GameTunnel-Host")
	if err != nil {
		log.Fatalf("single instance: %v", err)
	}
	defer lock.Close()

	// Start server — use Background context since the server manages its
	// own lifecycle via Close(); the caller must not cancel this context.
	s, err := server.New(server.Config{
		Addr:          f.addr,
		Subnet:        subnet,
		MaxPlayers:    f.maxPlayers,
		RoomPass:      f.roomPass,
		Version:       Version,
		Lang:          i18n.ParseLang(f.langFlag),
		TCPAddr:       f.tcpAddr,
		Verbose:       f.verbose,
		MaxPerIP:      f.maxPerIP,
		BandwidthLimit: f.bandwidth,
		StateDir:      f.stateDir,
		StatusAddr:    f.statusAddr,
		StatusToken:   f.statusToken,
		MaxRooms:      f.maxRooms,
	})
	if err != nil {
		log.Fatalf("server start: %v", err)
	}

	go s.Run(context.Background())
	s.WaitReady()
	log.Printf("[host] server started on %s", f.addr)

	// Build client config
	serverAddr := "127.0.0.1" + extractPort(f.addr)
	serverListenIP := net.ParseIP("127.0.0.1")

	clientCfg := &client.Config{
		ServerAddr:   serverAddr,
		PlayerName:   playerName,
		RoomID:       f.roomFlag,
		RoomPassword: f.roomPass,
		Lang:         f.langFlag,
		MTU:          client.DefaultMTU,
	}
	if err := clientCfg.Validate(); err != nil {
		log.Fatalf("client config: %v", err)
	}

	return clientCfg, newTUNFactory(serverListenIP), s
}

// run is the shared entry point called from platform-specific main().
func run(cfg *client.Config, tunFactory func(client.TunConfig) (client.TunDevice, error), s *server.Server) {
	logFile := logfile.Setup()
	defer func() {
		if logFile != os.Stderr {
			logFile.Close()
		}
	}()

	printBanner(cfg.ServerAddr, cfg.RoomPassword, cfg.PlayerName, cfg.RoomID)

	app := client.NewApp(cfg)
	app.SetTUNFactory(tunFactory)

	log.Printf("[host] connecting client to %s as %q (room: %s)", cfg.ServerAddr, cfg.PlayerName, cfg.RoomID)
	app.Connect(cfg)

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	fmt.Printf("\nReceived %v, shutting down...\n", sig)

	// Shutdown order: client first, then server
	app.Disconnect()
	log.Printf("[host] client disconnected")
	s.Close()
	log.Printf("[host] server stopped")
	fmt.Println("GameTunnel Host stopped.")
}

// extractPort extracts the port from an address like ":4700" or "127.0.0.1:4700".
func extractPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return defaultPort
	}
	return ":" + port
}

func printBanner(addr, password, playerName, room string) {
	authStatus := "No auth"
	if password != "" {
		authStatus = "HMAC auth"
	}

	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("  GameTunnel Host %s", Version)
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("  Client:  %s (room: %s)", playerName, room)
	log.Printf("  Auth:    %s", authStatus)
	log.Printf("  Version: %s (commit: %s)", Version, Commit)
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("  Other players connect to your public IP on port %s", addr)
	log.Printf("════════════════════════════════════════════════════════════")
}
