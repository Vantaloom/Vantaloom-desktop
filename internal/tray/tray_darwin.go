//go:build darwin

package tray

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Cocoa
#include <stdlib.h>
void vtlTrayCreate(void);
void vtlTraySetTooltip(const char *text);
void vtlTrayDestroy(void);
*/
import "C"

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// macOS：菜单栏的 NSStatusItem（tray_darwin.m）。所有 AppKit 调用都派发到主线程
// （Wails 的 NSApp 已经在跑主循环）；菜单每次打开时经 menuNeedsUpdate 回到 Go 现取，
// 点了某项经 vtlTrayOnSelect 回到 Go、再派发到新 goroutine——主线程从不被业务卡住。
//
// 跨进程只有一个托盘：用 ~/Library/Application Support/Vantaloom/tray.lock 上的
// flock（进程退出时内核自动释放，崩溃也不会留下一把死锁）。

var (
	instanceTok atomic.Bool
	active      atomic.Pointer[platformTray]
)

type platformTray struct {
	opts   Options
	lockFD int
	closed atomic.Bool
	mu     sync.Mutex
	tip    string
}

func startPlatform(opts Options) (*platformTray, error) {
	if !instanceTok.CompareAndSwap(false, true) {
		return nil, ErrAlreadyRunning
	}
	fd := -1
	if dir, err := os.UserConfigDir(); err == nil {
		lockPath := filepath.Join(dir, "Vantaloom", "tray.lock")
		_ = os.MkdirAll(filepath.Dir(lockPath), 0o755)
		if f, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR, 0o644); err == nil {
			if err := syscall.Flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				syscall.Close(f)
				return nil, ErrAlreadyRunning
			}
			fd = f
		}
	}
	t := &platformTray{opts: opts, lockFD: fd}
	active.Store(t)
	C.vtlTrayCreate()
	t.setTooltip(opts.Tooltip)
	return t, nil
}

func (t *platformTray) setTooltip(text string) {
	t.mu.Lock()
	if text == t.tip {
		t.mu.Unlock()
		return
	}
	t.tip = text
	t.mu.Unlock()
	if t.closed.Load() {
		return
	}
	cs := C.CString(text)
	defer C.free(unsafe.Pointer(cs))
	C.vtlTraySetTooltip(cs)
}

// macOS 的通知要应用签名 + 用户授权（UNUserNotificationCenter），壳是 ad-hoc 签名
// 的，这里不做；Windows 那边才有「收进托盘」的提示需要它。
func (t *platformTray) notify(string, string) {}

func (t *platformTray) close() {
	if !t.closed.CompareAndSwap(false, true) {
		return
	}
	C.vtlTrayDestroy()
	active.Store(nil)
	if t.lockFD >= 0 {
		syscall.Close(t.lockFD)
		t.lockFD = -1
	}
}

type menuItemJSON struct {
	ID        int    `json:"id"`
	Label     string `json:"label"`
	Disabled  bool   `json:"disabled"`
	Separator bool   `json:"separator"`
}

//export vtlTrayMenuJSON
func vtlTrayMenuJSON() *C.char {
	t := active.Load()
	if t == nil || t.opts.Menu == nil {
		return C.CString("[]")
	}
	items := t.opts.Menu()
	out := make([]menuItemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, menuItemJSON{ID: it.ID, Label: it.Label, Disabled: it.Disabled || it.ID == 0, Separator: it.Separator})
	}
	b, err := json.Marshal(out)
	if err != nil {
		return C.CString("[]")
	}
	return C.CString(string(b)) // 由 ObjC 侧 free
}

//export vtlTrayOnSelect
func vtlTrayOnSelect(id C.int) {
	t := active.Load()
	if t == nil || t.opts.OnSelect == nil {
		return
	}
	go t.opts.OnSelect(int(id))
}
