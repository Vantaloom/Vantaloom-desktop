//go:build !windows

package main

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// systemPrefersLight：macOS 读 AppleInterfaceStyle（暗色模式时为 "Dark"，亮色时
// 这个键不存在、命令以非零退出）。其它平台没有统一的来源，按暗处理。
func systemPrefersLight() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "defaults", "read", "-g", "AppleInterfaceStyle").Output()
	if err != nil {
		// 键不存在 = 亮色；超时 / 命令缺失也落到这里——宁可亮也不要卡住启动。
		return ctx.Err() == nil
	}
	return !strings.Contains(strings.ToLower(string(out)), "dark")
}
