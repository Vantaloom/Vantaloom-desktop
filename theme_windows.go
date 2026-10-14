//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	modAdvapi32     = syscall.NewLazyDLL("advapi32.dll")
	procRegGetValue = modAdvapi32.NewProc("RegGetValueW")
)

const (
	hkeyCurrentUser = 0x80000001
	rrfRtRegDword   = 0x00000010
)

// systemPrefersLight 读 Windows 的「应用模式」（设置 → 个性化 → 颜色）：
// HKCU\...\Themes\Personalize 的 AppsUseLightTheme，1 = 亮。读不到按暗处理。
func systemPrefersLight() bool {
	key, _ := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`)
	name, _ := syscall.UTF16PtrFromString("AppsUseLightTheme")
	var value uint32
	size := uint32(unsafe.Sizeof(value))
	r, _, _ := procRegGetValue.Call(
		hkeyCurrentUser,
		uintptr(unsafe.Pointer(key)),
		uintptr(unsafe.Pointer(name)),
		rrfRtRegDword,
		0,
		uintptr(unsafe.Pointer(&value)),
		uintptr(unsafe.Pointer(&size)),
	)
	return r == 0 && value == 1
}
