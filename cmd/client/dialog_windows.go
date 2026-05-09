//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"log"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/holipay/gametunnel/internal/client"
)

var (
	user32 = syscall.NewLazyDLL("user32.dll")

	procDialogBoxIndirectParam = user32.NewProc("DialogBoxIndirectParamW")
	procGetDlgItemText         = user32.NewProc("GetDlgItemTextW")
	procSetDlgItemText         = user32.NewProc("SetDlgItemTextW")
	procEndDialog              = user32.NewProc("EndDialog")
	procGetDlgItem             = user32.NewProc("GetDlgItem")
	procSendMessage            = user32.NewProc("SendMessageW")
	procSetFocus               = user32.NewProc("SetFocus")
)

const (
	IDC_SERVER       = 1001
	IDC_NAME         = 1002
	IDC_ROOM         = 1003
	IDC_PASSWORD     = 1004
	IDC_STATUS_LABEL = 1005

	WM_INITDIALOG  = 0x0110
	WM_COMMAND      = 0x0111
	WM_CLOSE        = 0x0010
	WM_SETFONT      = 0x0030

	WS_POPUP        = 0x80000000
	WS_CAPTION      = 0x00C00000
	WS_SYSMENU      = 0x00080000
	WS_VISIBLE      = 0x10000000
	WS_CHILD        = 0x40000000
	WS_TABSTOP      = 0x00010000
	WS_GROUP        = 0x00020000
	WS_BORDER       = 0x00800000
	DS_MODALFRAME   = 0x0080
	DS_SETFONT      = 0x0040
	DS_CENTER       = 0x0800
	ES_AUTOHSCROLL  = 0x0080
	ES_PASSWORD     = 0x0020
	SS_LEFT         = 0x00000000
	BS_PUSHBUTTON   = 0x00000000
	BS_DEFPUSHBUTTON = 0x00000001

	IDOK     = 1
	IDCANCEL = 2
)

var hFont uintptr

// showConfigDialog shows the Win32 native config dialog.
// Returns true if user clicked OK.
func showConfigDialog(statusText string) bool {
	cfg := client.LoadConfig()
	tmpl := buildDialogTemplate(statusText)

	ret, _, _ := procDialogBoxIndirectParam.Call(
		0,
		uintptr(unsafe.Pointer(&tmpl[0])),
		0,
		syscall.NewCallback(configDialogProc(cfg)),
	)
	if ret == 0 {
		log.Printf("[dialog] DialogBoxIndirectParam 返回 0（创建失败或用户取消）")
	}
	return ret == 1
}

func configDialogProc(cfg *client.Config) func(uintptr, uint32, uintptr, uintptr) uintptr {
	return func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
		switch msg {
		case WM_INITDIALOG:
			if hFont != 0 {
				for _, id := range []int{IDC_SERVER, IDC_NAME, IDC_ROOM, IDC_PASSWORD, IDC_STATUS_LABEL, IDOK, IDCANCEL} {
					hctl, _, _ := procGetDlgItem.Call(hwnd, uintptr(id))
					if hctl != 0 {
						procSendMessage.Call(hctl, WM_SETFONT, hFont, 1)
					}
				}
			}
			setEditText(hwnd, IDC_SERVER, cfg.ServerAddr)
			setEditText(hwnd, IDC_NAME, cfg.PlayerName)
			setEditText(hwnd, IDC_ROOM, cfg.RoomID)
			setEditText(hwnd, IDC_PASSWORD, cfg.RoomPassword)
			hctl, _, _ := procGetDlgItem.Call(hwnd, uintptr(IDC_SERVER))
			procSetFocus.Call(hctl)
			return 1

		case WM_COMMAND:
			switch wParam & 0xFFFF {
			case IDOK:
				cfg.ServerAddr = getEditText(hwnd, IDC_SERVER)
				cfg.PlayerName = getEditText(hwnd, IDC_NAME)
				cfg.RoomID = getEditText(hwnd, IDC_ROOM)
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

func getEditText(hwnd uintptr, id int) string {
	buf := make([]uint16, 512)
	procGetDlgItemText.Call(hwnd, uintptr(id), uintptr(unsafe.Pointer(&buf[0])), 512)
	return windows.UTF16ToString(buf)
}

func setEditText(hwnd uintptr, id int, text string) {
	p, _ := windows.UTF16PtrFromString(text)
	procSetDlgItemText.Call(hwnd, uintptr(id), uintptr(unsafe.Pointer(p)))
}

// buildDialogTemplate creates a Win32 DLGTEMPLATE + DLGITEMTEMPLATE byte buffer.
//
// Layout (260×140 DLU):
//   服务器地址: [____________________________]
//   玩家名称:   [____________________________]
//   房间 ID:    [____________________________]
//   密码:       [____________________________]
//   [状态文字                                    ]
//           [ 确定 ]      [ 取消 ]
func buildDialogTemplate(statusText string) []byte {
	var buf bytes.Buffer

	// ── DLGTEMPLATE header ──
	writeInt32(&buf, uint32(DS_MODALFRAME|DS_SETFONT|WS_POPUP|WS_CAPTION|WS_SYSMENU|DS_CENTER))
	writeInt32(&buf, 0)   // dwExtendedStyle
	writeInt16(&buf, 11)  // cdit: 4 labels + 4 edits + 1 status = 9? No: 4 labels + 4 edits + 1 status label + 2 buttons = 11. Actually: 4*2 + 1 + 2 = 11
	writeInt16(&buf, 50)  // x
	writeInt16(&buf, 30)  // y
	writeInt16(&buf, 260) // cx
	writeInt16(&buf, 180) // cy
	buf.Write([]byte{0, 0}) // menu: none
	buf.Write([]byte{0, 0}) // class: none
	writeUTF16(&buf, "GameTunnel 设置")
	writeInt16(&buf, 9)   // font size
	writeUTF16(&buf, "Microsoft YaHei UI")

	// ── Row 1: 服务器地址 ──
	addStatic(&buf, 7, 15, 50, 10, 0, "服务器地址:")
	addEdit(&buf, 62, 13, 188, 14, IDC_SERVER, false)

	// ── Row 2: 玩家名称 ──
	addStatic(&buf, 7, 35, 50, 10, 0, "玩家名称:")
	addEdit(&buf, 62, 33, 188, 14, IDC_NAME, false)

	// ── Row 3: 房间 ID ──
	addStatic(&buf, 7, 55, 50, 10, 0, "房间 ID:")
	addEdit(&buf, 62, 53, 188, 14, IDC_ROOM, false)

	// ── Row 4: 密码 ──
	addStatic(&buf, 7, 75, 50, 10, 0, "密码:")
	addEdit(&buf, 62, 73, 188, 14, IDC_PASSWORD, true)

	// ── Status label ──
	addStatic(&buf, 7, 97, 243, 12, IDC_STATUS_LABEL, statusText)

	// ── Buttons ──
	addButton(&buf, 85, 118, 40, 14, IDOK, "确定", true)
	addButton(&buf, 135, 118, 40, 14, IDCANCEL, "取消", false)

	return buf.Bytes()
}

func addStatic(buf *bytes.Buffer, x, y, cx, cy int16, id uint16, text string) {
	addItem(buf, x, y, cx, cy, id, 0x82, SS_LEFT, text)
}

func addEdit(buf *bytes.Buffer, x, y, cx, cy int16, id uint16, password bool) {
	style := uint32(WS_CHILD|WS_VISIBLE|WS_TABSTOP|WS_BORDER|ES_AUTOHSCROLL)
	if password {
		style |= ES_PASSWORD
	}
	addItem(buf, x, y, cx, cy, id, 0x81, style, "")
}

func addButton(buf *bytes.Buffer, x, y, cx, cy int16, id uint16, text string, isDefault bool) {
	style := uint32(WS_CHILD|WS_VISIBLE|WS_TABSTOP|BS_PUSHBUTTON)
	if isDefault {
		style |= BS_DEFPUSHBUTTON
	}
	addItem(buf, x, y, cx, cy, id, 0x80, style, text)
}

// addItem writes a DLGITEMTEMPLATE entry.
// classAtom: 0x80=BUTTON, 0x81=EDIT, 0x82=STATIC
func addItem(buf *bytes.Buffer, x, y, cx, cy int16, id uint16, classAtom uint16, style uint32, text string) {
	// Align to DWORD boundary
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}

	writeInt32(buf, 0)              // dwExtendedStyle
	writeInt32(buf, style)          // style
	writeInt16(buf, x)
	writeInt16(buf, y)
	writeInt16(buf, cx)
	writeInt16(buf, cy)
	writeInt16(buf, int16(id))
	buf.Write([]byte{0xFF, 0xFF})    // class: predefined
	writeInt16(buf, int16(classAtom))
	writeUTF16(buf, text)
	writeInt16(buf, 0)              // cbData (no extra creation data)
}

func writeInt16(buf *bytes.Buffer, v int16) {
	binary.Write(buf, binary.LittleEndian, v)
}

func writeInt32(buf *bytes.Buffer, v uint32) {
	binary.Write(buf, binary.LittleEndian, v)
}

func writeUTF16(buf *bytes.Buffer, s string) {
	for _, c := range syscall.StringToUTF16(s) {
		binary.Write(buf, binary.LittleEndian, c)
	}
}
