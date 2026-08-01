package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// fakeImgOpenList 返回一张固定图片，用于验证服务端图片缓存层。
func fakeImgOpenList(t *testing.T, img []byte) *httptest.Server {
	t.Helper()
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/fs/get":
			var b struct {
				Path string `json:"path"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			raw := base + "/raw?file=" + url.QueryEscape(b.Path)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 200, "message": "",
				"data": map[string]interface{}{"raw_url": raw, "url": "", "sign": ""},
			})
		case r.URL.Path == "/raw":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(img)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	base = srv.URL
	return srv
}

func TestServeItemImage_CacheLayer(t *testing.T) {
	img := []byte("FAKEJPEG-BYTES-0123456789")
	srv := fakeImgOpenList(t, img)
	defer srv.Close()

	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	item := model.MediaItem{ID: "m-img", LibraryID: "lib1", Kind: model.KindMovie, Title: "x", PosterPath: "/img/poster.jpg", PosterStorageID: "st1"}
	if err := st.SaveMediaItem(item); err != nil {
		t.Fatalf("save item: %v", err)
	}
	lib := model.Library{ID: "lib1", Name: "L", Mode: model.ModeNative, StorageID: "st1", RootPath: "/img"}
	_ = st.SaveLibrary(lib)
	strg := model.Storage{ID: "st1", Type: model.StorageOpenList, BaseURL: srv.URL, Token: "t"}
	_ = st.SaveStorage(strg)

	// 用户 + token（路由层需鉴权）
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")

	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	s := New(st, cfg)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	do := func() (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/items/m-img/poster", nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("req: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	code1, body1 := do()
	if code1 != http.StatusOK {
		t.Fatalf("first request code = %d, want 200", code1)
	}
	if body1 != string(img) {
		t.Fatalf("body mismatch: %q", body1)
	}

	// 关闭源站，验证第二次请求从缓存命中（不再回源）。
	srv.Close()
	code2, body2 := do()
	if code2 != http.StatusOK {
		t.Fatalf("cached request code = %d, want 200（缓存应命中）", code2)
	}
	if body2 != string(img) {
		t.Fatalf("cached body mismatch: %q", body2)
	}
}

// TestServeImageBytes_Range 验证 Range 分段返回。
func TestServeImageBytes_Range(t *testing.T) {
	b := []byte("0123456789") // 10 字节
	req := httptest.NewRequest(http.MethodGet, "/x?range=bytes=2-5", nil)
	req.Header.Set("Range", "bytes=2-5")
	w := httptest.NewRecorder()
	serveImageBytes(w, req, b, "image/jpeg")
	if w.Code != http.StatusPartialContent {
		t.Fatalf("code = %d, want 206", w.Code)
	}
	if w.Body.String() != "2345" {
		t.Fatalf("body = %q, want 2345", w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Range"), "bytes 2-5/10") {
		t.Fatalf("Content-Range = %q", w.Header().Get("Content-Range"))
	}
}
