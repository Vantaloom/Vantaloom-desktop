package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	rt "vantaloom.local/apps/desktop/internal/runtime"
)

// ── 后端没了：说真话 + 能自救（Vantaloom/Vantaloom#119）─────────────────────
//
// 以前壳对**任何**健康中断都显示「正在更新 Vantaloom」——它看不见真正的更新，而绝大
// 多数中断其实是后端崩了，用户盯着一个根本不存在的更新进度条。现在分四种，每种只说
// 壳真知道的事：
//
//	stopped    用户从托盘停的（#117）                  → 服务已停止 + 启动服务
//	installing self-update.json 刚写了 phase=installing → 正在安装更新 vX（真在装）
//	crash      api.err.log 在这次中断里新增了 panic / fatal → 客户端后端崩溃了 + 原因
//	           + 打开日志文件夹 + 重启 / 回退到旧版本；**不自动拉起**（崩了再拉起
//	           多半再崩，用户得先看见）
//	unknown    都不是                                   → 后端服务未响应（自动重连一次）
//
// 崩溃判据只看**这次中断新增**的日志：后端健康时记下 api.err.log 的大小
// （errLogOffset），掉线后只读那之后追加的字节——老日志里本来就躺着的 fatal 不会
// 把一次普通重启判成崩溃。
//
// 回退：拉 npm 上的版本列表 → 选一个 → 与首次安装同一条路装上 → 刷新应用，并把这个
// 版本**固定**下来：壳不再自动把它升回去（否则下次打开壳就又升回那个崩溃的版本），
// 直到用户在托盘或回退对话框里点「恢复自动更新」。

const (
	downStopped    = "stopped"
	downInstalling = "installing"
	downCrash      = "crash"
	downUnknown    = ""

	// rollbackEvent 是回退安装的进度事件（与启动页的 vantaloom:boot 同形）。
	rollbackEvent = "vantaloom:rollback"
	// openRollbackEvent 让宿主页打开回退对话框（托盘菜单用）。
	openRollbackEvent = "vantaloom:open-rollback"

	// selfUpdateFresh：self-update.json 的 installing 多久之内算「正在装」。
	selfUpdateFresh = 10 * time.Minute
)

type recoveryState struct {
	// errLogOffset：最近一次后端健康时 api.err.log 的大小。
	errLogOffset atomic.Int64
	// installing：用户发起的版本安装正在进行（回退对话框在显示进度），健康监视器
	// 这段时间不弹覆盖层、不自动拉起。
	installing atomic.Bool
	// bootCrash：启动失败时从日志里找到的崩溃（给启动页的错误态用）。
	bootCrash atomic.Pointer[rt.CrashInfo]
}

// classifyDown 判断这次中断是哪一种。detail 原样交给宿主页。
func (a *App) classifyDown() (string, map[string]any) {
	if a.userStoppedService() {
		return downStopped, nil
	}
	if ok, version := a.mgr.SelfUpdateInstalling(selfUpdateFresh); ok {
		return downInstalling, map[string]any{"version": version}
	}
	if crash := a.mgr.CrashSince(a.recovery.errLogOffset.Load()); crash != nil {
		return downCrash, map[string]any{"reason": crash.Reason, "at": crash.At}
	}
	return downUnknown, nil
}

// downOverlayJS 生成调宿主页 __vtlBackendDown 的那一行（加 / 减都在宿主页里）。
func downOverlayJS(kind string, detail map[string]any) string {
	if detail == nil {
		detail = map[string]any{}
	}
	k, _ := json.Marshal(kind)
	d, _ := json.Marshal(detail)
	return fmt.Sprintf("window.__vtlBackendDown && window.__vtlBackendDown(true, %s, %s)", k, d)
}

// monitorBackend 每 2s 探一次健康：
//   - 后端在：记下 api.err.log 的大小；版本变了（不停机的热重启）刷新应用。
//   - 后端没了：classifyDown；stopped / installing / crash 立刻显示，unknown 等 4s
//     （忽略瞬时抖动）；只对 unknown 自动拉起一次（8s）。
//   - 回来了：先拆覆盖层，再刷新应用（刷新只动 iframe，拆不掉宿主页的节点）。
//
// 用户在回退对话框里装版本时（recovery.installing）整段跳过——那边自己显示进度。
func (a *App) monitorBackend() {
	downSince := time.Time{}
	shown := false
	shownKey := ""
	revived := false
	// sawCrash：这次中断里判出过崩溃。之后哪怕用户点了重启、判据回到 unknown，也
	// 不再自动拉起——重启已经由那颗按钮发起了，再拉一次只会和它抢。
	sawCrash := false

	for {
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
		if a.recovery.installing.Load() {
			continue
		}

		v, ok := a.mgr.Health(a.ctx)
		if !ok {
			if downSince.IsZero() {
				downSince = time.Now()
			}
			down := time.Since(downSince)
			kind, detail := a.classifyDown()
			if kind == downCrash {
				sawCrash = true
			}
			key := kind + mustJSON(detail)
			if kind != downUnknown || down > 4*time.Second {
				if !shown || key != shownKey {
					shown, shownKey = true, key
					wruntime.WindowExecJS(a.ctx, downOverlayJS(kind, detail))
				}
			}
			// 只救「不知道为什么没了」的那种：停是用户的意思，装有安装进程在管，
			// 崩了再拉起多半再崩——那两颗按钮（重启 / 回退）交给用户。
			if kind == downUnknown && !sawCrash && !revived && down > autoReviveAfter {
				revived = true
				go func() {
					if err := a.mgr.Start(a.ctx); err != nil {
						fmt.Printf("[desktop] auto-revive backend: %v\n", err)
					}
				}()
			}
			continue
		}

		// Backend is up.
		a.recovery.errLogOffset.Store(a.mgr.APIErrLogSize())
		if shown {
			shown, shownKey = false, ""
			wruntime.WindowExecJS(a.ctx, removeDownOverlayJS)
			wruntime.WindowExecJS(a.ctx, "window.__vtlReloadApp ? window.__vtlReloadApp() : window.location.reload()")
			a.lastVersion = v
			downSince = time.Time{}
			revived, sawCrash = false, false
			continue
		}
		if a.lastVersion != "" && v != "" && v != a.lastVersion {
			a.lastVersion = v
			wruntime.WindowExecJS(a.ctx, "window.__vtlReloadApp ? window.__vtlReloadApp() : window.location.reload()")
		}
		downSince = time.Time{}
		revived, sawCrash = false, false
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ── 绑定给宿主页的方法 ─────────────────────────────────────────────────────

// RecoveryInfo 是启动页 / 覆盖层要的现状。
type RecoveryInfo struct {
	InstalledVersion string        `json:"installedVersion"`
	PinnedVersion    string        `json:"pinnedVersion"`
	BootCrash        *rt.CrashInfo `json:"bootCrash"`
	LogDir           string        `json:"logDir"`
}

// GetRecoveryInfo（window.go.main.App.GetRecoveryInfo）。
func (a *App) GetRecoveryInfo() RecoveryInfo {
	return RecoveryInfo{
		InstalledVersion: a.mgr.InstalledVersion(),
		PinnedVersion:    a.pinnedVersion(),
		BootCrash:        a.recovery.bootCrash.Load(),
		LogDir:           a.mgr.LogDir(),
	}
}

// OpenLogsFolder 打开运行时的日志目录（「客户端后端崩溃了，点击打开日志文件夹」）。
func (a *App) OpenLogsFolder() {
	dir := a.mgr.LogDir()
	_ = os.MkdirAll(dir, 0o755)
	openFolder(dir)
}

// ListRuntimeVersions 拉可回退的版本列表（不低于 0.13.5，倒序）。
func (a *App) ListRuntimeVersions() ([]rt.RuntimeVersion, error) {
	ctx, cancel := context.WithTimeout(a.ctx, 45*time.Second)
	defer cancel()
	return a.mgr.ListVersions(ctx)
}

// InstallRuntimeVersion 装指定版本（回退或指定升级）。与首次安装同一条路：停服务
// → 铺文件 → ctl install → ctl start → 等健康；成功后把它固定下来（装的是最新版
// 则解除固定），刷新应用。进度经 vantaloom:rollback 推给宿主页。
func (a *App) InstallRuntimeVersion(version string) error {
	version = strings.TrimSpace(version)
	if version == "" || version == "latest" {
		return errors.New("请选择一个具体的版本")
	}
	if !a.recovery.installing.CompareAndSwap(false, true) {
		return errors.New("已经有一个版本正在安装")
	}
	defer a.recovery.installing.Store(false)

	emit := func(p rt.Progress) { wruntime.EventsEmit(a.ctx, rollbackEvent, p) }
	ctx, cancel := context.WithTimeout(a.ctx, 12*time.Minute)
	defer cancel()
	// 只认这次安装之后新增的崩溃（日志里本来躺着的老崩溃不算）。
	logOffset := a.mgr.APIErrLogSize()

	installed, err := a.mgr.Install(ctx, version, emit)
	if err != nil {
		return err
	}
	emit(rt.Progress{Phase: "wait", Message: "正在等待后端就绪…", Percent: 95})
	if err := a.mgr.WaitHealthy(ctx, 90*time.Second); err != nil {
		if crash := a.mgr.CrashSince(logOffset); crash != nil {
			return fmt.Errorf("%s 装好了，但启动后崩溃了：%s（%s）", installed, crash.Reason, crash.At)
		}
		return fmt.Errorf("%s 装好了，但后端没有在 90 秒内就绪：%w", installed, err)
	}

	latest := ""
	lctx, lcancel := context.WithTimeout(a.ctx, 8*time.Second)
	if l, lerr := a.mgr.LatestVersion(lctx); lerr == nil {
		latest = l
	}
	lcancel()
	if latest != "" && installed == latest {
		a.setPinnedVersion("")
	} else {
		a.setPinnedVersion(installed)
	}
	if latest == "" || installed != latest {
		a.quietRuntimeAutoInstall()
	}
	a.recovery.errLogOffset.Store(a.mgr.APIErrLogSize())
	if v, ok := a.mgr.Health(a.ctx); ok {
		a.lastVersion = v
	}
	emit(rt.Progress{Phase: "done", Message: fmt.Sprintf("已切换到 %s", installed), Percent: 100})
	wruntime.WindowExecJS(a.ctx, removeDownOverlayJS)
	wruntime.WindowExecJS(a.ctx, "window.__vtlReloadApp ? window.__vtlReloadApp() : window.location.reload()")
	if a.tray != nil {
		go a.refreshTrayState()
	}
	return nil
}

// quietRuntimeAutoInstall：固定版本之后，运行时自己的后台更新（0.17.0+）仍会把最新版
// 下好——那没关系，装不装由用户点；但「空闲时自动安装」（#118）会在背后把它装回去，
// 固定就形同虚设。这里尽力关掉它与这一次的排队；旧运行时没有这两个端点（404 / 400），
// 一律忽略。
func (a *App) quietRuntimeAutoInstall() {
	base := fmt.Sprintf("http://127.0.0.1:%d/v1/runtime/self-update", a.mgr.APIPort())
	client := &http.Client{Timeout: 5 * time.Second}
	send := func(method, path, body string) {
		req, err := http.NewRequestWithContext(a.ctx, method, base+path, strings.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	send(http.MethodPut, "/settings", `{"autoInstallWhenIdle":false}`)
	send(http.MethodPost, "/queue", `{"queued":false}`)
}

// UnpinRuntimeVersion 解除固定：壳恢复「有新版就更新 / 提示」。
func (a *App) UnpinRuntimeVersion() { a.setPinnedVersion("") }

// openRollbackDialog：托盘菜单「回退运行时版本…」——把窗口叫回来并让宿主页打开对话框。
func (a *App) openRollbackDialog() {
	a.showWindow()
	wruntime.EventsEmit(a.ctx, openRollbackEvent)
}

// ── 固定版本 ───────────────────────────────────────────────────────────────
//
// 记在安装目录里（<prefix>/config/desktop-pin）而不是壳的配置目录：固定的是**这个
// 前缀上的运行时**——换一个 VANTALOOM_HOME 就是另一个运行时，不该继承这里的固定；
// 前缀被删了重装，固定也随之失效（重新装最新版是对的）。layDown 只换 bin/web/cli，
// config/ 不动，固定活得过每一次安装。

func (a *App) pinFile() string {
	return filepath.Join(a.mgr.Prefix, "config", "desktop-pin")
}

// pinnedVersion 返回被固定的运行时版本；"" = 没固定（照常自动更新）。
func (a *App) pinnedVersion() string {
	b, err := os.ReadFile(a.pinFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (a *App) setPinnedVersion(version string) {
	p := a.pinFile()
	if version == "" {
		_ = os.Remove(p)
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(version+"\n"), 0o644)
}
