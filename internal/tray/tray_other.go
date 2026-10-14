//go:build !windows && !darwin

package tray

// 其它平台（Linux）暂无托盘：Start 恒返回 ErrUnsupported，壳照常运行。

type platformTray struct{}

func startPlatform(Options) (*platformTray, error) { return nil, ErrUnsupported }

func (t *platformTray) setTooltip(string)     {}
func (t *platformTray) notify(string, string) {}
func (t *platformTray) close()                {}
