// Package tray 是桌面壳的系统托盘图标（Vantaloom/Vantaloom#117）。
//
// ── 为什么要从头写、而且写得这么拘谨 ──────────────────────────────────────
//
// 运行时曾经有过一个托盘（vantaloom-tray.exe，2026-06 删除，主仓 b4aa9617）。它是
// 一个由 vantaloomctl supervisor 常驻的**独立进程**，用 getlantern/systray。某些
// Windows 11 版本上它一启动就崩（rundll 报错），supervisor 立刻再拉起——崩掉的进程
// 来不及删图标，新进程又加一个，循环到内存耗尽、机器卡死，托盘区堆了几十个图标。
// 图标堆积是结果，根因是「崩溃 → 立即重启」的无限循环。
//
// 所以这一版的每条约束都对着那个根因：
//
//  1. 托盘住在壳进程里，**没有任何东西会重启它**。Start 失败只返回错误，调用方记
//     日志、本次没有托盘，绝不重试。
//  2. 一个进程最多一个托盘（单例），并且跨进程只有一个（命名互斥量）：副窗口进程
//     （--route）与误开的第二个主窗口都拿不到。
//  3. 图标只 NIM_ADD 一次；之后状态变化一律 NIM_MODIFY，而且只在文字真的变了时才
//     发。资源管理器重启（TaskbarCreated）时把同一个图标加回去；退出时 NIM_DELETE。
//  4. 不用第三方托盘库：Windows 纯 Win32 syscall（与壳里窗控 / 截图同一做法，无
//     cgo），macOS 用 NSStatusItem（cgo）。
//  5. 菜单每次打开时现建、关掉立刻销毁；回调派发到新 goroutine，消息循环永远不被
//     业务逻辑卡住。
package tray

import "errors"

// ErrUnsupported：当前平台没有托盘实现。
var ErrUnsupported = errors.New("tray: 当前平台不支持托盘")

// ErrAlreadyRunning：本进程已经有一个托盘，或者另一个壳进程已经持有托盘。
var ErrAlreadyRunning = errors.New("tray: 已有一个托盘图标在运行")

// Item 是右键菜单的一项。ID 为 0 的普通项不可点（当作说明文字）。
type Item struct {
	ID        int
	Label     string
	Disabled  bool
	Separator bool
	// Default 粗体显示（Windows），表示「单击图标时做的就是这件事」。
	Default bool
}

// Options 配置一个托盘。
type Options struct {
	// Tooltip 是悬停提示（Windows 最长 127 字，超出截断）。
	Tooltip string
	// OnActivate 在单击图标时调用（Windows）。在新 goroutine 里调。
	OnActivate func()
	// Menu 在每次打开右键菜单时调用，返回当前应显示的菜单。
	Menu func() []Item
	// OnSelect 在点了某一项时调用。在新 goroutine 里调。
	OnSelect func(id int)
}

// Tray 是一个运行中的托盘图标。
type Tray struct {
	impl *platformTray
}

// Start 创建托盘图标。同一进程只能成功一次（之后返回 ErrAlreadyRunning）；失败不
// 重试，由调用方决定本次不要托盘。
func Start(opts Options) (*Tray, error) {
	impl, err := startPlatform(opts)
	if err != nil {
		return nil, err
	}
	return &Tray{impl: impl}, nil
}

// SetTooltip 更新悬停提示；与当前相同时什么都不做。
func (t *Tray) SetTooltip(text string) {
	if t == nil || t.impl == nil {
		return
	}
	t.impl.setTooltip(text)
}

// Notify 弹一条系统通知（Windows 气泡 / 通知中心）。
func (t *Tray) Notify(title, message string) {
	if t == nil || t.impl == nil {
		return
	}
	t.impl.notify(title, message)
}

// Close 删除图标并释放资源；可重复调用。会等到图标真的删掉（有上限），保证进程
// 退出时不留下一个死图标。
func (t *Tray) Close() {
	if t == nil || t.impl == nil {
		return
	}
	t.impl.close()
}
