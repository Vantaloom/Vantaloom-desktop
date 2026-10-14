//go:build windows

package tray

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// ── Win32 ───────────────────────────────────────────────────────────────────

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pRegisterClassExW       = user32.NewProc("RegisterClassExW")
	pCreateWindowExW        = user32.NewProc("CreateWindowExW")
	pDefWindowProcW         = user32.NewProc("DefWindowProcW")
	pDestroyWindow          = user32.NewProc("DestroyWindow")
	pGetMessageW            = user32.NewProc("GetMessageW")
	pTranslateMessage       = user32.NewProc("TranslateMessage")
	pDispatchMessageW       = user32.NewProc("DispatchMessageW")
	pPostMessageW           = user32.NewProc("PostMessageW")
	pPostQuitMessage        = user32.NewProc("PostQuitMessage")
	pRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	pCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	pAppendMenuW            = user32.NewProc("AppendMenuW")
	pSetMenuDefaultItem     = user32.NewProc("SetMenuDefaultItem")
	pTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	pDestroyMenu            = user32.NewProc("DestroyMenu")
	pSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	pGetCursorPos           = user32.NewProc("GetCursorPos")
	pLoadIconW              = user32.NewProc("LoadIconW")
	pDestroyIcon            = user32.NewProc("DestroyIcon")
	pGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	pShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
	pExtractIconExW         = shell32.NewProc("ExtractIconExW")
	pGetModuleHandleW       = kernel32.NewProc("GetModuleHandleW")
	pCreateMutexW           = kernel32.NewProc("CreateMutexW")
	pCloseHandle            = kernel32.NewProc("CloseHandle")
)

const (
	wmDestroy     = 0x0002
	wmNull        = 0x0000
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmContextMenu = 0x007B
	wmApp         = 0x8000

	msgTrayCallback = wmApp + 1 // 图标上的鼠标事件
	msgApplyTooltip = wmApp + 2 // 别的 goroutine 改了提示文字
	msgNotify       = wmApp + 3 // 别的 goroutine 要弹一条通知
	msgClose        = wmApp + 4 // 别的 goroutine 要关掉托盘

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifInfo    = 0x01
	niifNoSound = 0x10

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020
	tpmNoNotify    = 0x0080
	tpmReturnCmd   = 0x0100

	idiApplication = 32512
	smCxSmIcon     = 49

	errorAlreadyExists = 183
)

// notifyIconData = NOTIFYICONDATAW（V3+，x64 / arm64 上 976 字节）。字段顺序与对齐
// 和 C 头文件一致——Go 在 uint32 后接 uintptr 时补的 4 字节正是 C 的那份填充。
// 尺寸由 tray_windows_test.go 钉住。
type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GUIDItem         [16]byte
	HBalloonIcon     uintptr
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type point struct{ X, Y int32 }

type msg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	_       uint32
}

// ── 单例 ────────────────────────────────────────────────────────────────────
//
// syscall.NewCallback 每调一次就永久占一个回调槽（进程上限约 2000，用完直接
// panic）——它只能在包里建**一次**。窗口过程经它进来，靠这个包级指针找到唯一的
// 那个托盘。

var (
	startOnce   sync.Once
	wndProcPtr  uintptr
	active      atomic.Pointer[platformTray]
	instanceTok atomic.Bool
)

// 测试与诊断用的计数（只增不减）：每次 NIM_ADD / NIM_MODIFY / NIM_DELETE 记一笔。
var (
	countAdd    atomic.Int64
	countModify atomic.Int64
	countDelete atomic.Int64
)

type platformTray struct {
	opts Options

	hwnd           uintptr
	icon           uintptr
	ownIcon        bool
	taskbarCreated uint32
	mutex          uintptr
	closed         atomic.Bool
	done           chan struct{}

	mu          sync.Mutex
	tooltip     string // 想要的
	shownTip    string // 最后一次真的交给系统的
	noteTitle   string
	noteMessage string
}

func startPlatform(opts Options) (*platformTray, error) {
	// 同一进程只允许成功一次——包括「上一个已经 Close 了」的情况：托盘不是一个可以
	// 关了再开的东西，重开就是上次那场事故的形状。
	if !instanceTok.CompareAndSwap(false, true) {
		return nil, ErrAlreadyRunning
	}

	// 跨进程只有一个：第二个主窗口进程（或任何别的壳进程）拿不到托盘。
	name, _ := syscall.UTF16PtrFromString(`Local\Vantaloom.Desktop.Tray`)
	h, _, callErr := pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return nil, fmt.Errorf("tray: CreateMutex: %v", callErr)
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno == errorAlreadyExists {
		pCloseHandle.Call(h)
		return nil, ErrAlreadyRunning
	}

	t := &platformTray{opts: opts, mutex: h, tooltip: truncateTip(opts.Tooltip), done: make(chan struct{})}
	ready := make(chan error, 1)
	go t.run(ready)
	select {
	case err := <-ready:
		if err != nil {
			pCloseHandle.Call(h)
			return nil, err
		}
		return t, nil
	case <-time.After(5 * time.Second):
		// 线程卡在建窗口 / 加图标上（系统极忙）：不等它，也不重试。它若之后成功了，
		// close 照常能把它收掉。
		return nil, errors.New("tray: 初始化超时")
	}
}

// run 独占一个 OS 线程：窗口、图标、菜单、消息循环全在这里，别的 goroutine 只发消息。
func (t *platformTray) run(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(t.done)

	startOnce.Do(func() { wndProcPtr = syscall.NewCallback(wndProc) })

	hinst, _, _ := pGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("VantaloomDesktopTray")
	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   wndProcPtr,
		HInstance:     hinst,
		LpszClassName: className,
	}
	// 类在本进程只注册这一次（instanceTok 保证 run 只跑一次）。
	if r, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		ready <- fmt.Errorf("tray: RegisterClassEx: %v", err)
		return
	}
	// 隐藏的普通窗口（不是 message-only 窗口：TrackPopupMenu 前要 SetForegroundWindow
	// 它，message-only 窗口不能成为前台，菜单点外面就收不起来）。从不 ShowWindow。
	hwnd, _, err := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, 0, 0, hinst, 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("tray: CreateWindowEx: %v", err)
		return
	}
	t.hwnd = hwnd
	tc, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := pRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(tc)))
	t.taskbarCreated = uint32(r)
	t.icon, t.ownIcon = loadAppIcon()
	active.Store(t)

	if !t.shell(nimAdd, nifMessage|nifIcon|nifTip, "", "") {
		active.Store(nil)
		pDestroyWindow.Call(hwnd)
		ready <- errors.New("tray: Shell_NotifyIcon(NIM_ADD) 失败")
		return
	}
	ready <- nil

	var m msg
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT，-1 = 出错：都结束，绝不重来
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	if t.ownIcon && t.icon != 0 {
		pDestroyIcon.Call(t.icon)
	}
	active.Store(nil)
}

func wndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	t := active.Load()
	if t == nil || hwnd != t.hwnd {
		r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
		return r
	}
	switch {
	case message == msgTrayCallback:
		switch uint32(lParam) & 0xFFFF {
		case wmLButtonUp:
			if fn := t.opts.OnActivate; fn != nil {
				go fn()
			}
		case wmRButtonUp, wmContextMenu:
			t.showMenu()
		}
		return 0
	case message == msgApplyTooltip:
		t.mu.Lock()
		tip := t.tooltip
		changed := tip != t.shownTip
		t.mu.Unlock()
		if changed && t.shell(nimModify, nifTip, "", "") {
			t.mu.Lock()
			t.shownTip = tip
			t.mu.Unlock()
		}
		return 0
	case message == msgNotify:
		t.mu.Lock()
		title, body := t.noteTitle, t.noteMessage
		t.mu.Unlock()
		t.shell(nimModify, nifInfo, title, body)
		return 0
	case message == msgClose:
		t.shell(nimDelete, 0, "", "")
		pDestroyWindow.Call(hwnd)
		return 0
	case message == wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	case t.taskbarCreated != 0 && message == t.taskbarCreated:
		// 资源管理器重启了：托盘区是全新的，我们那个图标已经不在了——把**同一个**
		// （同一 hWnd + uID）加回去。这是唯一一处会再次 NIM_ADD 的地方。
		t.shell(nimAdd, nifMessage|nifIcon|nifTip, "", "")
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

// shell 调 Shell_NotifyIconW。图标恒为 hWnd + uID=1 这一个。
func (t *platformTray) shell(op uintptr, flags uint32, infoTitle, info string) bool {
	nid := notifyIconData{
		CbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:             t.hwnd,
		UID:              1,
		UFlags:           flags,
		UCallbackMessage: msgTrayCallback,
		HIcon:            t.icon,
	}
	if flags&nifTip != 0 {
		t.mu.Lock()
		copyUTF16(nid.SzTip[:], t.tooltip)
		t.mu.Unlock()
	}
	if flags&nifInfo != 0 {
		copyUTF16(nid.SzInfoTitle[:], infoTitle)
		copyUTF16(nid.SzInfo[:], info)
		nid.DwInfoFlags = niifInfo | niifNoSound
	}
	r, _, _ := pShellNotifyIconW.Call(op, uintptr(unsafe.Pointer(&nid)))
	if r != 0 {
		switch op {
		case nimAdd:
			countAdd.Add(1)
			t.mu.Lock()
			t.shownTip = t.tooltip
			t.mu.Unlock()
		case nimModify:
			countModify.Add(1)
		case nimDelete:
			countDelete.Add(1)
		}
	}
	return r != 0
}

// showMenu 现建菜单 → 弹出 → 立刻销毁。返回的命令派发到新 goroutine。
func (t *platformTray) showMenu() {
	if t.opts.Menu == nil {
		return
	}
	items := t.opts.Menu()
	menu, _, _ := pCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer pDestroyMenu.Call(menu)
	for _, it := range items {
		if it.Separator {
			pAppendMenuW.Call(menu, mfSeparator, 0, 0)
			continue
		}
		label, _ := syscall.UTF16PtrFromString(it.Label)
		flags := uintptr(mfString)
		if it.Disabled || it.ID == 0 {
			flags |= mfGrayed
		}
		pAppendMenuW.Call(menu, flags, uintptr(it.ID), uintptr(unsafe.Pointer(label)))
		if it.Default && it.ID != 0 {
			pSetMenuDefaultItem.Call(menu, uintptr(it.ID), 0)
		}
	}
	var pt point
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// 不先把隐藏窗口设成前台，菜单点外面不会收起（MS 文档里的老规矩）。
	pSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := pTrackPopupMenu.Call(menu, tpmRightButton|tpmBottomAlign|tpmNoNotify|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	pPostMessageW.Call(t.hwnd, wmNull, 0, 0)
	if id := int(cmd); id != 0 && t.opts.OnSelect != nil {
		go t.opts.OnSelect(id)
	}
}

func (t *platformTray) setTooltip(text string) {
	text = truncateTip(text)
	t.mu.Lock()
	if text == t.tooltip {
		t.mu.Unlock()
		return
	}
	t.tooltip = text
	t.mu.Unlock()
	if !t.closed.Load() {
		pPostMessageW.Call(t.hwnd, msgApplyTooltip, 0, 0)
	}
}

func (t *platformTray) notify(title, message string) {
	t.mu.Lock()
	t.noteTitle, t.noteMessage = title, message
	t.mu.Unlock()
	if !t.closed.Load() {
		pPostMessageW.Call(t.hwnd, msgNotify, 0, 0)
	}
}

func (t *platformTray) close() {
	if !t.closed.CompareAndSwap(false, true) {
		return
	}
	pPostMessageW.Call(t.hwnd, msgClose, 0, 0)
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
	}
	if t.mutex != 0 {
		pCloseHandle.Call(t.mutex)
		t.mutex = 0
	}
}

// loadAppIcon 取本 exe 内嵌的应用图标（Wails 打包时写进资源的那一个）；开发态
// （go run，没有资源）退回系统默认图标。返回值第二项 = 需不需要 DestroyIcon。
func loadAppIcon() (uintptr, bool) {
	if exe, err := os.Executable(); err == nil {
		path, _ := syscall.UTF16PtrFromString(exe)
		var large, small uintptr
		n, _, _ := pExtractIconExW.Call(uintptr(unsafe.Pointer(path)), 0,
			uintptr(unsafe.Pointer(&large)), uintptr(unsafe.Pointer(&small)), 1)
		if n != 0 && n != ^uintptr(0) {
			// 高 DPI 下托盘要 20~32px：小图标是 16px 的就用大图标让系统缩，免得发虚。
			size, _, _ := pGetSystemMetrics.Call(smCxSmIcon)
			pick, other := small, large
			if size > 16 || small == 0 {
				pick, other = large, small
			}
			if other != 0 {
				pDestroyIcon.Call(other)
			}
			if pick != 0 {
				return pick, true
			}
		}
	}
	h, _, _ := pLoadIconW.Call(0, idiApplication)
	return h, false
}

func truncateTip(s string) string {
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:119]) + "…"
	}
	return s
}

func copyUTF16(dst []uint16, s string) {
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}
