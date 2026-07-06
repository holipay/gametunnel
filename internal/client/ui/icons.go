//go:build windows

package ui

// Icon loading is handled directly via walk stock icons:
//   - walk.IconApplication() — default disconnected icon
//   - walk.IconInformation() — connected icon
//
// No custom icon loading needed for the initial implementation.
