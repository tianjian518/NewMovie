// Vidrive 服务入口。
// 标准库 net/http 实现，零外部依赖，单二进制。
// 前端静态资源在构建时由 Dockerfile 的 node 阶段产出到 dist/ 并 embed 进二进制。
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"newmovie/internal/api"
	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/store"
)

//go:embed dist
var dist embed.FS

func main() {
	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Printf("警告：创建数据目录 %s 失败: %v（将以只读模式继续，设置无法持久化）", cfg.DataDir, err)
	}

	st, err := store.NewJSONStore(cfg.DBFile)
	if err != nil {
		log.Fatalf("打开存储失败: %v", err)
	}

	// 确保管理员账号存在
	if _, e := st.GetUserByName(cfg.AdminUser); e != nil {
		_ = st.SaveUser(model.User{
			ID:       "u-admin",
			Username: cfg.AdminUser,
			Password: auth.HashPassword(cfg.AdminPass),
			IsAdmin:  true,
		})
		log.Printf("已创建管理员账号 %q（请尽快修改默认密码）", cfg.AdminUser)
	}

	srv := api.New(st, cfg)

	mux := http.NewServeMux()
	mux.Handle("/api/", srv.Handler())

	// 前端静态资源（dist 由构建产出）。
	// 必须用 fs.Sub 剥掉 "dist" 这层前缀：embed.FS 的根是 dist 的**父目录**，
	// 直接 http.FS(dist) 会让 "/" 列出目录（页面上只有一个 dist/ 链接），
	// 且 index.html 里的 /assets/... 全部 404 → React 挂不上 → 白屏。
	mux.Handle("/", spaHandler(dist))

	log.Printf("NewMovie 启动 → %s  (data=%s, admin=%s)", cfg.Addr, cfg.DBFile, cfg.AdminUser)
	log.Printf("健康检查: GET %s/api/health", cfg.Addr)

	srvHTTP := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 不设 WriteTimeout：视频/大图是长连接流式传输，超时会截断播放。
	}

	// 端口占用等瞬时故障：先重试若干次再放弃。
	// 直接 Fatal 会让容器在 restart 策略下疯狂重启（无限重启的常见来源之一）。
	ln, err := listenWithRetry(cfg.Addr, 5)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", cfg.Addr, err)
	}

	go func() {
		if e := srvHTTP.Serve(ln); e != nil && e != http.ErrServerClosed {
			log.Printf("HTTP 服务异常退出: %v", e)
		}
	}()

	// 优雅退出：收到 SIGTERM（docker stop）时给在途请求收尾时间，
	// 避免半截写入把 JSON 存储写坏 —— 坏存储会导致下次启动即失败，进而无限重启。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("收到退出信号，正在优雅关闭…")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srvHTTP.Shutdown(ctx)
	if e := st.Close(); e != nil { // 落盘最后一批修改，避免数据丢失
		log.Printf("存储关闭时落盘失败: %v", e)
	}
	log.Printf("已退出")
}

// spaHandler 提供前端单页应用：
//  1. 用 fs.Sub 把 embed 根定位到 dist/ 内部，使 /assets/xxx.js 能正确命中；
//  2. 命中真实文件则直接返回（带上合适的缓存头）；
//  3. 未命中且不是 /api/ 的路径，一律回落到 index.html —— react-router 的
//     前端路由（如 /library/xxx）刷新时才不会 404。
func spaHandler(efs embed.FS) http.Handler {
	sub, err := fs.Sub(efs, "dist")
	if err != nil {
		log.Printf("警告：无法定位内嵌的 dist 目录: %v", err)
		return http.NotFoundHandler()
	}
	fileSrv := http.FileServer(http.FS(sub))

	indexHTML, idxErr := fs.ReadFile(sub, "index.html")
	if idxErr != nil {
		log.Printf("警告：内嵌前端缺少 index.html（前端可能未构建）: %v", idxErr)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := strings.TrimPrefix(r.URL.Path, "/")
		if upath == "" {
			upath = "index.html"
		}

		// 真实存在的文件：交给 FileServer（自动处理 Content-Type / Range / 304）
		if f, e := sub.Open(upath); e == nil {
			if st, se := f.Stat(); se == nil && !st.IsDir() {
				// 带内容哈希的构建产物可长期缓存；index.html 必须每次校验，
				// 否则前端更新后用户一直拿到旧版本。
				if strings.HasPrefix(upath, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				_ = f.Close()
				fileSrv.ServeHTTP(w, r)
				return
			}
			_ = f.Close()
		}

		// 其余路径回落到 index.html（前端路由接管）。
		// 但 /api/ 已由前面的 mux 处理；这里再兜一次，避免误把接口 404 变成 HTML。
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if indexHTML == nil {
			http.Error(w, "前端资源缺失：请确认镜像构建时已执行前端构建", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(indexHTML)
	})
}

// listenWithRetry 监听端口，失败时退避重试，缓解「端口尚未释放」这类瞬时冲突。
func listenWithRetry(addr string, attempts int) (net.Listener, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		lastErr = err
		wait := time.Duration(i+1) * 2 * time.Second
		log.Printf("监听 %s 失败（第 %d/%d 次）: %v，%v 后重试", addr, i+1, attempts, err, wait)
		time.Sleep(wait)
	}
	return nil, lastErr
}
