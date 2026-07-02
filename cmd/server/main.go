// GameTunnel Server — LAN Gaming Tunnel
//
// Usage:
//
//	gtunnel-server -addr :4700 -subnet 10.10.0.0/24 -max 10
//	gtunnel-server -addr :4700 -password myroomsecret
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/i18n"
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
	addr := flag.String("addr", ":4700", "listen address (UDP)")
	subnetStr := flag.String("subnet", "10.10.0.0/24", "virtual subnet (CIDR)")
	maxPlayers := flag.Int("max", 10, "max players")
	roomPass := flag.String("password", "", "room password (empty = no auth)")
	statusAddr := flag.String("status-addr", "", "status page address (HTTP), e.g. :4701")
	statusToken := flag.String("status-token", "", "status page access token (empty = no auth)")
	maxPerIP := flag.Int("max-per-ip", 3, "max connections per IP address")
	stateDir := flag.String("state-dir", "", "directory for room state persistence (empty = disabled)")
	multiRoom := flag.Bool("rooms", false, "enable multi-room mode (each room gets independent subnet)")
	maxRooms := flag.Int("max-rooms", 64, "max auto-created rooms in multi-room mode")
	bandwidthLimit := flag.Int("bandwidth", 0, "per-client outbound bandwidth limit in bytes/sec (0 = default 10Mbps)")
	tcpAddr := flag.String("tcp-addr", "", "TCP listen address for fallback (e.g. :4700), empty = disabled")
	langFlag := flag.String("lang", "zh", "language (zh or en)")
	verboseFlag := flag.Bool("verbose", false, "enable verbose relay logging")
	logFileFlag := flag.String("log-file", "", "log file path (empty = stderr only)")
	pprofAddrFlag := flag.String("pprof-addr", "", "pprof HTTP address (e.g. localhost:6060), empty = disabled")
	versionFlag := flag.Bool("version", false, "show version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-server %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	// Set language
	i18n.Set(i18n.ParseLang(*langFlag))
	t := i18n.T()

	// ====== Log file setup ======
	if *logFileFlag != "" {
		f, err := setupServerLog(*logFileFlag)
		if err != nil {
			log.Fatalf("open log file: %v", err)
		}
		defer f.Close()
	}

	// ====== Single-instance check ======
	lock, err := singleinstance.Acquire("GameTunnel-Server")
	if err != nil {
		log.Fatalf(t.ServerInstRunning, err)
	}
	defer lock.Close()

	_, subnet, err := net.ParseCIDR(*subnetStr)
	if err != nil {
		log.Fatalf(t.ServerSubnetFail, *subnetStr, err)
	}

	// Password strength warning
	if _, warnings := auth.CheckPasswordStrength(*roomPass); len(warnings) > 0 {
		for _, w := range warnings {
			log.Printf("[auth] %s", w)
		}
	}

	s, err := server.New(server.Config{
		Addr:       *addr,
		Subnet:     subnet,
		MaxPlayers: *maxPlayers,
		RoomPass:   *roomPass,
		StatusAddr:  *statusAddr,
		StatusToken: *statusToken,
		Version:    Version,
		Lang:       i18n.ParseLang(*langFlag),
		MaxPerIP:       *maxPerIP,
		StateDir:       *stateDir,
		MultiRoom:      *multiRoom,
		MaxRooms:       *maxRooms,
		BandwidthLimit: *bandwidthLimit,
		TCPAddr:        *tcpAddr,
		Verbose:        *verboseFlag,
	})
	if err != nil {
		log.Fatalf(t.ServerStartFail, err)
	}

	// ====== pprof HTTP server ======
	if *pprofAddrFlag != "" {
		pprofLn, err := net.Listen("tcp", *pprofAddrFlag)
		if err != nil {
			log.Fatalf("pprof listen: %v", err)
		}
		s.SetPprofListener(pprofLn)
		go func() {
			log.Printf("pprof listening on %s", pprofLn.Addr())
			if err := http.Serve(pprofLn, nil); err != nil {
				log.Printf("pprof server: %v", err)
			}
		}()
	}

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf(t.ServerSignal, sig)
		cancel()
		s.Close()
	}()

	// Print banner
	authStatus := t.ServerNoAuth
	if *roomPass != "" {
		authStatus = t.ServerHMACAuth
	}
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("%s", t.ServerBanner)
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("▎  %-7s %-31s ▎", t.ServerAddr+":", *addr)
	log.Printf("▎  %-7s %-31s ▎", t.ServerSubnet+":", subnet.String())
	log.Printf("▎  %-7s %-31d ▎", t.ServerMaxPlayers+":", *maxPlayers)
	log.Printf("▎  %-7s %-31s ▎", t.ServerAuth+":", authStatus)
	log.Printf("▎  %-7s %-31s ▎", t.ServerVersion+":", Version)
	log.Printf("▎  %-7s %-31s ▎", "Commit:", Commit)
	log.Printf("▎  %-7s %-31s ▎", "Built:", BuildTime)
	if *statusAddr != "" {
		log.Printf("▎  %-7s %-31s ▎", t.ServerStatusPage+":", fmt.Sprintf("http://%s", *statusAddr))
	}
	log.Printf("════════════════════════════════════════════════════════════")

	s.Run(ctx)
	log.Println(t.ServerShutdown)
}

const maxLogSize = 1 * 1024 * 1024 // 1 MB

// setupServerLog opens a log file and redirects log output to both the file and stderr.
// Rotation is simple: when the file exceeds maxLogSize, it's renamed to .1 before reuse.
func setupServerLog(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create log dir: %w", err)
		}
	}

	backup := path + ".1"
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSize {
		os.Remove(backup)
		if err := os.Rename(path, backup); err != nil {
			log.Printf("rotate log: %v", err)
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	return f, nil
}
