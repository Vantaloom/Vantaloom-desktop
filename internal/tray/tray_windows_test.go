//go:build windows

package tray

import (
	"os"
	"strconv"
	"testing"
	"time"
	"unsafe"
)

// NOTIFYICONDATAW 在 x64 / arm64 上是 976 字节。Go 结构体的对齐若跟 C 对不上，
// Shell_NotifyIcon 会读到错位的字段——要么静默失败，要么拿垃圾当图标句柄。
func TestNotifyIconDataSize(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("只钉 64 位布局")
	}
	if got := unsafe.Sizeof(notifyIconData{}); got != 976 {
		t.Fatalf("sizeof(NOTIFYICONDATAW) = %d, want 976", got)
	}
}

var (
	pGetGuiResources     = user32.NewProc("GetGuiResources")
	pGetCurrentProcess   = kernel32.NewProc("GetCurrentProcess")
	pSendMessageTimeoutW = user32.NewProc("SendMessageTimeoutW")
)

func guiResources() (gdi, usr uintptr) {
	proc, _, _ := pGetCurrentProcess.Call()
	gdi, _, _ = pGetGuiResources.Call(proc, 0)
	usr, _, _ = pGetGuiResources.Call(proc, 1)
	return
}

// 真托盘的压力测试（会在托盘区真的出现几秒图标，所以默认跳过）：
//
//	$env:VTL_TRAY_LIVE=1; go test ./internal/tray -run Live -v
//
// 钉住的是上次那场事故的反面：同一进程只有一个图标、改多少次都是 NIM_MODIFY、
// 「资源管理器重启」多少次都只把同一个图标加回去、关掉时删掉、句柄不涨。
func TestLiveTrayNeverDuplicates(t *testing.T) {
	if os.Getenv("VTL_TRAY_LIVE") != "1" {
		t.Skip("设 VTL_TRAY_LIVE=1 才跑（会在托盘区出现图标）")
	}
	gdi0, usr0 := guiResources()
	tr, err := Start(Options{
		Tooltip: "Vantaloom（托盘自测）",
		Menu:    func() []Item { return []Item{{ID: 1, Label: "测试"}} },
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := Start(Options{Tooltip: "第二个"}); err != ErrAlreadyRunning {
		t.Fatalf("第二次 Start 应返回 ErrAlreadyRunning，得到 %v", err)
	}
	if countAdd.Load() != 1 {
		t.Fatalf("NIM_ADD 次数 = %d，want 1", countAdd.Load())
	}

	// 500 次提示更新：只发 MODIFY，相同文字不发。
	for i := 0; i < 500; i++ {
		tr.SetTooltip("Vantaloom · 第 " + strconv.Itoa(i%50) + " 轮")
	}
	tr.SetTooltip("Vantaloom · 第 49 轮") // 与最后一次相同：不该再发
	waitIdle(tr.impl.hwnd)
	if countAdd.Load() != 1 {
		t.Fatalf("改提示之后 NIM_ADD = %d，want 1（改提示绝不能再加图标）", countAdd.Load())
	}
	if countModify.Load() == 0 {
		t.Fatalf("改提示一次 NIM_MODIFY 都没发")
	}

	// 「资源管理器重启」20 次：图标还在（没真重启），同一 hWnd+uID 的 NIM_ADD 会被
	// 系统拒绝——成功的 ADD 仍然只能是 1 次。
	for i := 0; i < 20; i++ {
		pPostMessageW.Call(tr.impl.hwnd, uintptr(tr.impl.taskbarCreated), 0, 0)
	}
	waitIdle(tr.impl.hwnd)
	if countAdd.Load() != 1 {
		t.Fatalf("TaskbarCreated × 20 之后成功的 NIM_ADD = %d，want 1", countAdd.Load())
	}

	gdi1, usr1 := guiResources()
	tr.Close()
	tr.Close() // 重复关无害
	if countDelete.Load() != 1 {
		t.Fatalf("NIM_DELETE = %d，want 1", countDelete.Load())
	}
	if _, err := Start(Options{Tooltip: "关了再开"}); err != ErrAlreadyRunning {
		t.Fatalf("同一进程关了再 Start 应仍被拒（托盘不是可以反复重开的东西），得到 %v", err)
	}
	// 句柄：建窗口 + 图标是一次性的，500 次更新 / 20 次重加不该让它们涨。
	if usr1 > usr0+4 || gdi1 > gdi0+4 {
		t.Fatalf("GUI 句柄涨了：GDI %d→%d，USER %d→%d", gdi0, gdi1, usr0, usr1)
	}
	t.Logf("NIM_ADD=%d NIM_MODIFY=%d NIM_DELETE=%d GDI %d→%d USER %d→%d",
		countAdd.Load(), countModify.Load(), countDelete.Load(), gdi0, gdi1, usr0, usr1)
}

// waitIdle 往托盘窗口发一条同步的空消息：它返回时，之前投递的消息都已处理完。
func waitIdle(hwnd uintptr) {
	var result uintptr
	pSendMessageTimeoutW.Call(hwnd, wmNull, 0, 0, 0, 2000, uintptr(unsafe.Pointer(&result)))
	time.Sleep(50 * time.Millisecond)
}
