package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// fakeOpenList 模拟内置 139cas：/ping、/api/auth/login、/api/admin/setting/get。
// readyAfter 用来模拟「进程刚起来还没就绪」——前 N 次 /ping 返回 503。
type fakeOpenList struct {
	srv        *httptest.Server
	pings      int32
	readyAfter int32
	user, pass string
	token      string
	signKey    string
	loginCalls int32
}

func newFakeOpenList(t *testing.T, f *fakeOpenList) *fakeOpenList {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&f.pings, 1)
		if n <= f.readyAfter {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("pong"))
	})

	mux.HandleFunc("/api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.loginCalls, 1)
		var req struct{ Username, Password string }
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Username != f.user || req.Password != f.pass {
			json.NewEncoder(w).Encode(map[string]any{"code": 401, "message": "密码不正确"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"code": 200, "message": "success",
			"data": map[string]string{"token": f.token},
		})
	})

	mux.HandleFunc("/api/admin/setting/get", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != f.token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 200,
			"data": map[string]string{"key": "token", "value": f.signKey},
		})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func bundledCfg(t *testing.T, url string) *config.Config {
	t.Helper()
	return &config.Config{
		DataDir:        t.TempDir(),
		Bundled:        true,
		BundledURL:     url,
		BundledName:    "内置网盘",
		BundledUser:    "admin",
		BundledTimeout: 8 * time.Second,
		DefaultRate:    2.0,
	}
}

// 用账号密码自动换 Token 并注册为默认存储 —— 这是 2.0「零配置」的核心承诺。
func TestBootstrapBundled_LoginAndRegister(t *testing.T) {
	f := newFakeOpenList(t, &fakeOpenList{
		user: "admin", pass: "s3cret", token: "tok-abc", signKey: "signkey-1",
	})
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatal(err)
	}
	cfg := bundledCfg(t, f.srv.URL)
	cfg.BundledPass = "s3cret"

	if err := bootstrapBundledSync(st, cfg); err != nil {
		t.Fatalf("接管失败: %v", err)
	}

	list, _ := st.ListStorages()
	if len(list) != 1 {
		t.Fatalf("期望注册 1 个存储，实际 %d", len(list))
	}
	s := list[0]
	if s.Type != model.StorageOpenList {
		t.Errorf("存储类型 = %q，期望 openlist", s.Type)
	}
	if s.BaseURL != f.srv.URL {
		t.Errorf("BaseURL = %q，期望 %q", s.BaseURL, f.srv.URL)
	}
	if s.Token != "tok-abc" {
		t.Errorf("Token = %q，期望自动登录换来的 tok-abc", s.Token)
	}
	if s.SignKey != "signkey-1" {
		t.Errorf("SignKey = %q，期望自动读取的 signkey-1", s.SignKey)
	}
	if s.Name != "内置网盘" {
		t.Errorf("Name = %q", s.Name)
	}
}

// 重复启动必须幂等：不能每次重启都堆一个新存储出来。
func TestBootstrapBundled_Idempotent(t *testing.T) {
	f := newFakeOpenList(t, &fakeOpenList{
		user: "admin", pass: "s3cret", token: "tok-abc", signKey: "sk",
	})
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := bundledCfg(t, f.srv.URL)
	cfg.BundledPass = "s3cret"

	for i := 0; i < 3; i++ {
		if err := bootstrapBundledSync(st, cfg); err != nil {
			t.Fatalf("第 %d 次接管失败: %v", i+1, err)
		}
	}
	list, _ := st.ListStorages()
	if len(list) != 1 {
		t.Fatalf("重复启动 3 次后存储数 = %d，期望恒为 1", len(list))
	}
}

// 用户改过的名称/限速不能被重启冲掉，但 Token 轮换要生效。
func TestBootstrapBundled_PreservesUserEditsButRefreshesToken(t *testing.T) {
	f := newFakeOpenList(t, &fakeOpenList{
		user: "admin", pass: "s3cret", token: "tok-v1", signKey: "sk",
	})
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := bundledCfg(t, f.srv.URL)
	cfg.BundledPass = "s3cret"

	if err := bootstrapBundledSync(st, cfg); err != nil {
		t.Fatal(err)
	}
	// 用户在 UI 里改了名字和限速
	list, _ := st.ListStorages()
	s := list[0]
	s.Name = "我的阿里云盘"
	s.RateLimit = 5
	s.LocalRoot = "/mnt/cloud"
	if err := st.SaveStorage(s); err != nil {
		t.Fatal(err)
	}

	// OpenList 侧 Token 轮换后再次启动
	f.token = "tok-v2"
	if err := bootstrapBundledSync(st, cfg); err != nil {
		t.Fatal(err)
	}

	list, _ = st.ListStorages()
	if len(list) != 1 {
		t.Fatalf("存储数 = %d，期望 1", len(list))
	}
	got := list[0]
	if got.Token != "tok-v2" {
		t.Errorf("Token = %q，期望刷新为 tok-v2", got.Token)
	}
	if got.Name != "我的阿里云盘" {
		t.Errorf("Name = %q，用户改的名字被冲掉了", got.Name)
	}
	if got.RateLimit != 5 {
		t.Errorf("RateLimit = %v，用户改的限速被冲掉了", got.RateLimit)
	}
	if got.LocalRoot != "/mnt/cloud" {
		t.Errorf("LocalRoot = %q，用户配置被冲掉了", got.LocalRoot)
	}
}

// 内置后端慢启动（前几次 ping 503）时应当耐心等待，而不是立刻放弃。
func TestBootstrapBundled_WaitsForSlowStart(t *testing.T) {
	f := newFakeOpenList(t, &fakeOpenList{
		user: "admin", pass: "s3cret", token: "tok", readyAfter: 3,
	})
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := bundledCfg(t, f.srv.URL)
	cfg.BundledPass = "s3cret"

	if err := bootstrapBundledSync(st, cfg); err != nil {
		t.Fatalf("慢启动场景接管失败: %v", err)
	}
	if got := atomic.LoadInt32(&f.pings); got < 4 {
		t.Errorf("ping 次数 = %d，期望至少 4 次（前 3 次 503）", got)
	}
	list, _ := st.ListStorages()
	if len(list) != 1 {
		t.Fatalf("存储数 = %d", len(list))
	}
}

// 显式配置了 Token 时不该再去登录。
func TestBootstrapBundled_ExplicitTokenSkipsLogin(t *testing.T) {
	f := newFakeOpenList(t, &fakeOpenList{user: "admin", pass: "x", token: "should-not-be-used"})
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := bundledCfg(t, f.srv.URL)
	cfg.BundledToken = "my-own-token"

	if err := bootstrapBundledSync(st, cfg); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&f.loginCalls); n != 0 {
		t.Errorf("登录调用 %d 次，配置了 Token 时不应登录", n)
	}
	list, _ := st.ListStorages()
	if list[0].Token != "my-own-token" {
		t.Errorf("Token = %q，期望使用显式配置的", list[0].Token)
	}
}

// 密码写在文件里（容器首次启动生成随机密码的场景）。
func TestBootstrapBundled_ReadsPassFromFile(t *testing.T) {
	f := newFakeOpenList(t, &fakeOpenList{user: "admin", pass: "file-pass", token: "tok-f"})
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := bundledCfg(t, f.srv.URL)
	if err := writeFileHelper(cfg.DataDir+"/openlist_admin_pass", "file-pass\n"); err != nil {
		t.Fatal(err)
	}

	if err := bootstrapBundledSync(st, cfg); err != nil {
		t.Fatalf("从文件读密码失败: %v", err)
	}
	list, _ := st.ListStorages()
	if list[0].Token != "tok-f" {
		t.Errorf("Token = %q", list[0].Token)
	}
}

// 内置后端起不来时只能失败返回，绝不能 panic 或阻塞太久。
func TestBootstrapBundled_UnreachableFailsGracefully(t *testing.T) {
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := bundledCfg(t, "http://127.0.0.1:1")
	cfg.BundledPass = "x"
	cfg.BundledTimeout = 1200 * time.Millisecond

	start := time.Now()
	err := bootstrapBundledSync(st, cfg)
	if err == nil {
		t.Fatal("期望返回错误")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误信息 = %q，期望包含「超时」", err.Error())
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("耗时 %v，超时控制失效", d)
	}
	list, _ := st.ListStorages()
	if len(list) != 0 {
		t.Errorf("失败时不应写入存储，实际 %d 条", len(list))
	}
}

// 密码错误时给出可读错误，不注册存储。
func TestBootstrapBundled_BadPassword(t *testing.T) {
	f := newFakeOpenList(t, &fakeOpenList{user: "admin", pass: "right", token: "tok"})
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := bundledCfg(t, f.srv.URL)
	cfg.BundledPass = "wrong"

	err := bootstrapBundledSync(st, cfg)
	if err == nil {
		t.Fatal("期望登录失败")
	}
	if !strings.Contains(err.Error(), "密码不正确") {
		t.Errorf("错误信息 = %q，期望透出 OpenList 的原始提示", err.Error())
	}
	list, _ := st.ListStorages()
	if len(list) != 0 {
		t.Errorf("登录失败不应注册存储")
	}
}

// 未开启 Bundled 时 BootstrapBundled 必须完全无副作用（1.x 行为不变）。
func TestBootstrapBundled_DisabledIsNoop(t *testing.T) {
	st, _ := store.NewJSONStore(t.TempDir() + "/v.json")
	cfg := &config.Config{Bundled: false}
	BootstrapBundled(st, cfg)
	BootstrapBundled(st, nil)
	time.Sleep(150 * time.Millisecond)
	list, _ := st.ListStorages()
	if len(list) != 0 {
		t.Errorf("未开启内置时不应有任何存储，实际 %d 条", len(list))
	}
}

func writeFileHelper(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
