package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// ── 可回退的版本列表（Vantaloom/Vantaloom#119）──────────────────────────────

// MinRollbackVersion 是壳愿意装的最旧运行时。更早的版本：VERSION 里写的是 git
// hash（0.13.5 之前）、还带要提权安装的旧 mesh 服务（0.13.0 之前）、应用没有 iframe
// 宿主下的应用内标题栏（0.13.4 之前）——这一版的壳跑不好它们，列表里不给。
const MinRollbackVersion = "0.13.5"

// RuntimeVersion 是版本列表的一项。
type RuntimeVersion struct {
	Version     string `json:"version"`
	PublishedAt string `json:"publishedAt"` // RFC3339；registry 没给时为空
	Current     bool   `json:"current"`
	Latest      bool   `json:"latest"`
}

// ListVersions 从 registry 拉本平台运行时包的全部版本（带发布时间），过滤到
// MinRollbackVersion 及以上的正式版，按版本倒序。
//
// 用完整的 packument（不是 install-v1 缩略版）：缩略版不带 time，而用户挑「回到
// 哪一版」时最需要的就是「哪天发的」。完整版约 0.5MB，只在打开对话框时拉一次。
func (m *Manager) ListVersions(ctx context.Context) ([]RuntimeVersion, error) {
	metaURL := m.Registry + "/" + strings.Replace(RuntimePackageName(), "/", "%2f", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vantaloom-desktop")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上 %s：%w", m.Registry, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("查询版本列表失败：HTTP %d", resp.StatusCode)
	}
	var meta struct {
		DistTags map[string]string          `json:"dist-tags"`
		Versions map[string]json.RawMessage `json:"versions"`
		Time     map[string]string          `json:"time"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("解析版本列表失败：%w", err)
	}
	names := make([]string, 0, len(meta.Versions))
	for v := range meta.Versions {
		names = append(names, v)
	}
	return filterVersions(names, meta.Time, meta.DistTags["latest"], m.InstalledVersion()), nil
}

// filterVersions：只留 X.Y.Z 正式版（不要预发布）、不低于下限，按版本倒序。
func filterVersions(names []string, times map[string]string, latest, current string) []RuntimeVersion {
	out := make([]RuntimeVersion, 0, len(names))
	for _, v := range names {
		if strings.ContainsAny(v, "-+") || !isSemver(v) {
			continue
		}
		if semverLess(v, MinRollbackVersion) {
			continue
		}
		out = append(out, RuntimeVersion{
			Version:     v,
			PublishedAt: times[v],
			Current:     v == current,
			Latest:      v == latest,
		})
	}
	sort.Slice(out, func(i, j int) bool { return semverLess(out[j].Version, out[i].Version) })
	return out
}
