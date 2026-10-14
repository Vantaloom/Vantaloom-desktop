package runtime

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ── 后端为什么没了（Vantaloom/Vantaloom#119）────────────────────────────────
//
// 壳以前对**任何**健康中断都说「正在更新」——它看不见真正的更新，而绝大多数中断
// 其实是后端崩了。这里给出两条**可观测**的判据，让壳只说它真知道的事：
//
//   - CrashSince：api.err.log 在这次中断里**新增**的内容里有没有 Go 的崩溃签名
//     （panic / fatal error / 栈溢出）。只看偏移之后追加的字节：老日志里本来就躺着
//     的 fatal（0.16.18 那次 stack overflow 至今还在 live 日志里）不能把一次普通的
//     重启判成崩溃。
//   - SelfUpdateInstalling：运行时自己的后台更新在 spawn 安装进程之前把
//     <install>/config/self-update.json 的 phase 写成 installing——这时候说「正在
//     安装更新」是真话。旧运行时（< 0.17.0）没有这个文件，按「没在装」处理。

// CrashInfo 描述一次被日志证实的崩溃。
type CrashInfo struct {
	// Reason 是崩溃签名那一行（如 "fatal error: stack overflow"）。
	Reason string `json:"reason"`
	// At 是崩溃前最后一条带时间戳的日志行的时间（"2026/10/14 20:51:49"）；拿不到
	// 时用日志文件的修改时间。
	At string `json:"at"`
	// LogPath 是那份日志的完整路径（界面上「打开日志文件夹」用它的目录）。
	LogPath string `json:"logPath"`
}

// crashScanCap：最多往回读这么多字节。崩溃的 goroutine dump 可以很长（几百 KB），
// 但签名行在 dump 的**开头**——从偏移读起、读到这么多足够了。
const crashScanCap = 4 << 20

var (
	crashSignature = regexp.MustCompile(`^(panic: |fatal error: |runtime: goroutine stack exceeds|\[signal |Exception 0x)`)
	// 主签名优先：fatal error / panic 才是给人看的那句，「runtime: …」与「[signal」
	// 是它前后的细节。
	primarySignature = regexp.MustCompile(`^(panic: |fatal error: )`)
	logTimestamp     = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)
)

// LogDir 是运行时的日志目录（<install>/logs）。
func (m *Manager) LogDir() string { return filepath.Join(m.Prefix, "logs") }

// APIErrLogPath 是 vantaloom-api 的 stderr 日志（Go 的 panic / fatal 写在这里）。
// 0.13.5 起的 vantaloomctl 都用这个名字（已核对）。
func (m *Manager) APIErrLogPath() string { return filepath.Join(m.LogDir(), "api.err.log") }

// APIErrLogSize 返回 api.err.log 当前大小；不存在为 0。
func (m *Manager) APIErrLogSize() int64 {
	fi, err := os.Stat(m.APIErrLogPath())
	if err != nil {
		return 0
	}
	return fi.Size()
}

// CrashSince 检查 api.err.log 在 offset 之后新增的内容里有没有崩溃签名。日志被
// 截短 / 换过（大小小于 offset）时从头读。没有证据返回 nil——**宁可说「未响应」，
// 也不把一次普通中断说成崩溃**。
func (m *Manager) CrashSince(offset int64) *CrashInfo {
	path := m.APIErrLogPath()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	if offset < 0 || fi.Size() < offset {
		offset = 0
	}
	if fi.Size()-offset > crashScanCap {
		// 新增内容太多：签名在 dump 开头，而 dump 在中断发生的地方——那更可能在
		// 末尾一段里。取末尾 crashScanCap。
		offset = fi.Size() - crashScanCap
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	info := scanCrash(io.LimitReader(f, crashScanCap))
	if info == nil {
		return nil
	}
	info.LogPath = path
	if info.At == "" {
		info.At = fi.ModTime().Format("2006/01/02 15:04:05")
	}
	return info
}

// scanCrash 在一段日志里找崩溃签名，返回签名行与它之前最后一个时间戳。
func scanCrash(r io.Reader) *CrashInfo {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	lastStamp := ""
	var fallback *CrashInfo
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if m := logTimestamp.FindStringSubmatch(line); m != nil {
			lastStamp = m[1]
			continue
		}
		if primarySignature.MatchString(line) {
			return &CrashInfo{Reason: clip(line, 200), At: lastStamp}
		}
		if fallback == nil && crashSignature.MatchString(line) {
			fallback = &CrashInfo{Reason: clip(line, 200), At: lastStamp}
		}
	}
	return fallback
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return string(r)
}

// SelfUpdateInstalling 报告运行时自己的后台更新此刻是不是正在安装（phase=
// installing，且是 freshFor 之内写下的——安装要么几分钟内完成要么失败，更早的
// installing 是没收场的残留，不能当成「正在装」）。
func (m *Manager) SelfUpdateInstalling(freshFor time.Duration) (bool, string) {
	path := filepath.Join(m.Prefix, "config", "self-update.json")
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) > freshFor {
		return false, ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, ""
	}
	var st struct {
		Phase         string `json:"phase"`
		StagedVersion string `json:"stagedVersion"`
	}
	if json.Unmarshal(bytes.TrimSpace(b), &st) != nil {
		return false, ""
	}
	return st.Phase == "installing", strings.TrimSpace(st.StagedVersion)
}
