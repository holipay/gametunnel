//go:build windows

package main

import (
	"log"
	"net"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
)

var (

	procCreateWindowEx         = user32.NewProc("CreateWindowExW")
	procGetDlgItemText         = user32.NewProc("GetDlgItemTextW")
	procSetDlgItemText         = user32.NewProc("SetDlgItemTextW")
	procGetWindowTextLength    = user32.NewProc("GetWindowTextLengthW")
	procSendMessage            = user32.NewProc("SendMessageW")
	procSetFocus               = user32.NewProc("SetFocus")
	procDialogBoxIndirectParam = user32.NewProc("DialogBoxIndirectParamW")
	procEndDialog              = user32.NewProc("EndDialog")
	procGetDlgItem             = user32.NewProc("GetDlgItem")
	procGetDC                  = user32.NewProc("GetDC")
	procReleaseDC              = user32.NewProc("ReleaseDC")

	gdi32              = syscall.NewLazyDLL("gdi32.dll")
	procCreateFont     = gdi32.NewProc("CreateFontW")
	procGetTextMetrics = gdi32.NewProc("GetTextMetricsW")
	procDeleteObject   = gdi32.NewProc("DeleteObject")
	procSelectObject   = gdi32.NewProc("SelectObject")
)

const (
	IDC_SERVER       = 1001
	IDC_NAME         = 1002
	IDC_ROOM         = 1003
	IDC_PASSWORD     = 1004
	IDC_STATUS_LABEL = 1005
	IDC_SHOW_PASS    = 1006

	WM_INITDIALOG = 0x0110
	WM_COMMAND    = 0x0111
	WM_CLOSE      = 0x0010
	WM_SETFONT    = 0x0030

	WS_POPUP         = 0x80000000
	WS_CAPTION       = 0x00C00000
	WS_SYSMENU       = 0x00080000
	WS_VISIBLE       = 0x10000000
	WS_CHILD         = 0x40000000
	WS_TABSTOP       = 0x00010000
	WS_BORDER        = 0x00800000

	DS_MODALFRAME = 0x0080
	DS_SHELLFONT  = 0x0048
	DS_CENTER     = 0x0800

	ES_AUTOHSCROLL = 0x0080
	ES_PASSWORD    = 0x0020
	BS_PUSHBUTTON   = 0x00000000
	BS_DEFPUSHBUTTON = 0x00000001
	BS_AUTOCHECKBOX  = 0x00000003
	IDOK     = 1
	IDCANCEL = 2
)

// textMetricW maps to the Win32 TEXTMETRICW structure.
type textMetricW struct {
	TmHeight           int32
	TmAscent           int32
	TmDescent          int32
	TmInternalLeading  int32
	TmExternalLeading  int32
	TmAveCharWidth     int32
	TmMaxCharWidth     int32
	TmWeight           int32
	TmOverhang         int32
	TmDigitizedAspectX int32
	TmDigitizedAspectY int32
	TmFirstChar        uint16
	TmLastChar         uint16
	TmDefaultChar      uint16
	TmBreakChar        uint16
	TmItalic           byte
	TmUnderlined       byte
	TmStruckOut        byte
	TmPitchAndFamily   byte
	TmCharSet          byte
}

// Package-level DLU→pixel converters, set before dialog creation.
var (
	dluToPixelX = func(dlu int) int { return dlu }
	dluToPixelY = func(dlu int) int { return dlu }
)

// showConfigDialog shows a Win32 native settings dialog.
// Returns true if the user clicked OK.
func showConfigDialog(statusText string) bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cfg := client.LoadConfig()

	// Create the dialog font: 9pt Segoe UI (standard Windows 10+ UI font).
	hFont, _, _ := procCreateFont.Call(
		uintptr(15), // lfHeight ≈ 9pt at 96 DPI
		0, 0, 0,
		400, // FW_NORMAL
		0, 0, 0,
		1, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("Segoe UI"))),
	)

	// Measure the font to compute dialog base units (1 DLU = 1/4 avgCharWidth × 1/8 height).
	var tm textMetricW
	dc, _, _ := procGetDC.Call(0)
	oldObj, _, _ := procSelectObject.Call(dc, hFont)
	procGetTextMetrics.Call(dc, uintptr(unsafe.Pointer(&tm)))
	procSelectObject.Call(dc, oldObj)
	procReleaseDC.Call(0, dc)

	baseX := int(tm.TmAveCharWidth)
	baseY := int(tm.TmHeight)
	dluToPixelX = func(dlu int) int { return (dlu*baseX + 2) / 4 }
	dluToPixelY = func(dlu int) int { return (dlu*baseY + 4) / 8 }

	// Build a minimal DLGTEMPLATE with zero controls.
	// All child controls are created via CreateWindowEx in WM_INITDIALOG,
	// which avoids all template-parsing compatibility issues.
	dlgTitle := i18n.T().DlgTitle
	if cfg.ServerAddr == "" {
		dlgTitle = i18n.T().DlgFirstRun
	}
	tmpl := buildDialogTemplate(260, 180, dlgTitle)
	defer runtime.KeepAlive(tmpl)

	ret, _, err := procDialogBoxIndirectParam.Call(
		0,
		uintptr(unsafe.Pointer(&tmpl[0])),
		0,
		syscall.NewCallback(configDialogProc(cfg, hFont, statusText)),
	)
	if hFont != 0 {
		procDeleteObject.Call(hFont)
	}

	log.Printf("[dialog] DialogBoxIndirectParam returned %d (err=%v)", ret, err)
	return ret == 1
}

func configDialogProc(cfg *client.Config, hFont uintptr, statusText string) func(uintptr, uint32, uintptr, uintptr) uintptr {
	return func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
		switch msg {
		case WM_INITDIALOG:
			log.Printf("[dialog] WM_INITDIALOG hwnd=%x", hwnd)
			s := i18n.T()

			// ── Layout (all values in DLU) ──
			margin := 7
			labelW := 50
			editX := margin + labelW + 5
			editW := 260 - editX - margin
			rowH := 14
			gap := 8

			// Row 1: Server address
			y1 := 15
			makeCtl("STATIC", s.DlgServerAddr, 0, margin, y1, labelW, rowH, hwnd, 0)
			makeCtl("EDIT", "", WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, editX, y1, editW, rowH, hwnd, IDC_SERVER)

			// Row 2: Player name
			y2 := y1 + rowH + gap
			makeCtl("STATIC", s.DlgPlayerName, 0, margin, y2, labelW, rowH, hwnd, 0)
			makeCtl("EDIT", "", WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, editX, y2, editW, rowH, hwnd, IDC_NAME)

			// Row 3: Room ID
			y3 := y2 + rowH + gap
			makeCtl("STATIC", s.DlgRoomID, 0, margin, y3, labelW, rowH, hwnd, 0)
			makeCtl("EDIT", "", WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL, editX, y3, editW, rowH, hwnd, IDC_ROOM)

			// Row 4: Password
			y4 := y3 + rowH + gap
			makeCtl("STATIC", s.DlgPassword, 0, margin, y4, labelW, rowH, hwnd, 0)
			makeCtl("EDIT", "", WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL|ES_PASSWORD, editX, y4, editW-50, rowH, hwnd, IDC_PASSWORD)

			// Show password checkbox (next to password field)
			makeCtl("BUTTON", s.DlgShowPass, BS_AUTOCHECKBOX|WS_TABSTOP, editX+editW-48, y4, 48, rowH, hwnd, IDC_SHOW_PASS)

			// Status label
			yStatus := y4 + rowH + gap + 4
			makeCtl("STATIC", statusText, 0, margin, yStatus, 260-2*margin, 12, hwnd, IDC_STATUS_LABEL)

			// Buttons — use "Connect" text for first run, "OK" otherwise
			btnY := yStatus + 12 + gap + 6
			btnW := 40
			btnGap := 10
			btnX := (260 - btnW*2 - btnGap) / 2
			okText := s.DlgOK
			if cfg.ServerAddr == "" {
				okText = s.DlgConnect
			}
			makeCtl("BUTTON", okText, BS_DEFPUSHBUTTON|WS_TABSTOP, btnX, btnY, btnW, 14, hwnd, IDOK)
			makeCtl("BUTTON", s.DlgCancel, BS_PUSHBUTTON|WS_TABSTOP, btnX+btnW+btnGap, btnY, btnW, 14, hwnd, IDCANCEL)

			// Apply font to every control.
			if hFont != 0 {
				for _, id := range []uintptr{IDC_SERVER, IDC_NAME, IDC_ROOM, IDC_PASSWORD, IDC_SHOW_PASS, IDC_STATUS_LABEL, IDOK, IDCANCEL} {
					hctl, _, _ := procGetDlgItem.Call(hwnd, id)
					if hctl != 0 {
						procSendMessage.Call(hctl, WM_SETFONT, hFont, 1)
					}
				}
			}

			// Populate fields from config.
			setEditText(hwnd, IDC_SERVER, cfg.ServerAddr)
			setEditText(hwnd, IDC_NAME, cfg.PlayerName)
			setEditText(hwnd, IDC_ROOM, cfg.RoomID)
			setEditText(hwnd, IDC_PASSWORD, cfg.RoomPassword)

			// Focus the first field.
			hctl, _, _ := procGetDlgItem.Call(hwnd, uintptr(IDC_SERVER))
			procSetFocus.Call(hctl)
			return 1

		case WM_COMMAND:
			switch wParam & 0xFFFF {
			case IDC_SHOW_PASS:
				// Toggle password visibility
				hPass, _, _ := procGetDlgItem.Call(hwnd, IDC_PASSWORD)
				if hPass != 0 {
					style, _, _ := procSendMessage.Call(hPass, 0x00F0 /* EM_GETPASSWORDCHAR */, 0, 0) // EM_GETSEL workaround
					// Check checkbox state
					hCheck, _, _ := procGetDlgItem.Call(hwnd, IDC_SHOW_PASS)
					checked, _, _ := procSendMessage.Call(hCheck, 0x00F0 /* BM_GETCHECK */, 0, 0)
					if checked == 1 {
						// Checked → show password (remove ES_PASSWORD by setting password char to 0)
						procSendMessage.Call(hPass, 0x00CC /* EM_SETPASSWORDCHAR */, 0, 0)
					} else {
						// Unchecked → hide password (set password char back to bullet)
						procSendMessage.Call(hPass, 0x00CC /* EM_SETPASSWORDCHAR */, uintptr(0x2022), 0)
					}
					// Redraw the edit control
					procSendMessage.Call(hPass, 0x000F /* WM_PAINT */, 0, 0)
					_ = style
				}
				return 1

			case IDOK:
				// ── Input validation ──
				serverAddr := getEditText(hwnd, IDC_SERVER)
				playerName := getEditText(hwnd, IDC_NAME)
				roomID := getEditText(hwnd, IDC_ROOM)

				if serverAddr == "" || (!containsPort(serverAddr) && net.ParseIP(serverAddr) == nil) {
					// Show validation error in status label
					setStatusText(hwnd, IDC_STATUS_LABEL, i18n.T().DlgInvalidAddr)
					// Focus server field
					hctl, _, _ := procGetDlgItem.Call(hwnd, uintptr(IDC_SERVER))
					procSetFocus.Call(hctl)
					return 1
				}
				if playerName == "" {
					setStatusText(hwnd, IDC_STATUS_LABEL, "玩家名称不能为空")
					hctl, _, _ := procGetDlgItem.Call(hwnd, uintptr(IDC_NAME))
					procSetFocus.Call(hctl)
					return 1
				}
				if roomID == "" {
					roomID = "default"
					setEditText(hwnd, IDC_ROOM, roomID)
				}

				cfg.ServerAddr = serverAddr
				cfg.PlayerName = playerName
				cfg.RoomID = roomID
				cfg.RoomPassword = getEditText(hwnd, IDC_PASSWORD)
				if err := client.SaveConfig(cfg); err != nil {
					log.Printf("[dialog] save config: %v", err)
				}
				procEndDialog.Call(hwnd, 1)
				return 1
			case IDCANCEL:
				procEndDialog.Call(hwnd, 0)
				return 1
			}

		case WM_CLOSE:
			procEndDialog.Call(hwnd, 0)
			return 1
		}
		return 0
	}
}

// makeCtl creates a child control inside the dialog.
// Coordinates (x, y, w, h) are in DLU and converted to pixels automatically.
func makeCtl(className, text string, style uint32, x, y, w, h int, parent uintptr, id uintptr) {
	classPtr, _ := windows.UTF16PtrFromString(className)
	textPtr, _ := windows.UTF16PtrFromString(text)

	procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(classPtr)),
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(WS_CHILD|WS_VISIBLE|style),
		uintptr(dluToPixelX(x)),
		uintptr(dluToPixelY(y)),
		uintptr(dluToPixelX(w)),
		uintptr(dluToPixelY(h)),
		parent,
		id,
		0,
		0,
	)
}

func getEditText(hwnd uintptr, id int) string {
	hctl, _, _ := procGetDlgItem.Call(hwnd, uintptr(id))
	if hctl == 0 {
		return ""
	}
	length, _, _ := procGetWindowTextLength.Call(hctl)
	if length == 0 {
		return ""
	}
	buf := make([]uint16, length+1)
	procGetDlgItemText.Call(hwnd, uintptr(id), uintptr(unsafe.Pointer(&buf[0])), uintptr(length+1))
	return windows.UTF16ToString(buf)
}

func setEditText(hwnd uintptr, id int, text string) {
	p, _ := windows.UTF16PtrFromString(text)
	procSetDlgItemText.Call(hwnd, uintptr(id), uintptr(unsafe.Pointer(p)))
}

// buildDialogTemplate creates a minimal DLGTEMPLATE with zero child controls.
// The actual controls are created via CreateWindowEx in WM_INITDIALOG.
// This avoids all DLGITEMTEMPLATE alignment/encoding compatibility issues.
func buildDialogTemplate(dluW, dluH int, title string) []byte {
	var buf leBuffer

	// DLGTEMPLATE header — cdit=0, no item templates.
	buf.writeUint32(DS_SHELLFONT | DS_MODALFRAME | WS_POPUP | WS_CAPTION | WS_SYSMENU | DS_CENTER)
	buf.writeUint32(0)                // dwExtendedStyle
	buf.writeUint16(0)                // cdit = 0 (no controls in template)
	buf.writeInt16(0)                 // x
	buf.writeInt16(0)                 // y
	buf.writeInt16(int16(dluW))       // cx (width in DLU)
	buf.writeInt16(int16(dluH))       // cy (height in DLU)
	buf.writeUint16(0)                // menu: none
	buf.writeUint16(0)                // class: none
	buf.writeWStr(title)              // title
	buf.writeUint16(9)                // font size (pt)
	buf.writeWStr("MS Shell Dlg")     // font name

	return buf.bytes()
}

// ── leBuffer: little-endian byte buffer ──

type leBuffer struct {
	data []byte
}

func (b *leBuffer) writeUint16(v uint16) {
	b.data = append(b.data, byte(v), byte(v>>8))
}

func (b *leBuffer) writeInt16(v int16) {
	b.writeUint16(uint16(v))
}

func (b *leBuffer) writeUint32(v uint32) {
	b.data = append(b.data, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// writeWStr writes a null-terminated UTF-16LE string.
func (b *leBuffer) writeWStr(s string) {
	for _, c := range syscall.StringToUTF16(s) {
		b.writeUint16(c)
	}
}

func (b *leBuffer) bytes() []byte { return b.data }

// containsPort checks if an address string contains a port (has ':').
func containsPort(addr string) bool {
	return strings.Contains(addr, ":")
}

// setStatusText updates the text of a control by ID.
func setStatusText(hwnd uintptr, id uintptr, text string) {
	p, _ := windows.UTF16PtrFromString(text)
	procSetDlgItemText.Call(hwnd, id, uintptr(unsafe.Pointer(p)))
}

// showSettingsDialog is the platform-specific settings dialog.
// On Windows, it delegates to the Win32 native dialog.
func showSettingsDialog(statusText string) bool {
	return showConfigDialog(statusText)
}
