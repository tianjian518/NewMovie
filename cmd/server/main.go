// Vidrive 服务入口。
// 标准库 net/http 实现，零外部依赖，单二进制。
// 前端静态资源在构建时由 Dockerfile 的 node 阶段产出到 dist/ 并 embed 进二进制。
package main

import (
	"embed"
	"log"
	"net/http"
	"os"

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
	_ = os.MkdirAll(cfg.DataDir, 0o755)

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

	// 前端静态资源（dist 由构建产出；开发期占位页见 dist/index.html）
	mux.Handle("/", http.FileServer(http.FS(dist)))

	log.Printf("Vidrive 启动 → %s  (data=%s, admin=%s)", cfg.Addr, cfg.DBFile, cfg.AdminUser)
	log.Printf("健康检查: GET %s/api/health", cfg.Addr)
	log.Fatal(http.ListenAndServe(cfg.Addr, mux))
}
