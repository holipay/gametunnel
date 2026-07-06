//go:build windows

package ui

import (
	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/holipay/gametunnel/internal/client"
	"github.com/holipay/gametunnel/internal/i18n"
)

// ShowSettingsDialog shows the settings dialog and returns a new Config if OK was clicked.
// Returns nil if the user cancelled.
func ShowSettingsDialog(owner walk.Form, cfg *client.Config) *client.Config {
	var serverAddr, playerName, roomID *walk.TextEdit
	var password *walk.LineEdit

	t := i18n.T()
	newCfg := *cfg // copy

	dlg := new(walk.Dialog)

	_, err := Dialog{
		AssignTo: &dlg,
		Title:    t.DlgTitle,
		MinSize:  Size{Width: 350, Height: 250},
		Layout:   VBox{Margins: Margins{Left: 15, Top: 15, Right: 15, Bottom: 15}, Spacing: 10},
		Children: []Widget{
			GroupBox{
				Title:  t.DlgServerAddr[:len(t.DlgServerAddr)-1], // remove trailing ":"
				Layout: Grid{Columns: 2, Spacing: 6, Margins: Margins{Left: 10, Top: 10, Right: 10, Bottom: 10}},
				Children: []Widget{
					Label{Text: t.DlgServerAddr + " "},
					TextEdit{AssignTo: &serverAddr, Text: cfg.ServerAddr},

					Label{Text: t.DlgPlayerName + " "},
					TextEdit{AssignTo: &playerName, Text: cfg.PlayerName},

					Label{Text: t.DlgRoomID + " "},
					TextEdit{AssignTo: &roomID, Text: cfg.RoomID},

					Label{Text: t.DlgPassword + " "},
					LineEdit{AssignTo: &password, Text: cfg.RoomPassword, PasswordMode: true},
				},
			},
			Composite{
				Layout: HBox{Spacing: 6},
				Children: []Widget{
					HSpacer{},
					PushButton{
						Text: t.DlgOK,
						OnClicked: func() {
							newCfg.ServerAddr = serverAddr.Text()
							newCfg.PlayerName = playerName.Text()
							newCfg.RoomID = roomID.Text()
							newCfg.RoomPassword = password.Text()

							if err := newCfg.Validate(); err != nil {
								walk.MsgBox(dlg, t.DlgTitle, err.Error(),
									walk.MsgBoxIconError|walk.MsgBoxOK)
								return
							}

							dlg.Accept()
						},
					},
					PushButton{
						Text: t.DlgCancel,
						OnClicked: func() {
							dlg.Cancel()
						},
					},
				},
			},
		},
	}.Run(owner)

	if err != nil {
		return nil
	}

	return &newCfg
}
