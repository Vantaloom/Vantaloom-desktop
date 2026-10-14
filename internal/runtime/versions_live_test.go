package runtime

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// 真去 npmjs.org 拉一次版本列表（VTL_NPM_LIVE=1 才跑；CI 不联网）。
// VTL_NPM_LIVE_OUT 给了路径就把结果写成 JSON（界面探针拿它当假桥的数据）。
func TestListVersionsLive(t *testing.T) {
	if os.Getenv("VTL_NPM_LIVE") != "1" {
		t.Skip("set VTL_NPM_LIVE=1 to hit registry.npmjs.org")
	}
	m := New(t.TempDir(), "https://registry.npmjs.org")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	vs, err := m.ListVersions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) == 0 || !vs[0].Latest {
		t.Fatalf("第一项应是 latest：%+v", vs[:1])
	}
	last := vs[len(vs)-1]
	if last.Version != MinRollbackVersion {
		t.Fatalf("最后一项应是下限 %s，得到 %s", MinRollbackVersion, last.Version)
	}
	for _, v := range vs {
		if v.PublishedAt == "" {
			t.Fatalf("%s 没有发布时间", v.Version)
		}
	}
	t.Logf("%d 个版本，最新 %s，最旧 %s", len(vs), vs[0].Version, last.Version)
	if out := os.Getenv("VTL_NPM_LIVE_OUT"); out != "" {
		b, _ := json.Marshal(vs)
		_ = os.WriteFile(out, b, 0o644)
	}
}
