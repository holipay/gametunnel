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
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

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
	langFlag := flag.String("lang", "zh", "language (zh or en)")
	versionFlag := flag.Bool("version", false, "show version")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-server %s (commit: %s, built: %s)\n", Version, Commit, BuildTime)
		os.Exit(0)
	}

	// Set language
	i18n.Set(i18n.ParseLang(*langFlag))
	t := i18n.T()

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

	s, err := server.New(server.Config{
		Addr:       *addr,
		Subnet:     subnet,
		MaxPlayers: *maxPlayers,
		RoomPass:   *roomPass,
		StatusAddr:  *statusAddr,
		StatusToken: *statusToken,
		Version:    Version,
		Lang:       i18n.ParseLang(*langFlag),
		MaxPerIP:   *maxPerIP,
		StateDir:   *stateDir,
		MultiRoom:  *multiRoom,
>>>>>>> eb7f26f (feat: add -rooms flag for multi-room mode)
	})
	if err != nil {
		log.Fatalf(t.ServerStartFail, err)
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
	log.Printf(t.ServerBanner)
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
