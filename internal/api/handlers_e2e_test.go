package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// TestE2E_ConnectLibraryPlay_RealFFmpeg 是「从连接 OpenList 到建媒体库到点击实际播放」的
// 端到端验证，跑的是 NewMovie 真实的全部代码链路：
//   1) 连接 OpenList（POST /api/storages，鉴权 + 存盘）
//   2) 建媒体库（POST /api/libraries，混合模式，指向 /movies）
//   3) 触发扫描（GET .../scan）-> 扫描器读 .cas 指针文件 -> IngestStrm 落库
//   4) 点击播放（GET /api/items/<fileID>/play）-> playItem 决策 L2 重封装
//   5) 真去拉源 + 真 ffmpeg -c copy 重封装成 MP4 流式回写
// 其中 OpenList 后端是本地伪造（无法连你真实网盘），但「媒体源」是一个用 ffmpeg 真实生成的
// mpeg4+aac MKV，重封装走的是真实 ffmpeg 管线——所以「建库→播放→出 MP4」这一段是 100% 真链路。
// （针对你真实 OpenList + 真实 .cas 的等价验证见 handlers_live_test.go，需设 LIVE_OPENLIST_TOKEN。）
func TestE2E_ConnectLibraryPlay_RealFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("环境无 ffmpeg，跳过真实 E2E")
	}

	// 1) 生成真实媒体文件：AV1(视频, 浏览器可解) + aac(音频) 的 MKV。
	//    AV1 在 Chrome/Firefox 原生可解，且可被 -c copy 重封装进 MP4 —— 正好走 L2 重封装，
	//    用来端到端验证「MKV → 真实 ffmpeg remux → 浏览器可播 MP4」全链路。
	dir := t.TempDir()
	mkv := filepath.Join(dir, "movie.mkv")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=80x60:rate=8:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-c:v", "libaom-av1", "-cpu-used", "8", "-crf", "32", "-c:a", "aac", "-y", mkv)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg 生成测试片失败（环境限制）: %v %s", err, out)
	}

	// 2) 媒体源服务：真实 MKV 文件，支持 Range（贴近真实网盘直链）。
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, mkv)
	}))
	defer media.Close()

	// 3) 伪造 OpenList：实现 /api/fs/list、/api/fs/get、/cas-content。
	//    .cas 内容是一个指向本 fake OpenList 的 /d/ 直链（模拟 CloudDrive2 .cas 经 OpenList 取链的形态）。
	var ol *httptest.Server
	ol = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/list":
			var b struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.Path == "/" || b.Path == "/movies" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code": 200, "message": "",
					"data": map[string]any{"content": []map[string]any{
						{"name": "盗梦空间.2010.cas", "size": 100, "is_dir": false, "modified": 1},
					}, "total": 1},
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"content": []any{}, "total": 0}})
			}
		case r.URL.Path == "/api/fs/get":
			var b struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			switch b.Path {
			case "/movies/盗梦空间.2010.cas":
				// .cas 指针文件内容：指向本 fake OpenList 的 /d/ 直链
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{
					"raw_url": ol.URL + "/cas-content", "url": "", "sign": "",
				}})
			case "/movies/foo.mkv":
				// 内部路径取链：返回真实媒体源直链（模拟网盘真实直链）
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{
					"raw_url": media.URL + "/movie.mkv", "url": "", "sign": "",
				}})
			default:
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "message": "not found"})
			}
		case r.URL.Path == "/cas-content":
			// 返回 .cas 文件内容：一个 /d/ 直链
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(ol.URL + "/d/movies/foo.mkv?sign=x"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ol.Close()

	// 4) 起 NewMovie 服务（自带 ffmpeg 能力探测），预置管理员并持有服务地址。
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	_ = st.SaveUser(model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true})
	_ = st.UpsertToken("u1", "tok")
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	// 关掉 HLS 以锁定 remux URL 旧断言（HLS 交付单独测）。
	t.Setenv("VIDRIVE_HLS", "0")
	ts := httptest.NewServer(New(st, cfg).Handler())
	defer ts.Close()

	do := func(method, path, body string) (int, string) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, rd)
		req.Header.Set("Authorization", "Bearer tok")
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatalf("req %s %s: %v", method, path, e)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 5) 连接 OpenList（真实 HTTP 端点）
	code, body := do(http.MethodPost, "/api/storages",
		`{"base_url":`+mustJSON(ol.URL)+`,"token":"t","type":"openlist","name":"ol"}`)
	if code != http.StatusOK {
		t.Fatalf("连接 OpenList 失败 code=%d %s", code, body)
	}
	var stResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &stResp); err != nil || stResp.ID == "" {
		t.Fatalf("解析存储响应失败: %v (%s)", err, body)
	}

	// 6) 建媒体库（混合模式，指向 /movies）
	code, body = do(http.MethodPost, "/api/libraries",
		`{"name":"电影库","mode":"mixed","storage_id":`+mustJSON(stResp.ID)+`,"root_path":"/movies"}`)
	if code != http.StatusOK {
		t.Fatalf("建媒体库失败 code=%d %s", code, body)
	}
	var libResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &libResp); err != nil || libResp.ID == "" {
		t.Fatalf("解析媒体库响应失败: %v (%s)", err, body)
	}

	// 7) 触发扫描（异步），轮询直到条目入库。
	code, body = do(http.MethodPost, "/api/libraries/"+libResp.ID+"/scan", "")
	if code != http.StatusOK {
		t.Fatalf("触发扫描失败 code=%d %s", code, body)
	}
	var itemID string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := st.ListMediaItems(libResp.ID)
		if len(items) > 0 {
			itemID = items[0].ID
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if itemID == "" {
		t.Fatalf("扫描未在超时内入库（电影库=%s）", libResp.ID)
	}
	files, _ := st.ListMediaFiles(itemID)
	if len(files) != 1 {
		t.Fatalf("媒体文件数 = %d, want 1", len(files))
	}
	if files[0].Source != model.SrcStrm {
		t.Fatalf("源应为 strm（.cas 按 strm 处理），得到 %q", files[0].Source)
	}

	// 8) 点击实际播放：GET /api/items/<fileID>/play
	code, body = do(http.MethodGet, "/api/items/"+files[0].ID+"/play", "")
	if code != http.StatusOK {
		t.Fatalf("播放决策失败 code=%d %s", code, body)
	}
	var d struct {
		Level    int    `json:"level"`
		URL      string `json:"url"`
		RawURL   string `json:"raw_url"`
		FFmpegOK bool   `json:"ffmpeg_ok"`
	}
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatalf("解析播放决策: %v (%s)", err, body)
	}
	if d.Level != 2 {
		t.Fatalf("期望 L2 重封装页内播，得到 level=%d (%s)", d.Level, body)
	}
	if !strings.Contains(d.URL, "/api/play/remux?u=") {
		t.Fatalf("播放 URL 应指向 remux 端点，得到 %q", d.URL)
	}
	t.Logf("播放决策: level=%d ffmpeg_ok=%v url=%s", d.Level, d.FFmpegOK, d.URL)

	// 9) 真去拉源 + 真 ffmpeg 重封装：GET remux URL（需鉴权），验证返回 200 + video/mp4 + ftyp。
	req, _ := http.NewRequest(http.MethodGet, ts.URL+d.URL, nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 remux 端点: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remux 期望 200，得到 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "video/mp4") {
		t.Fatalf("期望 video/mp4，得到 %q", ct)
	}
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	// 合法 MP4 以 ftyp box 开头：前 4 字节是 box 长度，第 4–8 字节是 "ftyp" 类型标识。
	if n < 12 || string(buf[4:8]) != "ftyp" {
		t.Fatalf("返回内容不是合法 MP4（缺少 ftyp 头）；前 %d 字节=%x", n, buf[:n])
	}
	t.Logf("✅ 端到端通过：连接 OpenList → 建库 → 扫描(.cas) → 点击播放 → 真实 ffmpeg 重封装出 MP4（ftyp 校验通过）")
}
