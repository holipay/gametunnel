//go:build !windows

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("  GameTunnel GUI 客户端仅支持 Windows")
	fmt.Println("  Linux 请使用命令行版本: go build ./cmd/server")
	fmt.Println()
	fmt.Println("  如需在 Linux 上运行客户端 GUI，请使用:")
	fmt.Println("  GOOS=windows go build -o gtunnel-client.exe ./cmd/client")
	os.Exit(1)
}
