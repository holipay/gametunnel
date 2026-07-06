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
	pSendMsg       = u32.NewProc("SendMessageW")
)

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

	// Message-only window for tray
	msgHwnd = createMsgWindow()

	// Tray icon
	hIcon, _, _ := pLoadIcon.Call(0, 32512)
	addTray(hIcon, "GameTunnel - 未连接")

	// First-run settings dialog (uses walk)
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
	type msgT struct {
		HWnd    uintptr
		Message uint32
		WParam  uintptr
		LParam  uintptr
		Time    uint32
		Pt      struct{ X, Y int32 }
	}
	m := (*msgT)(unsafe.Pointer(func() uintptr {
		r, _, _ := k32.NewProc("LocalAlloc").Call(0x0040, unsafe.Sizeof(msgT{}))
		return r
	}()))
	defer func() { k32.NewProc("LocalFree").Call(uintptr(unsafe.Pointer(m))) }()

	log.Printf("[ui] message loop started")
	for {
		ret, _, _ := pGetMsg.Call(uintptr(unsafe.Pointer(m)), 0, 0, 0)
		if ret == 0 {
			log.Printf("[ui] message loop exiting: WM_QUIT received")
			break
		}
		if ret == ^uintptr(0) {
			log.Printf("[ui] message loop exiting: GetMessage error")
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
	wp := syscall.NewCallback(trayProc)

	var wc struct {
		Size, Style, WndProc uintptr
		ClsExtra, WndExtra int32
		Instance, Icon, Cursor, Brush uintptr
		MenuName, ClassName, IconSm uintptr
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
	pShowWin.Call(h, 0)
	return h
}

func trayProc(hwnd uintptr, msg uint32, wp, lp uintptr) uintptr {
	switch msg {
	case trayCBMsg:
		if lp == 0x0204 {
			showMenu()
			return 0
		}
		if lp == 0x0205 {
			toggleWin()
			return 0
		}
	case 0x0011:
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

// --- Raw Win32 tray icon ---

func addTray(hIcon uintptr, tip string) {
	nid := make([]byte, 264)
	*(*uint32)(unsafe.Pointer(&nid[0])) = 264
	*(*uintptr)(unsafe.Pointer(&nid[4])) = msgHwnd
	*(*uint32)(unsafe.Pointer(&nid[12])) = 1
	*(*uint32)(unsafe.Pointer(&nid[16])) = 0x07
	*(*uint32)(unsafe.Pointer(&nid[20])) = trayCBMsg
	*(*uintptr)(unsafe.Pointer(&nid[24])) = hIcon
	p, _ := syscall.UTF16PtrFromString(tip)
	for i := 0; i < 128; i++ {
		ch := *(*uint16)(unsafe.Pointer(uintptr(unsafe.Pointer(p)) + uintptr(i*2)))
		if ch == 0 {
			break
		}
		*(*uint16)(unsafe.Pointer(&nid[28+i*2])) = ch
	}
	pShellNotify.Call(0, uintptr(unsafe.Pointer(&nid[0])))
}

func removeTray() {
	nid := make([]byte, 264)
	*(*uint32)(unsafe.Pointer(&nid[0])) = 264
	*(*uintptr)(unsafe.Pointer(&nid[4])) = msgHwnd
	*(*uint32)(unsafe.Pointer(&nid[12])) = 1
	pShellNotify.Call(2, uintptr(unsafe.Pointer(&nid[0])))
}

func showMenu() {
	hMenu, _, _ := pCreateMenu.Call(0)
	t := i18n.T()

	add := func(text string, id uintptr, enabled bool) {
		p, _ := syscall.UTF16PtrFromString(text)
		f := uintptr(0x0000) // MF_STRING
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
	// Returns the selected command ID instead of sending WM_COMMAND
	cmd, _, _ := pTrackMenu.Call(hMenu, 0x0180,
		uintptr(pt.X), uintptr(pt.Y),
		0, msgHwnd, 0)
	pSendMsg.Call(msgHwnd, 0x0122, 0, 0) // dismiss menu highlight

	if cmd != 0 {
		// Process the command directly
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
