//go:build windows

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/tun"
)

var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleWindow    = kernel32.NewProc("GetConsoleWindow")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procCreateToolhelp32    = kernel32.NewProc("CreateToolhelp32Snapshot")
	procProcess32First      = kernel32.NewProc("Process32FirstW")
	procProcess32Next       = kernel32.NewProc("Process32NextW")
	user32                  = syscall.NewLazyDLL("user32.dll")
	procShowWindow          = user32.NewProc("ShowWindow")
)

// processEntry32 matches the Win32 PROCESSENTRY32W structure.
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
	windows.SetConsoleOutputCP(65001)

	// Hide the console window (we only use the tray icon)
	hideConsole()

	// Request admin rights if not elevated
	requestAdmin()

	// Prevent multiple instances (must be after requestAdmin, since the
	// non-elevated copy exits before reaching here)
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

// checkSingleInstance enumerates running processes to detect another
// gtunnel-client instance. This replaces the previous Global\ mutex approach
// which failed when the elevated (runas) process ran in a different security
// context than the original mutex holder.
func checkSingleInstance() {
	if !isAnotherInstanceRunning() {
		return
	}
	windows.MessageBox(0,
		windows.StringToUTF16Ptr("GameTunnel 已经在运行中，请检查右下角系统托盘图标。\nGameTunnel is already running. Check the system tray icon."),
		windows.StringToUTF16Ptr("GameTunnel"),
		windows.MB_OK|windows.MB_ICONWARNING)
	os.Exit(0)
}

// isAnotherInstanceRunning checks if another gtunnel-client process is already
// running by enumerating processes via CreateToolhelp32Snapshot. It compares
// each process's executable name case-insensitively against our own.
func isAnotherInstanceRunning() bool {
	// Determine our own executable name for comparison.
	selfExe, err := os.Executable()
	if err != nil {
		return false // can't determine; allow startup
	}
	selfName := strings.ToLower(filepath.Base(selfExe))

	snapshot, _, _ := procCreateToolhelp32.Call(
		uintptr(0x00000002), // TH32CS_SNAPPROCESS
		0,
	)
	if snapshot == 0 || snapshot == uintptr(syscall.InvalidHandle) {
		return false // snapshot failed; allow startup
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

// hideConsole hides the console window.
func hideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, 0) // SW_HIDE = 0
	}
}

// requestAdmin checks if the process is running with admin rights.
// If not, re-launches with "runas" verb (UAC prompt).
func requestAdmin() {
	token := windows.GetCurrentProcessToken()
	if token.IsElevated() {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}

	verb, _ := windows.UTF16PtrFromString("runas")
	exePath, _ := windows.UTF16PtrFromString(exe)

	if err := windows.ShellExecute(0, verb, exePath, nil, nil, windows.SW_HIDE); err != nil {
		windows.MessageBox(0,
			windows.StringToUTF16Ptr(fmt.Sprintf(i18n.T().ErrElevateFailed, err)),
			windows.StringToUTF16Ptr("GameTunnel"),
			windows.MB_OK|windows.MB_ICONERROR)
		os.Exit(1)
	}

	os.Exit(0)
}

// parseHostIP extracts the IP from a "host:port" address string.
// Returns nil if parsing fails.
func parseHostIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Try as bare IP
		return net.ParseIP(addr)
	}
	return net.ParseIP(host)
}
