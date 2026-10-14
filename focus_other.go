//go:build !windows

package main

// focusMainWindow：macOS / Linux 上 WindowShow 已经会把窗口带到前面。
func focusMainWindow() {}
