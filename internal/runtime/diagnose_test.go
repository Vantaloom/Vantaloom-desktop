package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeErrLog(t *testing.T, m *Manager, content string, appendMode bool) {
	t.Helper()
	if err := os.MkdirAll(m.LogDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(m.APIErrLogPath(), flags, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

// 老日志里躺着的 fatal 不算——只看这次中断新增的内容（#119）。
func TestCrashSinceOnlyLooksAtNewBytes(t *testing.T) {
	m := New(t.TempDir(), "")
	writeErrLog(t, m, "2026/08/18 16:35:05 [relay] 重试\nruntime: goroutine stack exceeds 1000000000-byte limit\nfatal error: stack overflow\n\ngoroutine 1 [running]:\n", false)
	writeErrLog(t, m, "2026/10/14 20:00:00 [open] conversation=x\n", true)
	offset := m.APIErrLogSize()

	if c := m.CrashSince(offset); c != nil {
		t.Fatalf("偏移之后没有新崩溃，却判成了崩溃：%+v", c)
	}
	// 一次普通重启：只有正常日志。
	writeErrLog(t, m, "2026/10/14 20:05:00 [memory] Go 运行时内存软上限：4.0 GB\n", true)
	if c := m.CrashSince(offset); c != nil {
		t.Fatalf("普通重启不该判成崩溃：%+v", c)
	}
	// 这次真的崩了。
	writeErrLog(t, m, "2026/10/14 20:51:49 [hubconn] peer connected\npanic: runtime error: invalid memory address or nil pointer dereference\n[signal 0xc0000005 code=0x0 addr=0x0 pc=0x0]\n\ngoroutine 77 [running]:\nmain.main()\n", true)
	c := m.CrashSince(offset)
	if c == nil {
		t.Fatal("新增内容里有 panic，应判成崩溃")
	}
	if !strings.HasPrefix(c.Reason, "panic: runtime error") {
		t.Fatalf("原因应是 panic 那一行，得到 %q", c.Reason)
	}
	if c.At != "2026/10/14 20:51:49" {
		t.Fatalf("时间应取崩溃前最后一条带时间戳的行，得到 %q", c.At)
	}
	if c.LogPath != m.APIErrLogPath() {
		t.Fatalf("LogPath = %q", c.LogPath)
	}
}

func TestCrashSincePrefersFatalLineAndHandlesTruncation(t *testing.T) {
	m := New(t.TempDir(), "")
	writeErrLog(t, m, strings.Repeat("2026/10/14 20:00:00 [x] filler\n", 200), false)
	offset := m.APIErrLogSize()
	// 日志被换掉（比偏移还短）：从头读。
	writeErrLog(t, m, "runtime: goroutine stack exceeds 1000000000-byte limit\nfatal error: stack overflow\n", false)
	c := m.CrashSince(offset)
	if c == nil || c.Reason != "fatal error: stack overflow" {
		t.Fatalf("应取 fatal error 那一行，得到 %+v", c)
	}
}

func TestSelfUpdateInstallingNeedsFreshInstallingPhase(t *testing.T) {
	dir := t.TempDir()
	m := New(dir, "")
	if ok, _ := m.SelfUpdateInstalling(10 * time.Minute); ok {
		t.Fatal("没有 self-update.json（旧运行时）应当是没在装")
	}
	cfg := filepath.Join(dir, "config")
	_ = os.MkdirAll(cfg, 0o755)
	p := filepath.Join(cfg, "self-update.json")
	_ = os.WriteFile(p, []byte(`{"phase":"installing","stagedVersion":"0.18.3"}`), 0o644)
	if ok, v := m.SelfUpdateInstalling(10 * time.Minute); !ok || v != "0.18.3" {
		t.Fatalf("刚写的 installing 应当是在装 0.18.3，得到 %v %q", ok, v)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	if ok, _ := m.SelfUpdateInstalling(10 * time.Minute); ok {
		t.Fatal("一小时前留下的 installing 是没收场的残留，不能当成正在装")
	}
}

func TestFilterVersionsFloorPrereleaseAndOrder(t *testing.T) {
	names := []string{"0.1.5", "0.13.4", "0.13.5", "0.17.16", "0.18.0", "0.18.1", "0.18.2", "0.19.0-beta.1", "dev", "0.9.10"}
	times := map[string]string{"0.18.1": "2026-10-13T14:18:33.479Z"}
	got := filterVersions(names, times, "0.18.2", "0.18.1")
	var vs []string
	for _, v := range got {
		vs = append(vs, v.Version)
	}
	want := "0.18.2 0.18.1 0.18.0 0.17.16 0.13.5"
	if strings.Join(vs, " ") != want {
		t.Fatalf("got %v, want %s", vs, want)
	}
	if !got[0].Latest || got[0].Current || !got[1].Current || got[1].PublishedAt == "" {
		t.Fatalf("标记不对：%+v", got[:2])
	}
}
