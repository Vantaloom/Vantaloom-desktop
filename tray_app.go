package main

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"vantaloom.local/apps/desktop/internal/tray"
)

// ── 托盘（Vantaloom/Vantaloom#117）：右键看服务状态、启停服务、打开日志 ──────
//
// 托盘本身的约束（单例、只加一次、不重启）在 internal/tray 里。这里只管三件事：
//
//   - 状态：每 3s 打一次本机 /healthz（一次回环 HTTP，不 fork vantaloomctl），变了才
//     改悬停提示；右键菜单打开时按最新状态现建。
//   - 动作：启动 / 停止 / 重启服务走 vantaloomctl（与 CLI 同一条路），进行中整组置灰，
//     失败弹一条通知说清原因。
//   - 关窗：有托盘时点关闭 = 收进托盘（Windows），真正退出走托盘菜单。macOS 上关窗
//     行为不动——本机没有 Mac 能验证 Dock 重新打开窗口那一套，宁可保守。

const trayPollInterval = 3 * time.Second

const (
	trayOpen = iota + 1
	trayBrowser
	trayStart
	trayStop
	trayRestart
	trayLogs
	trayQuit
)

// serviceState 是托盘看到的服务状态（只由 trayLoop 与动作改）。
type serviceState struct {
	running bool
	version string
	port    int
	// busy 非空 = 正在执行的动作（「正在停止…」）；期间三颗启停按钮置灰。
	busy string
}

type trayState struct {
	t        *tray.Tray
	mu       sync.Mutex
	st       serviceState
	quitting atomic.Bool
	// hideNoticeShown：第一次「关窗收进托盘」时提示一句，之后不再打扰。
	hideNoticeShown atomic.Bool
	// stoppedByUser：服务是用户从托盘停掉的。健康监视器据此不去「救」它，覆盖层也
	// 改说「服务已停止」而不是「未响应、正在重启」——否则停了 8 秒又被自己拉起来。
	stoppedByUser atomic.Bool
	// booting：Bootstrap 还在跑（安装 / 更新 / 启动中），状态行显示「正在启动…」。
	booting atomic.Bool
}

// startTray 只在主窗口进程里调（副窗口 --route 不建托盘）。失败只记日志：本次没有
// 托盘，壳照常可用，绝不重试。
func (a *App) startTray() {
	if a.route != "" {
		return
	}
	ts := &trayState{}
	ts.booting.Store(true)
	t, err := tray.Start(tray.Options{
		Tooltip:    "Vantaloom · 正在启动…",
		OnActivate: a.showWindow,
		Menu:       a.trayMenu,
		OnSelect:   a.onTraySelect,
	})
	if err != nil {
		fmt.Printf("[desktop] 托盘不可用（本次不建）：%v\n", err)
		return
	}
	ts.t = t
	a.tray = ts
	go a.trayLoop()
}

func (a *App) stopTray() {
	if a.tray != nil && a.tray.t != nil {
		a.tray.t.Close()
	}
}

// trayLoop 轮询服务状态，直到窗口上下文结束。
func (a *App) trayLoop() {
	ticker := time.NewTicker(trayPollInterval)
	defer ticker.Stop()
	for {
		a.refreshTrayState()
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refreshTrayState 探一次健康并更新悬停提示（只在文字变了时真的发给系统）。
func (a *App) refreshTrayState() {
	ts := a.tray
	if ts == nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	version, ok := a.mgr.Health(ctx)
	cancel()
	ts.mu.Lock()
	ts.st.running = ok
	ts.st.version = version
	ts.st.port = a.mgr.APIPort()
	tip := "Vantaloom · " + statusLine(ts.st, ts.booting.Load(), ts.stoppedByUser.Load())
	ts.mu.Unlock()
	ts.t.SetTooltip(tip)
}

func statusLine(st serviceState, booting, stoppedByUser bool) string {
	switch {
	case st.busy != "":
		return st.busy
	case st.running && st.version != "":
		return fmt.Sprintf("服务运行中（v%s，端口 %d）", st.version, st.port)
	case st.running:
		return fmt.Sprintf("服务运行中（端口 %d）", st.port)
	case booting:
		return "正在启动…"
	case stoppedByUser:
		return "服务已停止"
	default:
		return "服务未运行"
	}
}

func (a *App) trayMenu() []tray.Item {
	ts := a.tray
	ts.mu.Lock()
	st := ts.st
	ts.mu.Unlock()
	booting := ts.booting.Load()
	busy := st.busy != "" || booting
	installed := a.mgr.Installed()
	return []tray.Item{
		{Label: statusLine(st, booting, ts.stoppedByUser.Load())},
		{Separator: true},
		{ID: trayOpen, Label: "打开 Vantaloom", Default: true},
		{ID: trayBrowser, Label: "在浏览器中打开", Disabled: !st.running},
		{Separator: true},
		{ID: trayStart, Label: "启动服务", Disabled: busy || st.running || !installed},
		{ID: trayStop, Label: "停止服务", Disabled: busy || !st.running},
		{ID: trayRestart, Label: "重启服务", Disabled: busy || !installed},
		{Separator: true},
		{ID: trayLogs, Label: "打开日志文件夹"},
		{Separator: true},
		{ID: trayQuit, Label: "退出 Vantaloom（服务继续在后台运行）"},
	}
}

func (a *App) onTraySelect(id int) {
	switch id {
	case trayOpen:
		a.showWindow()
	case trayBrowser:
		wruntime.BrowserOpenURL(a.ctx, a.mgr.BackendURL())
	case trayStart:
		a.runServiceAction("正在启动服务…", "启动服务失败", func(ctx context.Context) error {
			a.tray.stoppedByUser.Store(false)
			return a.mgr.Start(ctx)
		})
	case trayStop:
		a.runServiceAction("正在停止服务…", "停止服务失败", func(ctx context.Context) error {
			// 先立旗再停：健康监视器在停的过程中就会看到后端没了，它得知道这是故意的。
			a.tray.stoppedByUser.Store(true)
			err := a.mgr.Stop(ctx)
			if err != nil {
				a.tray.stoppedByUser.Store(false)
			}
			return err
		})
	case trayRestart:
		a.runServiceAction("正在重启服务…", "重启服务失败", func(ctx context.Context) error {
			a.tray.stoppedByUser.Store(false)
			if err := a.mgr.Stop(ctx); err != nil {
				return err
			}
			return a.mgr.Start(ctx)
		})
	case trayLogs:
		openFolder(filepath.Join(a.mgr.Prefix, "logs"))
	case trayQuit:
		a.quitShell()
	}
}

// runServiceAction 串行执行一个启停动作：进行中整组置灰、提示文字换成进行时，结束后
// 立刻刷新一次状态；失败弹通知。同一时刻只允许一个动作。
func (a *App) runServiceAction(busy, failTitle string, fn func(ctx context.Context) error) {
	ts := a.tray
	ts.mu.Lock()
	if ts.st.busy != "" {
		ts.mu.Unlock()
		return
	}
	ts.st.busy = busy
	ts.mu.Unlock()
	ts.t.SetTooltip("Vantaloom · " + busy)

	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	err := fn(ctx)
	if err == nil && !ts.stoppedByUser.Load() {
		// 启动 / 重启：等到真的能响应（或超时）再撤掉「进行中」。
		_ = a.mgr.WaitHealthy(ctx, 60*time.Second)
	}
	cancel()

	ts.mu.Lock()
	ts.st.busy = ""
	ts.mu.Unlock()
	a.refreshTrayState()
	if err != nil {
		ts.t.Notify(failTitle, trimForNotice(err.Error()))
	}
}

func trimForNotice(s string) string {
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:199]) + "…"
	}
	return s
}

// showWindow 把窗口叫回前台（托盘单击 / 菜单「打开」/ 再次双击 exe）。
func (a *App) showWindow() {
	if a.ctx == nil {
		return
	}
	wruntime.WindowShow(a.ctx)
	wruntime.WindowUnminimise(a.ctx)
	focusMainWindow()
}

// closeWindow 是窗口「关闭」的唯一出口（顶栏按钮、应用内标题栏、Alt+F4）：有托盘
// （Windows）就收进托盘，否则退出。
func (a *App) closeWindow() {
	if a.hideToTrayOnClose() {
		wruntime.WindowHide(a.ctx)
		if a.tray.hideNoticeShown.CompareAndSwap(false, true) {
			a.tray.t.Notify("Vantaloom 仍在运行", "窗口已收进系统托盘。右键托盘图标可查看服务状态、启停服务或退出。")
		}
		return
	}
	wruntime.Quit(a.ctx)
}

func (a *App) hideToTrayOnClose() bool {
	return runtime.GOOS == "windows" && a.tray != nil && !a.tray.quitting.Load()
}

// quitShell 真正退出壳（托盘菜单「退出」）。服务不受影响，继续在后台运行。
func (a *App) quitShell() {
	if a.tray != nil {
		a.tray.quitting.Store(true)
	}
	wruntime.Quit(a.ctx)
}

// beforeClose 接 Wails 的 OnBeforeClose（系统级关闭：Alt+F4、任务栏右键关闭）。
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	if a.hideToTrayOnClose() {
		a.closeWindow()
		return true
	}
	return false
}

// userStoppedService 给健康监视器用：服务是用户从托盘故意停掉的。
func (a *App) userStoppedService() bool {
	return a.tray != nil && a.tray.stoppedByUser.Load()
}

func (a *App) setBooting(v bool) {
	if a.tray != nil {
		a.tray.booting.Store(v)
		go a.refreshTrayState()
	}
}

func openFolder(path string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	// 不加 winproc.Hide：那会连资源管理器的窗口一起藏掉。
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

// trayStateSnapshot 给 ctl 的 /tray-state：托盘此刻的提示与菜单。
func (a *App) trayStateSnapshot() map[string]any {
	ts := a.tray
	if ts == nil {
		return map[string]any{"tray": false}
	}
	ts.mu.Lock()
	st := ts.st
	ts.mu.Unlock()
	items := []map[string]any{}
	for _, it := range a.trayMenu() {
		if it.Separator {
			continue
		}
		items = append(items, map[string]any{"id": it.ID, "label": it.Label, "disabled": it.Disabled || it.ID == 0})
	}
	return map[string]any{
		"tray":             true,
		"tooltip":          "Vantaloom · " + statusLine(st, ts.booting.Load(), ts.stoppedByUser.Load()),
		"menu":             items,
		"hiddenToTrayOnce": a.tray.hideNoticeShown.Load(),
	}
}
