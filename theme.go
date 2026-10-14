package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/options"
	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// ── 壳的亮 / 暗两套（与主应用同一对底色）──────────────────────────────────
//
// 启动页与应用 iframe 后面露出来的底色、以及原生窗口的底色（缩放时边缘露出的那
// 一圈、webview 就绪前的首帧）都要跟应用当前的主题一致，否则一开一合两种颜色。
//
// 主题的唯一来源是**应用自己**：它在壳里时经 postMessage 把当前主题告诉宿主页，
// 宿主页调 SetTheme。这里把它记进壳的配置目录——下次启动时 main.go 在建窗口之前
// 读它，原生底色从第一帧就对。没有记录时跟随系统。

// 与主应用 globals.css 同值：亮 = --app-background oklch(0.989 0 0)，
// 暗 = --background oklch(0.145 0 0)。
var (
	lightBackground = options.RGBA{R: 252, G: 252, B: 252, A: 255}
	darkBackground  = options.RGBA{R: 10, G: 10, B: 10, A: 255}
)

func themeFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Vantaloom", "desktop-theme")
}

// savedTheme 返回上次记下的主题；没有记录时退回系统主题。
func savedTheme() string {
	if path := themeFile(); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if t := strings.TrimSpace(string(b)); t == "light" || t == "dark" {
				return t
			}
		}
	}
	if systemPrefersLight() {
		return "light"
	}
	return "dark"
}

func backgroundFor(theme string) *options.RGBA {
	if theme == "light" {
		c := lightBackground
		return &c
	}
	c := darkBackground
	return &c
}

// SetTheme 由宿主页在应用告知主题时调用（window.go.main.App.SetTheme）：记下来，
// 并把原生窗口底色换掉。非法值直接忽略。
func (a *App) SetTheme(theme string) {
	if theme != "light" && theme != "dark" {
		return
	}
	bg := backgroundFor(theme)
	if a.ctx != nil {
		wruntime.WindowSetBackgroundColour(a.ctx, bg.R, bg.G, bg.B, bg.A)
	}
	path := themeFile()
	if path == "" {
		return
	}
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == theme {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(theme+"\n"), 0o644)
}
