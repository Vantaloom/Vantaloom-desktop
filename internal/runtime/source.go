package runtime

// 运行时包的两条线（主仓 Vantaloom/Vantaloom #272）。
//
// 主线 @vantaloom/runtime-<平台>：npmjs.org 上的真源，**版本以它为准**。
// 镜像线 @vantaloom/rt-<平台>：同一次发版、同一个版本号、内容只差 package.json 的名字。
// 它存在的唯一理由是 npmmirror 能同步它——主线历史上有一批解包超过 256MB 的版本，
// npmmirror 因此整包放弃同步主线（新版本一并卡死）。
//
// 下载顺序：先从 npmmirror 拿镜像线（国内快一个数量级），**按 npmjs.org 上登记的校验值
// 核对**；镜像还没同步到这一版、连不上、超时、核不上，一律退回 npmjs.org 的主线。
// 同一条约定在主仓还有两份：CLI（packages/cli/src/lib/runtime-source.mjs）与后端自更新
// （apps/api/internal/selfupdate）。改顺序或名字，三处一起改。

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"strings"
	"time"
)

const (
	// defaultMirrorRegistry 只用来搬字节：版本与校验值都从 Manager.Registry 取。
	defaultMirrorRegistry = "https://registry.npmmirror.com"
	// mirrorDownloadTimeout：镜像那一腿的上限，慢成这样不如直接换源。
	mirrorDownloadTimeout = 8 * time.Minute
)

// mirrorRegistry 是镜像线的下载源；"" = 不走镜像。
func (m *Manager) mirrorRegistry() string {
	switch v := strings.TrimRight(strings.TrimSpace(m.MirrorRegistry), "/"); v {
	case "":
		return defaultMirrorRegistry
	case "off":
		return ""
	default:
		return v
	}
}

// MirrorPackageName 是本平台运行时的镜像线包名。
func MirrorPackageName() string { return "@vantaloom/rt-" + PlatformID() }

// resolveRelease 解析这次要装的版本：Manager.Registry 上的主线是真源；它够不着才看
// 镜像线（先同一个 registry，再 npmmirror——那边可能晚几分钟，但不会更新）。
func (m *Manager) resolveRelease(ctx context.Context, version string) (resolved, error) {
	order := [][2]string{
		{m.Registry, RuntimePackageName()},
		{m.Registry, MirrorPackageName()},
	}
	if mirror := m.mirrorRegistry(); mirror != "" {
		order = append(order, [2]string{mirror, MirrorPackageName()})
	}
	var errs []string
	for _, attempt := range order {
		res, err := m.resolveFrom(ctx, attempt[0], attempt[1], version)
		if err == nil {
			if len(errs) > 0 {
				log.Printf("[runtime] 版本改由 %s @ %s 解析（%s）", attempt[1], attempt[0], errs[0])
			}
			return res, nil
		}
		errs = append(errs, err.Error())
	}
	return resolved{}, errors.New(strings.Join(errs, "；"))
}

// mirrorSource 是这一版在镜像线上的那一份：下载地址在 npmmirror，校验值优先取
// Manager.Registry 上登记的（npmjs.org），够不着才用镜像自己的。没有返回 false。
func (m *Manager) mirrorSource(ctx context.Context, version string) (resolved, bool) {
	name := MirrorPackageName()
	mirror := m.mirrorRegistry()
	if mirror == "" {
		return resolved{}, false
	}
	for _, registry := range []string{m.Registry, mirror} {
		meta, err := m.resolveFrom(ctx, registry, name, version)
		if err != nil || meta.Version != version {
			continue
		}
		meta.Registry = mirror
		meta.Tarball = mirrorTarballURL(mirror, name, version)
		meta.Mirror = true
		return meta, true
	}
	return resolved{}, false
}

// mirrorTarballURL 是 npmmirror 上一个包版本的 tarball 地址（与它元数据里写的同一个形状）。
func mirrorTarballURL(registry, name, version string) string {
	base := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		base = name[i+1:]
	}
	return strings.TrimRight(registry, "/") + "/" + name + "/-/" + base + "-" + version + ".tgz"
}

// downloadRelease 按「镜像线 → 已解析的那一份」的顺序下载到 archive 并核对，返回实际用上的那一份。
func (m *Manager) downloadRelease(ctx context.Context, res resolved, archive string, onPct func(int)) (resolved, error) {
	sources := []resolved{}
	if mirror, ok := m.mirrorSource(ctx, res.Version); ok && mirror.Tarball != res.Tarball {
		sources = append(sources, mirror)
	}
	sources = append(sources, res)

	var failures []string
	for i, src := range sources {
		attemptCtx, cancel := ctx, context.CancelFunc(func() {})
		if src.Mirror {
			attemptCtx, cancel = context.WithTimeout(ctx, mirrorDownloadTimeout)
		}
		err := m.download(attemptCtx, src.Tarball, archive, onPct)
		cancel()
		if err == nil {
			err = verifyArchive(archive, src)
		}
		if err == nil {
			return src, nil
		}
		_ = os.Remove(archive)
		failures = append(failures, fmt.Sprintf("%s: %v", src.Registry, err))
		if ctx.Err() != nil {
			break
		}
		if i < len(sources)-1 {
			log.Printf("[runtime] 从 %s 下载 %s 没成功（%v），改从 %s 下载", src.Registry, res.Version, err, sources[i+1].Registry)
		}
	}
	return resolved{}, errors.New(strings.Join(failures, "；"))
}

// verifyArchive 用 registry 登记的最强摘要核对 tarball：dist.integrity 的 sha512 / sha256
// 优先，没有就 dist.shasum（sha1）。registry 什么都没给就只记一句，不假装核过。
func verifyArchive(path string, res resolved) error {
	algo, want := pickDigest(res.Integrity, res.Shasum)
	if algo == "" {
		log.Printf("[runtime] %s 没有给出 %s 的校验值，本次未做校验", res.Registry, res.Version)
		return nil
	}
	var h hash.Hash
	switch algo {
	case "sha512":
		h = sha512.New()
	case "sha256":
		h = sha256.New()
	default:
		h = sha1.New()
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return fmt.Errorf("校验和不匹配（%s）：应为 %s，实际 %s", algo, want, got)
	}
	return nil
}

// pickDigest 返回 (算法, 十六进制摘要)；都没有返回空串。
func pickDigest(integrity, shasum string) (string, string) {
	best, bestHex := "", ""
	for _, entry := range strings.Fields(integrity) {
		algo, b64, ok := strings.Cut(entry, "-")
		if !ok {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		switch strings.ToLower(algo) {
		case "sha512":
			best, bestHex = "sha512", hex.EncodeToString(raw)
		case "sha256":
			if best != "sha512" {
				best, bestHex = "sha256", hex.EncodeToString(raw)
			}
		}
	}
	if best == "" {
		if s := strings.TrimSpace(shasum); s != "" {
			return "sha1", strings.ToLower(s)
		}
	}
	return best, bestHex
}
