package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/hls"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// newHLSClient 在已鉴权服务端基础上，额外暴露带响应头的 GET（校验分片 Content-Type/Range）。
func newHLSClient(t *testing.T) (*httptest.Server, store.Store, func(path string) (int, http.Header, string)) {
	t.Helper()
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	ts := httptest.NewServer(New(st, cfg).Handler())
	t.Cleanup(ts.Close)
	get := func(path string) (int, http.Header, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("req %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(b)
	}
	return ts, st, get
}

// TestHLS_RemuxPipeline 全流程验证 HLS 按需切片：
// 造一个 h264+aac 的 MKV（浏览器原生不认容器）→ 经 /api/play/hls 触发切片 →
// 拉取 index.m3u8（分片已注入 token）→ 拉取首个分片（静态文件、video/mp2t、可 Range）。
func TestHLS_RemuxPipeline(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不可用，跳过 HLS 集成测试")
	}
	ts, st, get := newHLSClient(t)

	// 1) h264+aac 的 MKV（与 remux 测试同款源）。
	mkv := filepath.Join(t.TempDir(), "sample.mkv")
	gen := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=3:size=320x180:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-c:v", "libopenh264", "-c:a", "aac", "-pix_fmt", "yuv420p", mkv)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成 mkv 失败: %v (%s)", err, out)
	}

	// 2) 本地媒体服务器 + 登记为存储源（放行内网拉取，复用 remux 测试做法）。
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, mkv)
	}))
	t.Cleanup(media.Close)
	_ = st.SaveStorage(model.Storage{ID: "st1", Type: model.StorageOpenList, BaseURL: media.URL})

	src := media.URL + "/sample.mkv"
	key := hls.KeyFor(src, "remux", -1)
	playURL := "/api/play/hls/" + hls.PlaylistName + "?u=" + url.QueryEscape(src) + "&mode=remux&token=tok"

	// 3) 拉取索引（处理器内部会启动 ffmpeg 切片并等待索引落盘）。
	code, _, body := get(playURL)
	if code != http.StatusOK {
		t.Fatalf("索引期望 200，得到 %d (body=%s)", code, body)
	}
	if !strings.Contains(body, "seg_00000.ts?key="+key) {
		t.Fatalf("索引未正确注入 key 或缺少首个分片：\n%s", body)
	}
	if !strings.Contains(body, "&token=tok") {
		t.Fatalf("索引分片未注入 token：\n%s", body)
	}
	if !strings.Contains(body, "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Errorf("索引缺少独立分片标记：\n%s", body)
	}

	// 4) 拉取首个分片：静态文件、video/mp2t、可 Range。
	segURL := "/api/play/hls/seg/seg_00000.ts?key=" + key + "&token=tok"
	scode, sheader, sbody := get(segURL)
	if scode != http.StatusOK {
		t.Fatalf("分片期望 200，得到 %d", scode)
	}
	if ct := sheader.Get("Content-Type"); ct != "video/mp2t" {
		t.Errorf("分片 Content-Type 应为 video/mp2t，得到 %q", ct)
	}
	if sheader.Get("Accept-Ranges") != "bytes" {
		t.Errorf("分片应声明 Accept-Ranges: bytes")
	}
	if len(sbody) < 1000 {
		t.Errorf("分片过小（%d 字节），可能不是合法 TS", len(sbody))
	}

	// 5) 无 token 且无 Authorization 头 的分片请求应被拒（鉴权兜底）。
	rawReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/play/hls/seg/seg_00000.ts?key="+key, nil)
	rawResp, err := http.DefaultClient.Do(rawReq)
	if err != nil {
		t.Fatalf("no-token req: %v", err)
	}
	rawResp.Body.Close()
	if rawResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("无 token 分片应 401，得到 %d", rawResp.StatusCode)
	}
}

// TestPlay_HLS_DeliversHlsURL 验证默认开启 HLS 时，playItem 对 L2 重封装文件下发的是
// HLS 索引 URL（而非旧的单 MP4 流 remux URL）。这是 HLS 作为 L2/L3 交付层的端到端接线验证。
func TestPlay_HLS_DeliversHlsURL(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不可用，跳过")
	}
	_, st, get := newHLSClient(t) // 默认开启 HLS（未设 VIDRIVE_HLS=0）

	// 本地媒体服务器 + 登记为存储源（放行内网拉取）。
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, filepath.Join(t.TempDir(), "sample.mkv"))
	}))
	t.Cleanup(media.Close)
	_ = st.SaveStorage(model.Storage{ID: "st1", Type: model.StorageOpenList, BaseURL: media.URL})

	// http 直链型 strm（Source=SrcStrm）：playItem 解析出 http 源 → L2 重封装 → HLS 交付。
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fhls", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: media.URL + "/sample.mkv",
	})

	code, _, body := get("/api/items/fhls/play")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level int    `json:"level"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if d.Level != 2 {
		t.Fatalf("应 L2 重封装，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/hls/") {
		t.Fatalf("HLS 默认开启时，L2 应下发 HLS 索引 URL，得到 %q", d.URL)
	}
	if !strings.Contains(d.URL, "mode=remux") {
		t.Fatalf("HLS URL 应带 mode=remux，得到 %q", d.URL)
	}
	// HLS 索引 URL 必须经 ?token= 兜底鉴权（<video> 拉不到 Authorization 头）。
	if !strings.Contains(d.URL, "token=") {
		t.Fatalf("HLS 索引 URL 应带 token，得到 %q", d.URL)
	}
}
