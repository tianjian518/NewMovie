// bootstrap.go —— 2.0 内置 OpenList（139cas）自动接管。
//
// 背景：1.x 里用户要自己部署 OpenList、复制 Token、再回 NewMovie 手填地址。
// 2.0 把 139cas 作为同容器的后端进程一起打包，于是这一步应该彻底消失：
// NewMovie 启动后自己等后端就绪、自己登录换 Token、自己把它注册成默认存储。
//
// 设计原则：
//  1. 幂等——重复启动只更新 Token，不会堆出一串重复存储。
//  2. 不阻断——内置后端起不来时只记日志，NewMovie 照常提供服务（用户仍可手动加外部 OpenList）。
//  3. 不侵入——139cas 源码零修改，所有胶水都在这里，方便同步上游。
package api

import (
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"newmovie/internal/auth"
	"newmovie/internal/config"
	"newmovie/internal/model"
	"newmovie/internal/openlist"
	"newmovie/internal/store"
)

// bundledReady 记录内置后端是否已成功接管，供 /api/health 暴露给前端。
var bundledReady atomic.Bool

// BundledReady 返回内置网盘是否已接管成功。
func BundledReady() bool { return bundledReady.Load() }

// BootstrapBundled 在后台完成内置 OpenList 的接管。
//
// 立即返回，真正的等待与注册在 goroutine 里做——内置后端首次启动要初始化
// 数据库和加载几十个 driver，可能十几秒，不能让 NewMovie 的 HTTP 端口跟着卡住。
//
// 接管失败会持续重试：内置后端可能因为磁盘慢、驱动多而启动很久，也可能被
// supervisor 重启过。一次失败就永久放弃的话，用户得手动重启整个容器才能恢复。
func BootstrapBundled(st store.Store, cfg *config.Config) {
	if cfg == nil || !cfg.Bundled {
		return
	}
	go func() {
		for attempt := 1; ; attempt++ {
			if err := bootstrapBundledSync(st, cfg); err == nil {
				bundledReady.Store(true)
				log.Printf("[bundled] 内置网盘已就绪并注册为默认存储：%s", cfg.BundledURL)
				// 进入保活：后端一旦崩溃/被 supervisor 重启、Token 失效，
				// 立刻复位状态并自动重新接管，UI 的「网盘挂载」状态才能反映真实情况。
				runBundledKeepalive(st, cfg)
				return
			} else if attempt == 1 {
				log.Printf("[bundled] 内置网盘接管失败：%v（后台将持续重试；期间可在「存储」里手动添加外部 OpenList）", err)
			} else if attempt%10 == 0 {
				log.Printf("[bundled] 内置网盘仍未接管成功（第 %d 次）：%v", attempt, err)
			}
			// bootstrapBundledSync 内部已有就绪轮询（默认 90s），这里再间隔一段，
			// 避免后端彻底挂掉时刷屏。
			time.Sleep(30 * time.Second)
		}
	}()
}

// runBundledKeepalive 周期性探测内置后端健康。探测失败时把 bundledReady 复位成
// false（UI 据此显示「未挂载」），然后持续重试重新接管，直到再次成功。
//
// 为什么需要它：139cas 由 supervisord 监管，可能因 OOM / 驱动异常被重启；重启后
// 旧的 Token 可能失效、存储记录变陈旧。原实现只在首次成功时置 true 且永不复位，
// 于是后端早已挂掉、UI 却还显示「已挂载」，用户点进去全是报错——典型的「状态假绿」。
func runBundledKeepalive(st store.Store, cfg *config.Config) {
	cl := &openlist.Client{BaseURL: cfg.BundledURL}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if perr := cl.Ping(); perr == nil {
			continue
		} else {
			log.Printf("[bundled] 内置网盘健康检查失败，复位状态并尝试重新接管：%v", perr)
		}
		bundledReady.Store(false)
		for {
			if err := bootstrapBundledSync(st, cfg); err == nil {
				bundledReady.Store(true)
				log.Printf("[bundled] 内置网盘已重新接管：%s", cfg.BundledURL)
				break
			}
			time.Sleep(5 * time.Second)
		}
	}
}

// bootstrapBundledSync 同步版本，供测试直接调用。
func bootstrapBundledSync(st store.Store, cfg *config.Config) error {
	cl := &openlist.Client{BaseURL: cfg.BundledURL}

	// 1) 等内置后端就绪。
	if err := waitReady(cl, cfg.BundledTimeout); err != nil {
		return err
	}

	// 2) 拿 Token：优先用显式配置的，其次用账号密码登录换。
	token := strings.TrimSpace(cfg.BundledToken)
	if token == "" {
		pass := cfg.BundledPass
		if pass == "" {
			// 容器启动脚本会把首次生成的随机管理员密码写到这里。
			pass = readTokenFile(cfg.DataDir + "/openlist_admin_pass")
		}
		if pass == "" {
			return errBundled("未配置内置网盘的 Token 或管理员密码")
		}
		t, err := cl.Login(cfg.BundledUser, pass)
		if err != nil {
			return err
		}
		token = t
	}
	cl.Token = token

	// 3) 顺手把签名密钥也读过来，这样 /d/ 直链能自己算 sign，用户不用去后台关签名。
	signKey, err := cl.SettingSignAll()
	if err != nil {
		// 后台没开「签名所有」时读不到很正常，不当错误。
		signKey = ""
	}

	// 4) 幂等注册为存储。
	return upsertBundledStorage(st, cfg, token, signKey)
}

// waitReady 轮询 /ping 直到内置后端响应或超时。
func waitReady(cl *openlist.Client, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.Now().Add(timeout)
	interval := 500 * time.Millisecond
	var last error
	for time.Now().Before(deadline) {
		if err := cl.Ping(); err == nil {
			return nil
		} else {
			last = err
		}
		time.Sleep(interval)
		// 退避到最多 3s，避免早期高频打日志、后期又等太久。
		if interval < 3*time.Second {
			interval += 500 * time.Millisecond
		}
	}
	if last == nil {
		last = errBundled("超时")
	}
	return errBundled("等待内置网盘就绪超时：" + last.Error())
}

// upsertBundledStorage 幂等地把内置 OpenList 写进存储列表。
//
// 已存在同 BaseURL 的记录时只刷新 Token/签名密钥，保留用户可能改过的名称、
// 限速和 LocalRoot——用户的手工调整不该被每次重启冲掉。
func upsertBundledStorage(st store.Store, cfg *config.Config, token, signKey string) error {
	existing, err := st.GetStorageByBaseURL(cfg.BundledURL)
	if err == nil && existing.ID != "" {
		changed := false
		if existing.Token != token {
			existing.Token = token
			changed = true
		}
		if signKey != "" && existing.SignKey != signKey {
			existing.SignKey = signKey
			changed = true
		}
		if !changed {
			return nil
		}
		return st.SaveStorage(existing)
	}

	s := model.Storage{
		ID:        auth.GenID("st"),
		Name:      cfg.BundledName,
		Type:      model.StorageOpenList,
		BaseURL:   cfg.BundledURL,
		Token:     token,
		SignKey:   signKey,
		RateLimit: cfg.DefaultRate,
		CreatedAt: time.Now().Unix(),
	}
	return st.SaveStorage(s)
}

// readTokenFile 读取一个只含单行内容的文件，去掉首尾空白。文件不存在返回空串。
func readTokenFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type bundledErr string

func (e bundledErr) Error() string { return string(e) }

func errBundled(msg string) error { return bundledErr(msg) }
