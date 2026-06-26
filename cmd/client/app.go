package main

// App and related types are now in internal/client/app.go.
// This file re-exports them for backward compatibility with tray.go.
//
// All platform-independent business logic (connection management, status
// tracking, auto-reconnect) lives in internal/client/ so it can be reused
// by future macOS/Linux clients.

import "github.com/holipay/gametunnel/internal/client"

// App is re-exported from internal/client.
type App = client.App

// StatusResponse is re-exported from internal/client.
type StatusResponse = client.StatusResponse

// NewApp creates a new App. Delegates to internal/client.NewApp.
func NewApp(cfg *client.Config) *App {
	return client.NewApp(cfg)
}
