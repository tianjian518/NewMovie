package webdav

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 一个模拟 WebDAV 服务端：对 PROPFIND 返回 multistatus，对 GET 返回文本。
func fakeWebDAVServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "PROPFIND":
			// 根目录下列出 电影(目录) 和 A.mkv(文件)
			w.Header().Set("Content-Type", "application/xml")
			io.WriteString(w, `<?xml version="1.0" encoding="utf-8"?>
<D:multistatus xmlns:D="DAV:">
  <D:response>
    <D:href>/dav/</D:href>
    <D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop></D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/%E7%94%B5%E5%BD%B1/</D:href>
    <D:propstat><D:prop><D:resourcetype><D:collection/></D:resourcetype></D:prop></D:propstat>
  </D:response>
  <D:response>
    <D:href>/dav/A.mkv</D:href>
    <D:propstat><D:prop>
      <D:resourcetype/>
      <D:getcontentlength>123456</D:getcontentlength>
      <D:getlastmodified>Wed, 02 Jan 2024 12:00:00 GMT</D:getlastmodified>
    </D:prop></D:propstat>
  </D:response>
</D:multistatus>`)
		case "GET":
			w.Write([]byte("hello strm"))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestWebDAV_List(t *testing.T) {
	srv := fakeWebDAVServer(t)
	c := New(srv.URL+"/dav", "")
	objs, err := c.List("/", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// 应列出 电影 与 A.mkv 两个（自身根条目被过滤）
	if len(objs) != 2 {
		t.Fatalf("List = %d 项, want 2: %+v", len(objs), objs)
	}
	var hasDir, hasFile bool
	for _, o := range objs {
		if o.Name == "电影" && o.IsDir {
			hasDir = true
		}
		if o.Name == "A.mkv" && !o.IsDir && int64(o.Size) == 123456 {
			hasFile = true
		}
	}
	if !hasDir || !hasFile {
		t.Fatalf("目录/文件解析失败: %+v", objs)
	}
}

func TestWebDAV_GetLinkAndRead(t *testing.T) {
	srv := fakeWebDAVServer(t)
	c := New(srv.URL+"/dav", "")
	link, err := c.GetLink("/电影/A.mkv", false)
	if err != nil {
		t.Fatal(err)
	}
	want := srv.URL + "/dav/电影/A.mkv"
	if link.Data.RawURL != want {
		t.Fatalf("GetLink.RawURL = %q, want %q", link.Data.RawURL, want)
	}
	if c.SignedDURL("/x.mkv") != srv.URL+"/dav/x.mkv" {
		t.Fatalf("SignedDURL 应为直链")
	}
	txt, err := c.ReadText("/a.nfo")
	if err != nil || txt != "hello strm" {
		t.Fatalf("ReadText = %q err=%v", txt, err)
	}
}

func TestWebDAV_AuthHeader(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/dav/", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusMultiStatus)
		io.WriteString(w, `<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"/>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL+"/dav", "basic:user1:pass1")
	_, _ = c.List("/", false)
	if gotAuth != "Basic dXNlcjE6cGFzczE=" { // user1:pass1
		t.Fatalf("Authorization = %q, want Basic auth", gotAuth)
	}
}

// 兼容 openlist.FSClient 接口的编译期断言已在 client.go 中，这里验证 New 能正确拆 basic token。
func TestWebDAV_ParseBasicToken(t *testing.T) {
	c := New("https://dav.x.com/dav/", "basic:alice:secret")
	if c.BaseURL != "https://dav.x.com/dav" || c.User != "alice" || c.Pass != "secret" {
		t.Fatalf("parse: %+v", c)
	}
	c2 := New("https://dav.x.com/dav/", "")
	if c2.User != "" || c2.BaseURL != "https://dav.x.com/dav" {
		t.Fatalf("no token parse: %+v", c2)
	}
}
