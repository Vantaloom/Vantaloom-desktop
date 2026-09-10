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

## 历史

本目录原先住在私有主仓的 `apps/desktop/`，并镜像到公开仓 `Vantaloom-app` 的
`desktop/`。2026-09-11 分仓，改为独立仓库、独立发布，主仓与公开仓的副本同时删除。
本仓保留了迁出前的全部提交历史。
