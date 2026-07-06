//go:build windows

package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/crashlog"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/singleinstance"
	"github.com/holipay/gametunnel/internal/tun"
)

var (
	modKernel32         = syscall.NewLazyDLL("kernel32.dll")
	modUser32           = syscall.NewLazyDLL("user32.dll")
	modComctl32         = syscall.NewLazyDLL("comctl32.dll")
	procLoadLibrary     = modKernel32.NewProc("LoadLibraryW")
	procGetConsoleWindow = modKernel32.NewProc("GetConsoleWindow")
	procShowWindow      = modUser32.NewProc("ShowWindow")
	procInitCommonCtrls = modComctl32.NewProc("InitCommonControlsEx")
)

type initCommonControlsEx struct {
	dwSize uint32
	dwICC  uint32
}

func main() {
	defer crashlog.WriteCrashLog("GameTunnel Client")

	// Step 1: Force-load comctl32.dll to ensure tooltip class is available.
	// Some Windows configurations don't load it automatically.
	comctl32, _ := syscall.UTF16PtrFromString("comctl32.dll")
	procLoadLibrary.Call(uintptr(unsafe.Pointer(comctl32)))

	// Step 2: Initialize common controls with tooltip class enabled.
	var icc initCommonControlsEx
	icc.dwSize = uint32(unsafe.Sizeof(icc))
	icc.dwICC = 0x000000FF | 0x00000080 // ICC_WIN95_CLASSES | ICC_TOOLTIP_CLASS
	ret, _, _ := procInitCommonCtrls.Call(uintptr(unsafe.Pointer(&icc)))
	if ret == 0 {
		log.Printf("warning: InitCommonControlsEx failed")
	}

	// Step 3: Lock OS thread for Win32 GUI (walk requirement)
	runtime.LockOSThread()

	// Request admin rights if not elevated (needed for TUN device)
	requestAdmin()

	// Prevent multiple instances
	if _, err := singleinstance.Acquire("GameTunnel-Client"); err != nil {
		log.Printf("single instance check: %v", err)
		fmt.Println("GameTunnel is already running.")
		os.Exit(0)
	}

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

	// Launch GUI
	runWindows(cfg, tunFactory)

	// Hide console after runWindows returns
	hideConsole()
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

	verb, _ := windows.UTF16PtrFromString("runas")
	exePath, _ := windows.UTF16PtrFromString(exe)

	if err := windows.ShellExecute(0, verb, exePath, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to elevate: %v\n", err)
		os.Exit(1)
	}

	os.Exit(0)
}

func hideConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		procShowWindow.Call(hwnd, 0) // SW_HIDE
	}
}
