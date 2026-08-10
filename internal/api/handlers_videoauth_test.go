package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// 这一组测试守的是一个非常容易被「测试通过但用户播不了」蒙混过去的坑：
//
// 浏览器的 <video src="..."> / <track src="..."> 是浏览器自己发起的请求，
// **没有任何办法附加 Authorization 头**。而 /api/play/remux、/transcode、/subtitle
// 都要鉴权。所以哪怕 Go 写的 E2E 全绿（因为它手动 set 了 header），
// 真人点播放依然是 401 黑屏。
//
// 修法：playItem 下发 URL 时就把 ?token= 带上（后端 getToken 本就支持）。
// 下面的测试一律**不带 Authorization 头**，模拟真实浏览器行为。

func videoAuthFixture(t *testing.T) (store.Store, *httptest.Server) {
	t.Helper()
	// 关掉 HLS 以锁定 remux URL 旧断言（HLS 交付单独测）。
	t.Setenv("VIDRIVE_HLS", "0")
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	_ = st.SaveLibrary(model.Library{ID: "lib1", Name: "电影库"})
	_ = st.SaveMediaItem(model.MediaItem{ID: "m1", LibraryID: "lib1", Kind: model.KindMovie, Title: "测试片"})

	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	ts := httptest.NewServer(New(st, cfg).Handler())
	t.Cleanup(ts.Close)
	return st, ts
}

// playDecision 取播放决策（这一步是 XHR，前端会带 header，所以这里也带）。
func playDecision(t *testing.T, ts *httptest.Server, fileID string) map[string]interface{} {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/items/"+fileID+"/play", nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("play: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("play 期望 200，得到 %d: %s", resp.StatusCode, b)
	}
	var d map[string]interface{}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("解析决策失败: %v (%s)", err, b)
	}
	return d
}

// getNoAuth 完全模拟浏览器：不带任何鉴权头。
func getNoAuth(t *testing.T, rawurl string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, rawurl, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no-auth GET: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestPlayURL_CarriesTokenForBrowserVideoTag 是本组的核心断言：
// L2 重封装下发的 URL 必须自带 token，否则 <video> 拉不动。
func TestPlayURL_CarriesTokenForBrowserVideoTag(t *testing.T) {
	st, ts := videoAuthFixture(t)
	// MKV + h264/aac → 决策必然是 L2 重封装，URL 指向 /api/play/remux。
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "f1", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "https://example.invalid/a.mkv", ProbeState: "done",
		VideoCodec: "h264", AudioCodec: "aac", Container: "mkv",
	})

	d := playDecision(t, ts, "f1")
	if lv, _ := d["level"].(float64); int(lv) != 2 {
		t.Fatalf("期望 L2 重封装，得到 level=%v", d["level"])
	}
	u, _ := d["url"].(string)
	if !strings.Contains(u, "/api/play/remux?") {
		t.Fatalf("URL 应指向 remux 端点: %q", u)
	}
	if !strings.Contains(u, "token=") {
		t.Fatalf("播放 URL 缺少 token —— 浏览器 <video> 会 401 黑屏。URL=%q", u)
	}
}

// TestPlayURL_NoAuthRequestIsNotUnauthorized 端到端确认：
// 拿着下发的 URL、不带 header 直接请求，不应该是 401。
//
// 上游 example.invalid 拉不通，所以预期是 5xx（取源失败），
// 但**绝不能是 401** —— 401 意味着鉴权这关就没过去。
func TestPlayURL_NoAuthRequestIsNotUnauthorized(t *testing.T) {
	st, ts := videoAuthFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "f1", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "https://example.invalid/a.mkv", ProbeState: "done",
		VideoCodec: "h264", AudioCodec: "aac", Container: "mkv",
	})

	d := playDecision(t, ts, "f1")
	u, _ := d["url"].(string)

	if code := getNoAuth(t, ts.URL+u); code == http.StatusUnauthorized {
		t.Fatalf("浏览器式（无 header）请求返回 401 —— 页内播放会黑屏。URL=%q", u)
	}
}

// TestPlayURL_WithoutTokenStillRejected 反向确认：把 token 去掉就该 401。
// 免得哪天有人「顺手」把 remux 端点改成公开的，那才是真的出事。
func TestPlayURL_WithoutTokenStillRejected(t *testing.T) {
	st, ts := videoAuthFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "f1", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "https://example.invalid/a.mkv", ProbeState: "done",
		VideoCodec: "h264", AudioCodec: "aac", Container: "mkv",
	})

	d := playDecision(t, ts, "f1")
	u, _ := d["url"].(string)
	// 剥掉 token 参数
	if i := strings.Index(u, "&token="); i >= 0 {
		u = u[:i]
	}
	if code := getNoAuth(t, ts.URL+u); code != http.StatusUnauthorized {
		t.Fatalf("去掉 token 后应当 401（端点不能变成公开的），得到 %d", code)
	}
}

// TestSubtitleURL_CarriesToken 字幕同理：ArtPlayer 加载字幕也带不上 header。
func TestSubtitleURL_CarriesToken(t *testing.T) {
	st, ts := videoAuthFixture(t)
	_ = st.SaveMediaFile(model.MediaFile{
		ID: "f1", ItemID: "m1", Source: model.SrcStrm,
		StrmRaw: "https://example.invalid/a.mkv", ProbeState: "done",
		VideoCodec: "h264", AudioCodec: "aac", Container: "mkv",
		Subtitles: []model.Subtitle{{Lang: "chi", Title: "简体中文"}},
	})

	d := playDecision(t, ts, "f1")
	subs, _ := d["subtitles"].([]interface{})
	if len(subs) == 0 {
		t.Fatal("期望有字幕轨")
	}
	m, _ := subs[0].(map[string]interface{})
	su, _ := m["url"].(string)
	if !strings.Contains(su, "token=") {
		t.Fatalf("字幕 URL 缺少 token —— 播放器加载字幕会 401。URL=%q", su)
	}
}

// TestAppendToken 单元级：各种边界都别搞出双问号或重复 token。
func TestAppendToken(t *testing.T) {
	cases := []struct{ in, tok, want string }{
		{"/api/play/remux?u=x", "abc", "/api/play/remux?u=x&token=abc"},
		{"/api/play/remux", "abc", "/api/play/remux?token=abc"},
		{"", "abc", ""},                             // 空 URL 不动
		{"/x?u=1", "", "/x?u=1"},                    // 无 token 不动
		{"/x?token=old", "new", "/x?token=old"},     // 已有 token 不覆盖
		{"/x?u=1", "a b+c", "/x?u=1&token=a+b%2Bc"}, // 需转义
	}
	for _, c := range cases {
		if got := appendToken(c.in, c.tok); got != c.want {
			t.Errorf("appendToken(%q, %q) = %q，期望 %q", c.in, c.tok, got, c.want)
		}
	}
}
