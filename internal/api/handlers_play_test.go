package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newmovie/internal/model"
)

// TestPlay_StrmHttpResolvesInPage 锁定 STRM 修复：http 直链型 strm 此前因 StorageID 为空，
// playItem 用 GetStorage("") 直接 400，只能甩外部播放器。修复后应解析出 http 源并走 L2 重封装页内播。
func TestPlay_StrmHttpResolvesInPage(t *testing.T) {
	st, do := featureFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fstrm", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "https://cdn.example.com/movie.mkv",
	})
	code, body := do(http.MethodGet, "/api/items/fstrm/play", "")
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
		t.Fatalf("strm http 应 L2 重封装页内播，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("strm 播放 URL 应指向 remux 端点，得到 %q", d.URL)
	}
}

// TestPlay_StrmHttpHEVC_TranscodeWhenEnabled 锁定视频转码：HEVC 的 strm 在开启
// 「允许视频转码」后应走 L3（HEVC→H.264），任何浏览器都能页内播，不再报「视频不存在」。
func TestPlay_StrmHttpHEVC_TranscodeWhenEnabled(t *testing.T) {
	st, do := featureFixture(t)
	_ = st.SaveSetting("transcode_enabled", "1")
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "fhevc", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "https://cdn.example.com/movie-hevc.mkv",
		VideoCodec: "hevc", AudioCodec: "aac",
	})
	code, body := do(http.MethodGet, "/api/items/fhevc/play", "")
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
	if d.Level != 3 && d.Level != 2 {
		t.Fatalf("HEVC+转码应页内播放（L3 转码或 L2 重封装保留 HEVC），得到 level=%d (%s)", d.Level, body)
	}
	if d.Level == 3 && !strings.Contains(d.URL, "/api/play/transcode?u=") {
		t.Fatalf("HEVC 转码 URL 应指向 transcode 端点，得到 %q", d.URL)
	}
	if d.Level == 2 && !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("HEVC 重封装 URL 应指向 remux 端点，得到 %q", d.URL)
	}
}

// mockOpenList 起一个最小 OpenList /api/fs/get 模拟服务：仅接受 accept 路径返回
// 真实直链，其余返回 code!=200 模拟「文件不存在」。用于验证本地路径型 strm 的
// 精确映射与逐级探测兜底。
func mockOpenListFS(t *testing.T, accept string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Path string `json:"path"`
		}
		_ = json.NewDecoder(r.Body).Decode(&q)
		if q.Path == accept {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 200,
				"data": map[string]any{"raw_url": "https://ol.example.com/file" + accept, "url": "https://ol.example.com/d" + accept},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "message": "object not found"})
	}))
}

// TestPlay_StrmLocalPath_MapsViaLocalRoot 锁定源头修复：本地挂载路径型 strm
// （/mnt/cloud/媒体/A.mkv）+ 存储配了 LocalRoot。resolver 剥离挂载前缀映射成
// 存储内部路径 /媒体/A.mkv，playItem 直接取链，走 L2 页内播放，不再需要任何
// 全局白名单 / 改 strm / 路径重写。
func TestPlay_StrmLocalPath_MapsViaLocalRoot(t *testing.T) {
	st, do := featureFixture(t)
	ol := mockOpenListFS(t, "/媒体/A.mkv")
	defer ol.Close()
	_ = st.SaveStorage(model.Storage{ID: "ol1", Name: "ol", Type: model.StorageOpenList, BaseURL: ol.URL, Token: "t", LocalRoot: "/mnt/cloud"})
	_ = st.SaveMediaFile(model.MediaFile{ID: "flocal", ItemID: "m1", Source: model.SrcStrm, StrmRaw: "/mnt/cloud/媒体/A.mkv"})

	code, body := do(http.MethodGet, "/api/items/flocal/play", "")
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
		t.Fatalf("本地路径 strm 应 L2 页内播，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("应指向 remux 端点，得到 %q", d.URL)
	}
	// remux 的 u 参数应是映射后的内部路径 /媒体/A.mkv（URL 编码为 %2F%E5%AA%92%E4%BD%93%2FA.mkv）
	if !strings.Contains(d.URL, "%E5%AA%92%E4%BD%93") {
		t.Fatalf("remux 的 u 应含映射后的内部路径「媒体」，得到 %q", d.URL)
	}
}

// TestPlay_StrmLocalPath_ProbeFallback 锁定零配置兜底：本地路径型 strm 但存储
// 没配 LocalRoot。resolver 把整路径当内部路径兜底，playItem 的 resolveOpenListLink
// 逐级剥前缀（/mnt/cloud/媒体/A.mkv → /cloud/媒体/A.mkv → /媒体/A.mkv）回退探测，
// 命中即页内播——用户无需任何配置。
func TestPlay_StrmLocalPath_ProbeFallback(t *testing.T) {
	st, do := featureFixture(t)
	ol := mockOpenListFS(t, "/媒体/A.mkv")
	defer ol.Close()
	_ = st.SaveStorage(model.Storage{ID: "ol1", Name: "ol", Type: model.StorageOpenList, BaseURL: ol.URL, Token: "t"})
	_ = st.SaveMediaFile(model.MediaFile{ID: "flocal2", ItemID: "m1", Source: model.SrcStrm, StrmRaw: "/mnt/cloud/媒体/A.mkv"})

	code, body := do(http.MethodGet, "/api/items/flocal2/play", "")
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
		t.Fatalf("零配置兜底应仍能 L2 页内播，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("应指向 remux 端点，得到 %q", d.URL)
	}
}

// TestPlay_StrmOpenListDLink_FallbackWhenResolveFails 锁定「.cas / /d/ 直链」源头修复：
// STRM 文本本身是 OpenList 的 /d/...?sign= 中转直链（CloudDrive2 的 .cas 经 OpenList
// 取链即返回此形态，自身 302 即跳真实网盘解密流）。当内部路径取链失败（路径编码对不上、
// 模板占位符、或签名态异常）时，playItem 应回退用原始 StrmRaw 直链继续 L2 重封装页内播，
// 而不是 502「网盘直链为空」。这正是真实环境里部分 .cas 播不了的根因修复。
func TestPlay_StrmOpenListDLink_FallbackWhenResolveFails(t *testing.T) {
	st, do := featureFixture(t)
	// mock 对任何路径都返回 500（模拟取链失败），逼出回退路径。
	ol := mockOpenListFS(t, "__never_match__")
	defer ol.Close()
	_ = st.SaveStorage(model.Storage{ID: "ol1", Name: "ol", Type: model.StorageOpenList, BaseURL: ol.URL, Token: "t"})
	strmRaw := ol.URL + "/d/%E5%BD%B1%E8%A7%86/foo.mkv?sign=xyz"
	_ = st.SaveMediaFile(model.MediaFile{ID: "fcas", ItemID: "m1", Source: model.SrcStrm, StrmRaw: strmRaw,
		ProbeState: "done", Container: "mkv", VideoCodec: "h264", AudioCodec: "aac"})

	code, body := do(http.MethodGet, "/api/items/fcas/play", "")
	if code != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d (%s)", code, body)
	}
	var d struct {
		Level  int    `json:"level"`
		URL    string `json:"url"`
		RawURL string `json:"raw_url"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析: %v (%s)", err, body)
	}
	if d.Level != 2 {
		t.Fatalf(".cas /d/ 直链回退应 L2 重封装页内播，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("应指向 remux 端点，得到 %q", d.URL)
	}
	// 回退后用原始 StrmRaw 作为源，不再 502。
	if d.RawURL != strmRaw {
		t.Fatalf("回退源应是原始 StrmRaw 直链，得到 raw_url=%q，期望 %q", d.RawURL, strmRaw)
	}
}
