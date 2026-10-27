package runtime

import (
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeNpm 回答任何包名的元数据（latest = version，tarball 指回自己）与任何 .tgz。
func fakeNpm(t *testing.T, version string, published, served []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	sum := sha512.Sum512(published)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".tgz") {
			hits.Add(1)
			_, _ = w.Write(served)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dist-tags": map[string]string{"latest": version},
			"versions": map[string]any{version: map[string]any{"dist": map[string]any{
				"tarball": srv.URL + "/pkg/-/pkg-" + version + ".tgz", "integrity": integrity,
			}}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, hits
}

// 两条线（主仓 #272）：镜像先下、按真源校验值核对；镜像被改过就退回主线，是好的就不碰主线。
func TestDownloadReleasePrefersVerifiedMirror(t *testing.T) {
	good := []byte("runtime tarball bytes")
	for _, tc := range []struct {
		name        string
		mirrorBytes []byte
		wantMain    bool
	}{
		{"镜像被改过", []byte("tampered"), true},
		{"镜像是好的", good, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			main, mainHits := fakeNpm(t, "0.23.6", good, good)
			mirror, mirrorHits := fakeNpm(t, "0.23.6", good, tc.mirrorBytes)
			m := &Manager{Registry: main.URL, MirrorRegistry: mirror.URL, HTTP: http.DefaultClient}
			ctx := context.Background()
			res, err := m.resolveRelease(ctx, "latest")
			if err != nil || res.Version != "0.23.6" {
				t.Fatalf("resolve: %v %+v", err, res)
			}
			archive := filepath.Join(t.TempDir(), "runtime.tgz")
			used, err := m.downloadRelease(ctx, res, archive, nil)
			if err != nil {
				t.Fatalf("download: %v", err)
			}
			data, _ := os.ReadFile(archive)
			if string(data) != string(good) {
				t.Fatalf("落盘的字节不对：%q", data)
			}
			if mirrorHits.Load() == 0 {
				t.Fatal("应当先试镜像")
			}
			if (mainHits.Load() > 0) != tc.wantMain || used.Mirror == tc.wantMain {
				t.Fatalf("主线被请求=%v（期望 %v），用上的是镜像=%v", mainHits.Load() > 0, tc.wantMain, used.Mirror)
			}
		})
	}
}

func TestMirrorOffAndTarballShape(t *testing.T) {
	m := &Manager{MirrorRegistry: "off"}
	if _, ok := m.mirrorSource(context.Background(), "0.23.6"); ok {
		t.Fatal("关掉镜像就不该有镜像来源")
	}
	if got := mirrorTarballURL("https://registry.npmmirror.com/", "@vantaloom/rt-win32-x64", "0.23.6"); got != "https://registry.npmmirror.com/@vantaloom/rt-win32-x64/-/rt-win32-x64-0.23.6.tgz" {
		t.Fatalf("tarball 地址形状不对：%s", got)
	}
	if algo, _ := pickDigest("", ""); algo != "" {
		t.Fatal("什么都没给就不该有算法")
	}
}
