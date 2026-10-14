//go:build windows

package main

var procSetForegroundWindow = user32.NewProc("SetForegroundWindow")

// focusMainWindow 把本进程的主窗口切到前台。托盘单击 / 菜单回调就在用户刚刚的那
// 次输入里，系统允许本进程抢前台（否则 SetForegroundWindow 只会让任务栏按钮闪）。
func focusMainWindow() {
	if hwnd := findMainWindow(); hwnd != 0 {
		procSetForegroundWindow.Call(hwnd)
	}
}
