package tmdb

import (
	"net/url"
	"testing"
)

func TestImageURL_DefaultDirect(t *testing.T) {
	// 缺省返回 image.tmdb.org 直链（旧行为不变）。
	if got := ImageURL("/a.jpg", "w500"); got != "https://image.tmdb.org/t/p/w500/a.jpg" {
		t.Fatalf("默认应返回直链，得到 %q", got)
	}
	if got := ImageURL("", "w500"); got != "" {
		t.Fatalf("空 path 应返回空串，得到 %q", got)
	}
}

func TestImageURL_ProxyPrefix(t *testing.T) {
	defer SetImageProxyPrefix("")
	defer SetImageBase("")

	SetImageProxyPrefix("/api/image?u=")
	got := ImageURL("/a.jpg", "w500")
	want := "/api/image?u=" + url.QueryEscape("https://image.tmdb.org/t/p/w500/a.jpg")
	if got != want {
		t.Fatalf("应返回经代理的地址，得到 %q，期望 %q", got, want)
	}
}

func TestImageURL_MirrorBase(t *testing.T) {
	defer SetImageProxyPrefix("")
	defer SetImageBase("")

	SetImageBase("https://m.example.com/t/p")
	SetImageProxyPrefix("/api/image?u=")
	got := ImageURL("/a.jpg", "w500")
	want := "/api/image?u=" + url.QueryEscape("https://m.example.com/t/p/w500/a.jpg")
	if got != want {
		t.Fatalf("镜像基址应生效，得到 %q，期望 %q", got, want)
	}
	// ImageBaseHost / ImageHosts 应反映镜像主机，供 SSRF 白名单使用。
	if h := ImageBaseHost(); h != "m.example.com" {
		t.Fatalf("ImageBaseHost 应返回镜像主机，得到 %q", h)
	}
	hosts := ImageHosts()
	hasMirror, hasOfficial := false, false
	for _, h := range hosts {
		if h == "m.example.com" {
			hasMirror = true
		}
		if h == "image.tmdb.org" {
			hasOfficial = true
		}
	}
	if !hasMirror || !hasOfficial {
		t.Fatalf("ImageHosts 应含镜像与官方主机，得到 %v", hosts)
	}
}
