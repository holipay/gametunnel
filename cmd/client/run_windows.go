//go:build windows

package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/client/ui"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/logfile"
)

const trayCBMsg = 0x401

var (
	u32 = syscall.NewLazyDLL("user32.dll")
	s32 = syscall.NewLazyDLL("shell32.dll")
	k32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClass = u32.NewProc("RegisterClassExW")
	pCreateWindow  = u32.NewProc("CreateWindowExW")
	pDefWndProc    = u32.NewProc("DefWindowProcW")
	pPostQuit      = u32.NewProc("PostQuitMessage")
	pGetMsg        = u32.NewProc("GetMessageW")
	pTranslate     = u32.NewProc("TranslateMessage")
	pDispatch      = u32.NewProc("DispatchMessageW")
	pLoadIcon      = u32.NewProc("LoadIconW")
	pShowWin       = u32.NewProc("ShowWindow")
	pShellNotify   = s32.NewProc("Shell_NotifyIconW")
	pGetModHandle  = k32.NewProc("GetModuleHandleW")

	pCreateMenu    = u32.NewProc("CreatePopupMenu")
	pAppendMenu    = u32.NewProc("AppendMenuW")
	pTrackMenu     = u32.NewProc("TrackPopupMenu")
	pDestroyMenu   = u32.NewProc("DestroyMenu")
	pGetCursorPos  = u32.NewProc("GetCursorPos")
	pSetForeground = u32.NewProc("SetForegroundWindow")
)

// NOTIFYICONDATAW — matches the Windows API layout exactly.
// Using Go struct avoids manual byte-offset errors on 64-bit.
type nidW struct {
	cbSize           uint32
	_                [4]byte // padding: cbSize(4) → hWnd(8-byte align)
	hWnd             uintptr
	uID              uint32
	_                [4]byte // padding: uID(4) → uFlags(8-byte align)
	uFlags           uint32
	uCallbackMessage uint32
	_                [4]byte // padding to 8-byte align
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	_                [4]byte // padding
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

var (
	app     *client.App
	msgHwnd uintptr
	mwOpen  bool
)

func runWindows(cfg *client.Config, tunFactory func(client.TunConfig) (client.TunDevice, error)) {
	logFile := logfile.Setup(cfg.LogFile)
	defer func() {
		if logFile != nil {
			logFile.Close()
		}
	}()

	cleanup, err := setupFirewallPlatform()
	if err != nil {
		log.Printf("firewall setup failed: %v (non-fatal)", err)
	}
	defer cleanup()

	if _, warnings := auth.CheckPasswordStrength(cfg.RoomPassword); len(warnings) > 0 {
		for _, w := range warnings {
			log.Printf("[auth] %s", w)
		}
	}

	app = client.NewApp(cfg)
	app.SetTUNFactory(tunFactory)

	// Message-only window for tray events
	msgHwnd = createMsgWindow()

	// Create tray icon
	hIcon, _, _ := pLoadIcon.Call(0, 32512) // IDI_APPLICATION
	addTray(hIcon, "GameTunnel")

	// Settings dialog on first run
	if cfg.ServerAddr == "" {
		tmp := tmpForm()
		newCfg := ui.ShowSettingsDialog(tmp, cfg)
		tmp.Dispose()
		if newCfg == nil {
			removeTray()
			return
		}
		cfg = newCfg
		app.Cfg = cfg
	}

	// pprof
	var pprofLn net.Listener
	if cfg.PprofAddr != "" {
		pprofLn, err = net.Listen("tcp", cfg.PprofAddr)
		if err != nil {
			log.Fatalf("pprof listen: %v", err)
		}
		go func() {
			log.Printf("pprof listening on %s", pprofLn.Addr())
			if http.Serve(pprofLn, nil) != nil {
				log.Printf("pprof server stopped")
			}
		}()
	}

	app.Connect(cfg)
	log.Printf("GameTunnel Client %s (commit: %s, built: %s)", Version, Commit, BuildTime)
	hideConsole()

	// Message loop
	type winMSG struct {
		HWnd    uintptr
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      struct{ X, Y int32 }
	}
	m := new(winMSG)
	log.Printf("[ui] message loop started")
	for {
		ret, _, _ := pGetMsg.Call(uintptr(unsafe.Pointer(m)), 0, 0, 0)
		if ret == 0 {
			log.Printf("[ui] WM_QUIT received, exiting")
			break
		}
		if ret == ^uintptr(0) {
			log.Printf("[ui] GetMessage error, exiting")
			break
		}
		pTranslate.Call(uintptr(unsafe.Pointer(m)))
		pDispatch.Call(uintptr(unsafe.Pointer(m)))
	}

	if pprofLn != nil {
		pprofLn.Close()
	}
	removeTray()
	app.Disconnect()
	fmt.Println("Disconnected.")
}

// --- Message-only window ---

func createMsgWindow() uintptr {
	cn, _ := syscall.UTF16PtrFromString("GameTunnelTray")
	hInst, _, _ := pGetModHandle.Call(0)

	wp := syscall.NewCallback(func(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
		switch msg {
		case trayCBMsg:
			if lp == 0x0204 { // WM_RBUTTONUP
				showMenu()
			} else if lp == 0x0205 { // WM_LBUTTONDBLCLK
				toggleWin()
			}
			return 0
		case 0x0011: // WM_COMMAND
			switch wp & 0xFFFF {
			case 1: toggleWin()
			case 2: app.Connect(app.Cfg)
			case 3: app.Disconnect()
			case 4: openSettings()
			case 5: pPostQuit.Call(0)
			}
			return 0
		}
		r, _, _ := pDefWndProc.Call(hwnd, uintptr(msg), wp, lp)
		return r
	})

	var wc struct {
		Size, Style, WndProc             uintptr
		ClsExtra, WndExtra               int32
		Instance, Icon, Cursor, Brush     uintptr
		MenuName, ClassName, IconSm       uintptr
	}
	wc.Size = unsafe.Sizeof(wc)
	wc.WndProc = wp
	wc.Instance = hInst
	wc.ClassName = uintptr(unsafe.Pointer(cn))
	pRegisterClass.Call(uintptr(unsafe.Pointer(&wc)))

	t, _ := syscall.UTF16PtrFromString("GameTunnel")
	h, _, _ := pCreateWindow.Call(
		0, uintptr(unsafe.Pointer(cn)), uintptr(unsafe.Pointer(t)),
		0, 0, 0, 0, 0, 3, 0, 0, hInst)
	pShowWin.Call(h, 0) // SW_HIDE
	return h
}

// --- Walk main window (on-demand) ---

var theMW *walk.MainWindow

func toggleWin() {
	if theMW != nil && !theMW.IsDisposed() {
		if mwOpen {
			theMW.Hide()
			mwOpen = false
		} else {
			theMW.Show()
			theMW.SetFocus()
			mwOpen = true
		}
		return
	}
	mw, err := walk.NewMainWindow()
	if err != nil {
		log.Printf("create window: %v", err)
		return
	}
	mw.SetTitle("GameTunnel")
	mw.SetSize(walk.Size{Width: 400, Height: 300})
	mw.Closing().Attach(func(c *bool, r walk.CloseReason) {
		*c = true
		mw.Hide()
		mwOpen = false
	})
	theMW = mw
	mwOpen = true
}

func tmpForm() *walk.MainWindow {
	f, _ := walk.NewMainWindow()
	f.SetBounds(walk.Rectangle{X: -32000, Y: -32000, Width: 1, Height: 1})
	return f
}

func openSettings() {
	f := tmpForm()
	defer f.Dispose()
	cfg := ui.ShowSettingsDialog(f, app.Cfg)
	if cfg != nil {
		app.Connect(cfg)
	}
}

// --- Tray icon (Win32 Shell_NotifyIcon) ---

func addTray(hIcon uintptr, tip string) {
	var nid nidW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = msgHwnd
	nid.uID = 1
	nid.uFlags = 0x01 | 0x02 | 0x04 // NIF_MESSAGE | NIF_ICON | NIF_TIP
	nid.uCallbackMessage = trayCBMsg
	nid.hIcon = hIcon
	tipPtr, _ := syscall.UTF16PtrFromString(tip)
	copy(nid.szTip[:], (*[128]uint16)(unsafe.Pointer(tipPtr))[:])

	log.Printf("[ui] adding tray icon, nid size=%d", unsafe.Sizeof(nid))
	ret, _, err := pShellNotify.Call(0, uintptr(unsafe.Pointer(&nid))) // NIM_ADD
	log.Printf("[ui] Shell_NotifyIconW NIM_ADD: ret=%d err=%v", ret, err)
}

func removeTray() {
	var nid nidW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = msgHwnd
	nid.uID = 1
	pShellNotify.Call(2, uintptr(unsafe.Pointer(&nid))) // NIM_DELETE
}

func updateTray(hIcon uintptr, tip string) {
	var nid nidW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = msgHwnd
	nid.uID = 1
	nid.uFlags = 0x02 | 0x04 // NIF_ICON | NIF_TIP
	nid.hIcon = hIcon
	tipPtr, _ := syscall.UTF16PtrFromString(tip)
	copy(nid.szTip[:], (*[128]uint16)(unsafe.Pointer(tipPtr))[:])
	pShellNotify.Call(1, uintptr(unsafe.Pointer(&nid))) // NIM_MODIFY
}

func showMenu() {
	hMenu, _, _ := pCreateMenu.Call(0)
	t := i18n.T()

	add := func(text string, id uintptr, enabled bool) {
		p, _ := syscall.UTF16PtrFromString(text)
		f := uintptr(0x0000)
		if !enabled {
			f = 0x0001 // MF_GRAYED
		}
		pAppendMenu.Call(hMenu, f, id, uintptr(unsafe.Pointer(p)))
	}
	sep := func() { pAppendMenu.Call(hMenu, 0x0800, 0, 0) }

	add(t.DlgStatusIdle, 0, false)
	sep()
	add("Open Window", 1, true)
	add(t.DlgConnect, 2, app != nil && !app.Connected)
	add(t.DlgDisconnect, 3, app != nil && app.Connected)
	sep()
	add(t.DlgSettings, 4, true)
	sep()
	add("Exit", 5, true)

	var pt struct{ X, Y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForeground.Call(msgHwnd)

	// TPM_RETURNCMD (0x0100) | TPM_NONOTIFY (0x0080)
	cmd, _, _ := pTrackMenu.Call(hMenu, 0x0180,
		uintptr(pt.X), uintptr(pt.Y), 0, msgHwnd, 0)
	pSendMsg := u32.NewProc("SendMessageW")
	pSendMsg.Call(msgHwnd, 0x0122, 0, 0)

	if cmd != 0 {
		switch cmd {
		case 1: toggleWin()
		case 2: app.Connect(app.Cfg)
		case 3: app.Disconnect()
		case 4: openSettings()
		case 5: pPostQuit.Call(0)
		}
	}
	pDestroyMenu.Call(hMenu)
}
