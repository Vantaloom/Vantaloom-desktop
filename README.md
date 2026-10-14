# Vantaloom Desktop

Vantaloom 的桌面外壳（[Wails](https://wails.io) + Go）。Windows / macOS，之后可能加 Linux。

## 它是什么

一个**薄启动器**，不含任何专有核心：

1. 检测本机有没有装 Vantaloom 运行时
2. 没装就装、有更新就更新（从 npmjs.org 上的 `@vantaloom/runtime-<platform>` 拉）
3. 没启动就启动
4. 把 WebView 指向本机运行时提供的 Web 应用

真正的后端与前端在私有主仓，经 npm 分发；本仓只有这层壳，所以源码是公开的。

## 构建

```bash
wails build            # Windows 是纯 Go，不需要 gcc
```

安装 Wails CLI：

```bash
GOPROXY="https://goproxy.cn,direct" go install github.com/wailsapp/wails/v2/cmd/wails@latest
```

> 默认的 `,direct` 回落在国内连 golang.org 会超时，所以要显式指定代理。

## 发布

推 `main` 即触发 `.github/workflows/build-desktop.yml`：

| 平台 | 产物 | 签名 |
|---|---|---|
| Windows | 便携 zip（单个 `Vantaloom.exe`，免安装） | 无——SmartScreen 会提示，选「更多信息 → 仍要运行」 |
| macOS | 通用 DMG | ad-hoc 签名，未公证——首次右键 → 打开 |

产物发到 tag 为 `desktop-build<run>-<sha>` 的 GitHub Release。

**Windows 不出 NSIS 安装包**（2026-08-14 拍板）：壳就是单文件 exe，安装包做的
也只是把它释放到某个目录，不值得为它在 CI 上挂一条外部工具链依赖——首次真跑
连挂两次都挂在装 NSIS 上。

## 已知的坑

- 仓库根 `.gitignore` 忽略 `dist/` 与 `build/`，所以 `frontend/dist/index.html`
  （内嵌启动页）和 `build/appicon.png` 需要 `git add -f` 才进得了版本控制。
- 壳**看不见**运行时的更新（前端触发的 updater 是脱离壳进程的）。所以它对任何
  健康中断都不要替后端编故事说「正在更新」——只陈述可观测的事实，并给逃生按钮。
- 注入到宿主页的全屏覆盖层，「加」和「减」必须成对写：恢复路径 `__vtlReloadApp`
  只刷 iframe，够不着宿主页的节点，只加不减就是「弹出即永久」。

- **托盘（`internal/tray`）不能做成「挂了就重来」的东西。** 运行时曾经有一个由
  supervisor 常驻的独立托盘进程，某些 Windows 11 上一启动就崩，被立刻拉起、再崩、
  再拉——每轮留下一个死图标，最后内存耗尽（主仓 b4aa9617）。现在托盘住在壳进程里：
  一个进程只能 `Start` 成功一次、跨进程用命名互斥量只留一个、图标只 `NIM_ADD` 一次
  （之后只 `NIM_MODIFY`，资源管理器重启时把同一个加回去）、失败就本次不要托盘。
  `syscall.NewCallback` 也只建一次——它占的回调槽进程内有上限，反复建会 panic。
  改这里之前跑一遍 `VTL_TRAY_LIVE=1 go test ./internal/tray -run Live -v`。
- 有托盘时（Windows）关窗 = 收进托盘，真正退出走托盘菜单；主窗口带单实例锁，
  再双击 exe 只会把已有窗口叫回来。副窗口（`--route`）不建托盘、不受单实例锁。
- 用户从托盘停掉服务后，健康监视器**不去救**它（否则 8 秒后又被自己拉起来），
  覆盖层改说「服务已停止」。ctl 的 `/tray-state` 能读到托盘此刻的提示与菜单。
- **后端没了，覆盖层只说壳真知道的事**（`recovery.go`，Vantaloom/Vantaloom#119）。
  以前任何中断都显示「正在更新」，而绝大多数其实是崩溃。现在分四种：托盘停的 /
  `config/self-update.json` 刚写了 `installing`（真在装）/ `logs/api.err.log`
  **在这次中断里新增**了 `panic:` / `fatal error:`（崩溃：给原因、打开日志、重启、
  回退，**不自动拉起**）/ 都不是（未响应，自动重连一次）。崩溃判据只读「后端最后
  一次健康时的日志大小」之后的字节——live 日志里本来就躺着 0.16.18 那次 stack
  overflow，从头读会把每次普通重启都判成崩溃。
- **回退运行时版本**：版本列表来自 npm 完整 packument（要发布时间，约 0.5MB），
  下限 `0.13.5`（更早的 VERSION 是 git hash、带旧 mesh 服务、没有 iframe 宿主下的
  标题栏；会堆托盘图标的旧托盘 6 月 6 日就删了，也在下限之前）。0.13.5 起的
  `vantaloomctl` 都认 `install --prefix --version` / `start|stop --prefix`，
  `/healthz` 都带 `ok`+`version`（已逐一核对）。装上旧版后**固定**在
  `<prefix>/config/desktop-pin`：启动时不自动更新、不弹更新，并尽力关掉运行时自己的
  「空闲时自动安装」——否则下次开壳 / 空闲一分钟就又升回那个崩溃的版本。
- WebView2 的远程调试端口开不出来（Wails 自己传了浏览器参数，
  `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS` 被覆盖）。e2e 要看真实窗口画面用
  `PrintWindow(PW_RENDERFULLCONTENT)`，截图进程要先 `SetProcessDPIAware`，否则在
  150% 缩放下只截到左上角三分之二。

## 历史

本目录原先住在私有主仓的 `apps/desktop/`，并镜像到公开仓 `Vantaloom-app` 的
`desktop/`。2026-09-11 分仓，改为独立仓库、独立发布，主仓与公开仓的副本同时删除。
本仓保留了迁出前的全部提交历史。
