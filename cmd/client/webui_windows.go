//go:build windows

package main

import "golang.org/x/sys/windows"

func openBrowser(url string) {
	windows.ShellExecute(0, nil, windows.StringToUTF16Ptr(url), nil, nil, windows.SW_SHOWNORMAL)
}
