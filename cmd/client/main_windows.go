//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/tun"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	user32               = syscall.NewLazyDLL("user32.dll")
	procCloseHandle      = kernel32.NewProc("CloseHandle")
	procCreateToolhelp32 = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First   = kernel32.NewProc("Process32FirstW")
	procProcess32Next    = kernel32.NewProc("Process32NextW")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procShowWindow       = user32.NewProc("ShowWindow")
)

type processEntry32 struct {
	Size            uint32
	Usage           uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	Threads         uint32
	ParentProcessID uint32
	PriorityClass   int32
	Flags           uint32
	ExeFile         [260]uint16
}

func main() {
	// Hide console window immediately to prevent flash during UAC elevation.
	// The elevated copy will show its own console window.
	if wnd, _, _ := procGetConsoleWindow.Call(); wnd != 0 {
		procShowWindow.Call(wnd, 0) // SW_HIDE
	}

	defer writeCrashLog()

	// Request admin rights if not elevated (needed for TUN device)
	requestAdmin()

	// Show console for the elevated (or already admin) process
	if wnd, _, _ := procGetConsoleWindow.Call(); wnd != 0 {
		procShowWindow.Call(wnd, 5) // SW_SHOW
	}

	windows.SetConsoleOutputCP(65001)

	// Prevent multiple instances
	checkSingleInstance()

	// Load config
	cfg := client.LoadConfig()

	// Set language from config
	if cfg.Lang != "" {
		i18n.Set(i18n.ParseLang(cfg.Lang))
	}

	// Parse server public IP for route exclusion
	serverPublicIP := parseHostIP(cfg.ServerAddr)

	// Setup TUN factory for Windows
	tunFactory := func(tunCfg client.TunConfig) (client.TunDevice, error) {
		return tun.New(tun.Config{
			VirtualIP:      tunCfg.VirtualIP,
			SubnetMask:     tunCfg.SubnetMask,
			ServerIP:       tunCfg.ServerIP,
			ServerPublicIP: serverPublicIP,
			MTU:            tunCfg.MTU,
		})
	}

	run(cfg, tunFactory)
}

func checkSingleInstance() {
	if !isAnotherInstanceRunning() {
		return
	}
	fmt.Println("GameTunnel is already running.")
	os.Exit(0)
}

func isAnotherInstanceRunning() bool {
	selfExe, err := os.Executable()
	if err != nil {
		return false
	}
	selfName := strings.ToLower(filepath.Base(selfExe))

	snapshot, _, _ := procCreateToolhelp32.Call(
		uintptr(0x00000002), // TH32CS_SNAPPROCESS
		0,
	)
	if snapshot == 0 || snapshot == uintptr(syscall.InvalidHandle) {
		return false
	}
	defer procCloseHandle.Call(snapshot)

	var entry processEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	ret, _, _ := procProcess32First.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
	if ret == 0 {
		return false
	}

	currentPID := uint32(os.Getpid())

	for {
		pid := entry.ProcessID
		if pid != currentPID {
			name := windows.UTF16PtrToString(&entry.ExeFile[0])
			if strings.ToLower(name) == selfName {
				return true
			}
		}
		ret, _, _ = procProcess32Next.Call(snapshot, uintptr(unsafe.Pointer(&entry)))
		if ret == 0 {
			break
		}
	}
	return false
}

func requestAdmin() {
	token := windows.GetCurrentProcessToken()
	if token.IsElevated() {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	// Console is already hidden by main(). Launch elevated copy.
	verb, _ := windows.UTF16PtrFromString("runas")
	exePath, _ := windows.UTF16PtrFromString(exe)

	if err := windows.ShellExecute(0, verb, exePath, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to elevate: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

func writeCrashLog() {
	r := recover()
	if r == nil {
		return
	}

	logDir := filepath.Join(appDataPath(), "GameTunnel")
	os.MkdirAll(logDir, 0755)
	f, err := os.OpenFile(filepath.Join(logDir, "crash.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()

	fmt.Fprintf(f, "=== Crash %s ===\n", "GameTunnel Client")
	fmt.Fprintf(f, "Panic: %v\n\n", r)
	fmt.Fprintf(f, "Stack:\n%s\n", debug.Stack())
}
