package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

// mockOpenList 同时扮演 OpenList API（/api/fs/get 返回指向自身的 raw_url）
// 与普通文件服务器（按路径返回字节），方便在单测里端到端验证字幕/播放链路。
func mockOpenList(t *testing.T, files map[string][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/fs/get" {
			var req struct {
				Path string `json:"path"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			raw := "http://" + r.Host + req.Path
			json.NewEncoder(w).Encode(map[string]any{"code": 200, "data": map[string]any{"raw_url": raw, "url": raw}})
			return
		}
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
}

func setupServerWith(t *testing.T, mock *httptest.Server, item model.MediaItem, files ...model.MediaFile) (*httptest.Server, func(path string) (int, string)) {
	st, err := store.NewJSONStore(t.TempDir() + "/v.json")
	if err != nil {
		t.Fatal(err)
	}
	u := model.User{ID: "u1", Username: "admin", Password: auth.HashPassword("admin"), IsAdmin: true}
	_ = st.SaveUser(u)
	_ = st.UpsertToken(u.ID, "tok")
	cfg := &config.Config{DataDir: t.TempDir(), CacheDir: t.TempDir() + "/cache"}
	_ = st.SaveStorage(model.Storage{ID: "st1", Type: model.StorageOpenList, BaseURL: mock.URL})
	_ = st.UpsertMediaItemByTitle(item)
	for _, f := range files {
		_ = st.SaveMediaFile(f)
	}
	ts := httptest.NewServer(New(st, cfg).Handler())
	t.Cleanup(ts.Close)
	get := func(p string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+p, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	return ts, get
}

func TestServeSubtitle_SRT(t *testing.T) {
	srt := []byte("1\n00:00:01,000 --> 00:00:04,000\n你好，世界\n")
	mock := mockOpenList(t, map[string][]byte{"/sample.zh.srt": srt})
	defer mock.Close()
	item := model.MediaItem{ID: "m1", LibraryID: "lib1", Title: "测试", Kind: model.KindMovie}
	f := model.MediaFile{ID: "f1", ItemID: "m1", StorageID: "st1", Path: "/sample.mkv",
		Container: "mkv", VideoCodec: "h264", AudioCodec: "aac", ProbeState: "skipped",
		Subtitles: []model.Subtitle{{ID: "s1", StorageID: "st1", Path: "/sample.zh.srt", Lang: "zh", Title: "简体中文", Ext: "srt", Source: "sidecar"}}}
	_, get := setupServerWith(t, mock, item, f)
	code, body := get("/api/play/subtitle?file=f1&lang=zh")
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d body=%s", code, body)
	}
	if !strings.Contains(body, "WEBVTT") {
		t.Errorf("不是 WebVTT: %s", body)
	}
	if !strings.Contains(body, "你好，世界") {
		t.Errorf("字幕正文丢失: %s", body)
	}
}

func TestServeSubtitle_NoSubs(t *testing.T) {
	mock := mockOpenList(t, map[string][]byte{})
	defer mock.Close()
	item := model.MediaItem{ID: "m1", LibraryID: "lib1", Title: "测试", Kind: model.KindMovie}
	f := model.MediaFile{ID: "f1", ItemID: "m1", StorageID: "st1", Path: "/sample.mkv", Container: "mkv", ProbeState: "skipped"}
	_, get := setupServerWith(t, mock, item, f)
	code, _ := get("/api/play/subtitle?file=f1&lang=zh")
	if code != http.StatusNotFound {
		t.Errorf("无字幕应 404，得到 %d", code)
	}
}

func TestServeSubtitle_MissingParam(t *testing.T) {
	mock := mockOpenList(t, nil)
	defer mock.Close()
	item := model.MediaItem{ID: "m1", LibraryID: "lib1", Title: "测试", Kind: model.KindMovie}
	f := model.MediaFile{ID: "f1", ItemID: "m1", StorageID: "st1", Path: "/sample.mkv", ProbeState: "skipped"}
	_, get := setupServerWith(t, mock, item, f)
	code, _ := get("/api/play/subtitle")
	if code != http.StatusBadRequest {
		t.Errorf("缺参应 400，得到 %d", code)
	}
}

func TestPlayItem_IncludesSubtitlesAndAudio(t *testing.T) {
	mock := mockOpenList(t, map[string][]byte{"/sample.mkv": []byte("fake")})
	defer mock.Close()
	item := model.MediaItem{ID: "m1", LibraryID: "lib1", Title: "测试", Kind: model.KindMovie}
	f := model.MediaFile{ID: "f1", ItemID: "m1", StorageID: "st1", Path: "/sample.mkv",
		Container: "mkv", VideoCodec: "h264", AudioCodec: "aac", ProbeState: "skipped",
		Subtitles:   []model.Subtitle{{ID: "s1", StorageID: "st1", Path: "/sample.zh.srt", Lang: "zh", Title: "简体中文", Ext: "srt"}},
		AudioTracks: []model.AudioTrack{{Index: 0, Lang: "und", Codec: "aac"}, {Index: 1, Lang: "eng", Codec: "aac"}},
	}
	_, get := setupServerWith(t, mock, item, f)
	code, body := get("/api/items/f1/play")
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d body=%s", code, body)
	}
	var dec struct {
		Subtitles   []map[string]string `json:"subtitles"`
		AudioTracks []model.AudioTrack  `json:"audio_tracks"`
	}
	if err := json.Unmarshal([]byte(body), &dec); err != nil {
		t.Fatal(err)
	}
	if len(dec.Subtitles) != 1 || dec.Subtitles[0]["lang"] != "zh" {
		t.Errorf("字幕未返回: %+v", dec.Subtitles)
	}
	if len(dec.AudioTracks) != 2 {
		t.Errorf("音轨未返回: %+v", dec.AudioTracks)
	}
}

func TestRemux_Atrack(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg 不可用，跳过选轨集成测试")
	}
	mkv := filepath.Join(t.TempDir(), "a.mkv")
	gen := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=2:size=320x180:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=2",
		"-map", "0:v", "-map", "1:a", "-map", "2:a",
		"-c:v", "libopenh264", "-c:a", "aac", "-pix_fmt", "yuv420p", mkv)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("生成 mkv 失败: %v %s", err, out)
	}
	data, err := os.ReadFile(mkv)
	if err != nil {
		t.Fatal(err)
	}
	mock := mockOpenList(t, map[string][]byte{"/a.mkv": data})
	defer mock.Close()
	_, get := setupServerWith(t, mock, model.MediaItem{}, model.MediaFile{})
	code, body := get("/api/play/remux?u=" + mock.URL + "/a.mkv&atrack=1")
	if code != http.StatusOK {
		t.Fatalf("remux atrack 期望 200，得到 %d body=%s", code, body)
	}
	if !strings.Contains(body, "ftyp") {
		t.Errorf("atrack 输出不是 MP4: %s", body)
	}
}
