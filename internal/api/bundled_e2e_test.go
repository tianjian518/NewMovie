package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// TestE2E_BundledOpenList_FullChain 是 2.0 的真实端到端验证。
//
// 它启动一个**真实的 139cas 进程**，然后完整走一遍用户路径：
//
//	启动内置后端 → NewMovie 自动接管（登录换 Token、读签名密钥、注册存储）
//	→ 浏览目录 → 建媒体库 → 扫描 → 点击播放 → 真实 ffmpeg 重封装 → 校验 MP4
//
// 与 mock 测试的区别：这里的 OpenList 是真的，签名算法是真的，视频是真的，
// 重封装也是真的。只有「网盘」用本地目录代替（Local driver），因为不能在
// CI 里挂真实云盘。
//
// 需要先编译好 139cas 二进制：
//
//	cd openlist && CGO_ENABLED=0 go build -tags=jsoniter -o /tmp/openlist .
//	NEWMOVIE_E2E_OPENLIST_BIN=/tmp/openlist go test ./internal/api/ -run TestE2E_Bundled -v
//
// 未设置该变量时自动跳过，不影响常规 CI。
func TestE2E_BundledOpenList_FullChain(t *testing.T) {
	olBin := os.Getenv("NEWMOVIE_E2E_OPENLIST_BIN")
	if olBin == "" {
		t.Skip("未设置 NEWMOVIE_E2E_OPENLIST_BIN，跳过真实双进程 E2E")
	}
	if _, err := os.Stat(olBin); err != nil {
		t.Skipf("139cas 二进制不存在: %v", err)
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("缺少 ffmpeg，跳过")
	}

	const adminPass = "E2ePass2024"
	workDir := t.TempDir()
	olData := filepath.Join(workDir, "openlist-data")
	mediaRoot := filepath.Join(workDir, "media")
	movieDir := filepath.Join(mediaRoot, "电影")
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// ---- 0) 造一个真实视频。用 AV1 是因为浏览器能解码，
	//         于是播放决策会落到 L2「重封装」而不是 L4「外部播放器」，
	//         这样才能顺带验证 ffmpeg -c copy 这条真实路径。----
	srcFile := filepath.Join(movieDir, "测试影片 (2024).mkv")
	mk := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libaom-av1", "-cpu-used", "8", "-crf", "40",
		"-c:a", "aac", "-shortest", srcFile)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg 无法生成 AV1 测试视频（缺 libaom-av1？）: %v\n%s", err, lastLines(string(out), 5))
	}

	// ---- 1) 初始化并启动真实 139cas ----
	olPort := freePort(t)
	olURL := fmt.Sprintf("http://127.0.0.1:%d", olPort)

	setPass := exec.Command(olBin, "admin", "set", adminPass, "--data", olData)
	setPass.Dir = workDir
	if out, err := setPass.CombinedOutput(); err != nil {
		t.Fatalf("初始化 139cas 管理员失败: %v\n%s", err, out)
	}
	// OpenList 的 config.json 一旦生成，端口就以文件为准，OPENLIST_HTTP_PORT
	// 环境变量不再生效（这是它的既定行为，容器里也一样）。所以直接改文件。
	patchOpenListPort(t, filepath.Join(olData, "config.json"), olPort)

	olCmd := exec.Command(olBin, "server", "--no-prefix", "--data", olData)
	olCmd.Dir = workDir
	olCmd.Env = os.Environ()
	if err := olCmd.Start(); err != nil {
		t.Fatalf("启动 139cas 失败: %v", err)
	}
	t.Cleanup(func() {
		if olCmd.Process != nil {
			_ = olCmd.Process.Kill()
			_, _ = olCmd.Process.Wait()
		}
	})

	waitHTTP(t, olURL+"/ping", 60*time.Second)
	t.Log("✅ 内置 139cas 已启动:", olURL)

	// ---- 2) 在 139cas 里挂一个 Local 存储当作「网盘」----
	olToken := olLogin(t, olURL, "admin", adminPass)
	olMountLocal(t, olURL, olToken, "/local", mediaRoot)
	t.Log("✅ 已挂载本地目录为网盘: /local")

	// ---- 3) NewMovie 自动接管 —— 2.0 的核心承诺：用户不填任何 Token ----
	st, err := store.NewJSONStore(filepath.Join(workDir, "newmovie.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		DataDir:        workDir,
		CacheDir:       filepath.Join(workDir, "cache"),
		Bundled:        true,
		BundledURL:     olURL,
		BundledName:    "内置网盘",
		BundledUser:    "admin",
		BundledPass:    adminPass,
		BundledTimeout: 30 * time.Second,
		DefaultRate:    2.0,
	}
	if err := bootstrapBundledSync(st, cfg); err != nil {
		t.Fatalf("自动接管失败: %v", err)
	}
	storages, _ := st.ListStorages()
	if len(storages) != 1 {
		t.Fatalf("自动注册的存储数 = %d，期望 1", len(storages))
	}
	sto := storages[0]
	if len(sto.Token) < 50 {
		t.Fatalf("自动换取的 Token 不像真的 JWT（%d 字符）", len(sto.Token))
	}
	if sto.SignKey == "" {
		t.Error("未自动读取到签名密钥——用户将不得不去后台关闭签名")
	}
	t.Logf("✅ 自动接管成功: Token %d 字符, 签名密钥 %d 字符", len(sto.Token), len(sto.SignKey))

	// ---- 4) 起 NewMovie HTTP 服务 ----
	_ = st.SaveUser(model.User{ID: "u-admin", Username: "admin", IsAdmin: true})
	const tok = "e2e-tok"
	_ = st.UpsertToken("u-admin", tok)
	srv := New(st, cfg)
	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Handler())
	if p := NewBundledProxy(olURL); p != nil {
		mux.Handle(BundledProxyPrefix, p)
		mux.Handle(BundledProxyPrefix+"/", p)
	}
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// ---- 5) 反代：用户在同一端口就能开网盘管理页 ----
	if body, code := httpGet(t, ts.URL+BundledProxyPrefix+"/ping", ""); code != 200 || !strings.Contains(body, "pong") {
		t.Errorf("反代 /openlist/ping 失败: HTTP %d, body=%q", code, body)
	} else {
		t.Log("✅ 反代可用: /openlist/ → 内置后端")
	}

	// ---- 6) 浏览目录：用户点点就能选，不用手打路径 ----
	browseBody, code := httpGet(t, ts.URL+"/api/storages/"+sto.ID+"/browse?path=/", tok)
	if code != 200 {
		t.Fatalf("浏览目录失败: HTTP %d, %s", code, browseBody)
	}
	if !strings.Contains(browseBody, "/local") {
		t.Fatalf("浏览结果里没看到挂载的 /local: %s", browseBody)
	}
	t.Log("✅ 目录浏览: 直接看到内置网盘的 /local")

	// ---- 7) 建媒体库 ----
	libBody, code := httpPostJSON(t, ts.URL+"/api/libraries", tok, map[string]any{
		"name": "我的电影", "mode": "mixed",
		"storage_id": sto.ID, "root_path": "/local/电影",
	})
	if code != 200 {
		t.Fatalf("建媒体库失败: HTTP %d, %s", code, libBody)
	}
	var lib struct{ ID string }
	json.Unmarshal([]byte(libBody), &lib)
	if lib.ID == "" {
		t.Fatalf("媒体库 ID 为空: %s", libBody)
	}

	// ---- 8) 扫描 ----
	if body, code := httpPostJSON(t, ts.URL+"/api/libraries/"+lib.ID+"/scan", tok, nil); code != 200 {
		t.Fatalf("触发扫描失败: HTTP %d, %s", code, body)
	}
	var items []model.MediaItem
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		items, _ = st.ListMediaItems(lib.ID)
		if len(items) > 0 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if len(items) == 0 {
		t.Fatal("扫描后没有任何媒体项")
	}
	if items[0].Title != "测试影片" || items[0].Year != 2024 {
		t.Errorf("刮削结果 = %q (%d)，期望 测试影片 (2024)", items[0].Title, items[0].Year)
	}
	t.Logf("✅ 扫描完成: %q (%d)", items[0].Title, items[0].Year)

	// ---- 9) 点击播放 ----
	files, err := st.ListMediaFiles(items[0].ID)
	if err != nil || len(files) == 0 {
		t.Fatalf("媒体项下没有文件: %v", err)
	}
	playBody, code := httpGet(t, ts.URL+"/api/items/"+files[0].ID+"/play", tok)
	if code != 200 {
		t.Fatalf("播放决策失败: HTTP %d, %s", code, playBody)
	}
	var play struct {
		Level   int    `json:"level"`
		URL     string `json:"url"`
		RawURL  string `json:"raw_url"`
		Label   string `json:"label"`
		Reason  string `json:"reason"`
	}
	json.Unmarshal([]byte(playBody), &play)
	if play.Level != 2 {
		t.Fatalf("播放层级 = %d（%s），期望 2（重封装）。完整响应: %s", play.Level, play.Label, playBody)
	}
	if !strings.Contains(play.RawURL, "sign=") {
		t.Errorf("直链没有带签名，用户可能被迫去后台关闭签名: %s", play.RawURL)
	}
	t.Logf("✅ 播放决策: L%d %s（直链已自动签名）", play.Level, play.Label)

	// ---- 10) 真实拉流：ffmpeg 重封装出可播的 MP4 ----
	req, _ := http.NewRequest(http.MethodGet, ts.URL+play.URL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("拉流失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		t.Fatalf("拉流 HTTP %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "video/mp4") {
		t.Errorf("Content-Type = %q，期望 video/mp4", ct)
	}
	buf := make([]byte, 16)
	n, _ := io.ReadFull(resp.Body, buf)
	if n < 12 {
		t.Fatalf("只读到 %d 字节，重封装疑似失败", n)
	}
	// MP4 首个 box：前 4 字节是长度，紧接着是 "ftyp"。
	if string(buf[4:8]) != "ftyp" {
		t.Fatalf("输出不是合法 MP4，前 16 字节 = %x", buf[:n])
	}
	written, _ := io.Copy(io.Discard, resp.Body)
	t.Logf("✅ 真实重封装: 合法 MP4，共 %d 字节", int64(n)+written)
	t.Log("🎉 2.0 全链路通过：零配置连接 → 建库 → 扫描 → 播放")
}

// ---- 辅助 ----

// patchOpenListPort 改写 OpenList 的 config.json，把 HTTP 端口设为 port、关掉 HTTPS。
func patchOpenListPort(t *testing.T, path string, port int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 OpenList 配置失败: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析 OpenList 配置失败: %v", err)
	}
	scheme, _ := cfg["scheme"].(map[string]any)
	if scheme == nil {
		scheme = map[string]any{}
		cfg["scheme"] = scheme
	}
	scheme["http_port"] = port
	scheme["https_port"] = -1
	out, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("写回 OpenList 配置失败: %v", err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitHTTP(t *testing.T, url string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(url)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("等待 %s 就绪超时", url)
}

func olLogin(t *testing.T, base, user, pass string) string {
	t.Helper()
	body, code := httpPostJSONRaw(t, base+"/api/auth/login", "",
		map[string]string{"username": user, "password": pass})
	if code != 200 {
		t.Fatalf("登录 139cas 失败 HTTP %d: %s", code, body)
	}
	var r struct {
		Code int `json:"code"`
		Data struct{ Token string } `json:"data"`
	}
	json.Unmarshal([]byte(body), &r)
	if r.Data.Token == "" {
		t.Fatalf("139cas 未返回 token: %s", body)
	}
	return r.Data.Token
}

func olMountLocal(t *testing.T, base, token, mountPath, rootDir string) {
	t.Helper()
	addition, _ := json.Marshal(map[string]any{
		"root_folder_path": rootDir, "thumbnail": false, "show_hidden": false,
	})
	body, code := httpPostJSONRaw(t, base+"/api/admin/storage/create", token, map[string]any{
		"mount_path": mountPath, "driver": "Local",
		"order": 0, "status": "work", "addition": string(addition),
	})
	if code != 200 || !strings.Contains(body, `"code":200`) {
		t.Fatalf("挂载本地存储失败 HTTP %d: %s", code, body)
	}
	// 挂载后 OpenList 需要一点时间加载驱动。
	time.Sleep(1500 * time.Millisecond)
}

func httpGet(t *testing.T, url, bearer string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), resp.StatusCode
}

func httpPostJSON(t *testing.T, url, bearer string, payload any) (string, int) {
	t.Helper()
	return httpPostJSONRaw(t, url, "Bearer "+bearer, payload)
}

func httpPostJSONRaw(t *testing.T, url, auth string, payload any) (string, int) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), resp.StatusCode
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
