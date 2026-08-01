package strm

import (
	"os"
	"path/filepath"
	"testing"

	"newmovie/internal/model"
)

func newTestResolver() *Resolver {
	storages := []model.Storage{
		{ID: "s1", Name: "main", Type: model.StorageOpenList, BaseURL: "http://openlist:5244"},
	}
	rewrites := []model.PathRewrite{
		{ID: "r1", Priority: 1, Pattern: `^http://localhost:5244/d/(.*)$`, Replacement: "openlist://main/$1"},
	}
	return NewResolver(storages, rewrites)
}

func TestResolveDefaultSiteURL(t *testing.T) {
	r := newTestResolver()
	// OpenList strm 驱动默认形态：Site URL + /d/，含中文与空格（未编码）
	src := r.Resolve("http://openlist:5244/d/115_open/Video/电影/再见，李可乐 (2023).mkv")
	if !src.IsOpenListD || src.Scheme != "openlist" {
		t.Fatalf("应归一为 openlist /d/，得到 %+v", src)
	}
	if src.StorageID != "s1" {
		t.Fatalf("StorageID 应为 s1，得到 %q", src.StorageID)
	}
	want := "/115_open/Video/电影/再见，李可乐 (2023).mkv"
	if src.Path != want {
		t.Fatalf("路径应为 %q，得到 %q", want, src.Path)
	}
}

func TestResolveEncodePath(t *testing.T) {
	r := newTestResolver()
	// 开启 Encode Path：内部路径被 URL 编码
	src := r.Resolve("http://openlist:5244/d/115_open/Video/%E7%94%B5%E5%BD%B1/xx.mkv")
	if src.Path != "/115_open/Video/电影/xx.mkv" {
		t.Fatalf("应解码为 %q，得到 %q", "/115_open/Video/电影/xx.mkv", src.Path)
	}
}

func TestResolveSignedDURL(t *testing.T) {
	r := newTestResolver()
	// 开启签名：/d/ 后带 ?sign=，应忽略签名并归一
	src := r.Resolve("http://openlist:5244/d/quark/film.mkv?sign=abc123:0")
	if !src.IsOpenListD || src.Path != "/quark/film.mkv" {
		t.Fatalf("签名应被忽略，得到 %+v", src)
	}
}

func TestResolveLocalhostRewrite(t *testing.T) {
	r := newTestResolver()
	// 社区常见坑：strm 里写死 localhost（容器内不通）→ 重写规则修正
	src := r.Resolve("http://localhost:5244/d/quark/x.mkv")
	if src.Scheme != "openlist" || src.StorageID != "s1" || src.Path != "/quark/x.mkv" {
		t.Fatalf("重写后应归一，得到 %+v", src)
	}
}

func TestResolveWithoutURL(t *testing.T) {
	r := newTestResolver()
	// Without Url 模式：纯内部路径
	src := r.Resolve("/quark/电影/x.mkv")
	if src.Scheme != "openlist" || src.Path != "/quark/电影/x.mkv" {
		t.Fatalf("无 URL 模式应视为 openlist 内部路径，得到 %+v", src)
	}
}

func TestResolveDirectHTTP(t *testing.T) {
	r := newTestResolver()
	// 其他 CDN 直链，不是 OpenList
	src := r.Resolve("http://other-cdn.example.com/film.mp4")
	if src.Scheme != "http" || src.RawURL != "http://other-cdn.example.com/film.mp4" {
		t.Fatalf("应识别为直链，得到 %+v", src)
	}
}

func TestResolveLocalFile(t *testing.T) {
	r := newTestResolver()
	// 本地挂载点绝对路径（CloudDrive2 方案）：文件真实存在才算 file
	dir := t.TempDir()
	fp := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(fp, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := r.Resolve(fp)
	if src.Scheme != "file" || src.Path != fp {
		t.Fatalf("应识别为本地文件，得到 %+v", src)
	}
}

func TestResolveRelative(t *testing.T) {
	r := newTestResolver()
	src := r.Resolve("流浪地球.mkv")
	if src.Scheme != "relative" {
		t.Fatalf("应识别为相对路径，得到 %+v", src)
	}
}

func TestCleanStripsBOMAndComment(t *testing.T) {
	r := newTestResolver()
	// BOM + CRLF + 注释行处理：clean 在 Resolve 内部调用
	src := r.Resolve("\ufeffhttp://openlist:5244/d/quark/a.mkv\r")
	if src.Scheme != "openlist" || src.Path != "/quark/a.mkv" {
		t.Fatalf("BOM/CR 应被清理，得到 %+v", src)
	}
}
