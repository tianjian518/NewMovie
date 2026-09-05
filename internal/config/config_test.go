package config

import (
	"testing"
	"time"
)

// 默认（裸跑二进制）必须保持 1.x 行为：不启用内置网盘。
// 否则老用户升级后会看到莫名其妙的「内置网盘接管失败」日志。
func TestLoad_BundledDefaultsOff(t *testing.T) {
	c := Load()
	if c.Bundled {
		t.Error("未设置环境变量时 Bundled 应为 false（保持 1.x 行为）")
	}
	if c.BundledProxy {
		t.Error("未开启内置时不应启用反代")
	}
	if c.BundledURL != "http://127.0.0.1:5244" {
		t.Errorf("BundledURL 默认值 = %q", c.BundledURL)
	}
	if c.BundledUser != "admin" {
		t.Errorf("BundledUser 默认值 = %q", c.BundledUser)
	}
	if c.BundledTimeout != 90*time.Second {
		t.Errorf("BundledTimeout 默认值 = %v", c.BundledTimeout)
	}
}

// 容器里注入 NEWMOVIE_BUNDLED=1 后，反代应该跟着自动打开。
func TestLoad_BundledOnEnablesProxy(t *testing.T) {
	t.Setenv("NEWMOVIE_BUNDLED", "1")
	c := Load()
	if !c.Bundled {
		t.Fatal("NEWMOVIE_BUNDLED=1 时 Bundled 应为 true")
	}
	if !c.BundledProxy {
		t.Error("开启内置后反代应默认跟随开启")
	}
}

// 反代可以单独关掉（有人可能不想暴露 OpenList 后台）。
func TestLoad_ProxyCanBeDisabledSeparately(t *testing.T) {
	t.Setenv("NEWMOVIE_BUNDLED", "1")
	t.Setenv("NEWMOVIE_BUNDLED_PROXY", "0")
	c := Load()
	if !c.Bundled {
		t.Fatal("Bundled 应为 true")
	}
	if c.BundledProxy {
		t.Error("显式设 NEWMOVIE_BUNDLED_PROXY=0 时不应启用反代")
	}
}

// URL 尾部斜杠要归一化，否则和 GetStorageByBaseURL 的匹配会错位、
// 导致每次重启都新增一条存储。
func TestLoad_BundledURLTrimsTrailingSlash(t *testing.T) {
	t.Setenv("NEWMOVIE_BUNDLED_URL", "http://openlist:5244///")
	c := Load()
	if c.BundledURL != "http://openlist:5244" {
		t.Errorf("BundledURL = %q，期望去掉尾部斜杠", c.BundledURL)
	}
}

func TestLoad_BundledTimeoutParsing(t *testing.T) {
	t.Setenv("NEWMOVIE_BUNDLED_TIMEOUT", "3m")
	if c := Load(); c.BundledTimeout != 3*time.Minute {
		t.Errorf("BundledTimeout = %v，期望 3m", c.BundledTimeout)
	}
	// 非法值回退默认，不能变成 0 导致永不等待。
	t.Setenv("NEWMOVIE_BUNDLED_TIMEOUT", "很久")
	if c := Load(); c.BundledTimeout != 90*time.Second {
		t.Errorf("非法值时 BundledTimeout = %v，期望回退 90s", c.BundledTimeout)
	}
	t.Setenv("NEWMOVIE_BUNDLED_TIMEOUT", "-5s")
	if c := Load(); c.BundledTimeout != 90*time.Second {
		t.Errorf("负值时 BundledTimeout = %v，期望回退 90s", c.BundledTimeout)
	}
}

func TestBoolenv(t *testing.T) {
	cases := []struct {
		val  string
		def  bool
		want bool
	}{
		{"1", false, true}, {"true", false, true}, {"on", false, true},
		{"yes", false, true}, {"Y", false, true}, {"TRUE", false, true},
		{"0", true, false}, {"false", true, false}, {"off", true, false},
		{"no", true, false}, {"n", true, false},
		{" 1 ", false, true}, // 带空白也要认
		{"", true, true},     // 未设置用默认
		{"", false, false},
		{"随便", true, true}, // 无法识别用默认
		{"随便", false, false},
	}
	for _, c := range cases {
		t.Setenv("TEST_BOOLENV", c.val)
		if got := boolenv("TEST_BOOLENV", c.def); got != c.want {
			t.Errorf("boolenv(%q, def=%v) = %v，期望 %v", c.val, c.def, got, c.want)
		}
	}
}

// 自动定时扫描：VIDRIVE_SCAN_INTERVAL 解析成 Go duration，非法值保持关闭。
func TestLoad_ScanIntervalParsing(t *testing.T) {
	t.Setenv("VIDRIVE_SCAN_INTERVAL", "30m")
	if c := Load(); c.ScanInterval != 30*time.Minute {
		t.Errorf("ScanInterval = %v，期望 30m", c.ScanInterval)
	}
	t.Setenv("VIDRIVE_SCAN_INTERVAL", "1h")
	if c := Load(); c.ScanInterval != time.Hour {
		t.Errorf("ScanInterval = %v，期望 1h", c.ScanInterval)
	}
	// 未设置保持关闭（与 1.x 一致）。
	t.Setenv("VIDRIVE_SCAN_INTERVAL", "")
	if c := Load(); c.ScanInterval != 0 {
		t.Errorf("未设置时 ScanInterval = %v，期望 0（关闭）", c.ScanInterval)
	}
	// 非法值回退 0，绝不能变成疯狂扫描。
	t.Setenv("VIDRIVE_SCAN_INTERVAL", "not-a-duration")
	if c := Load(); c.ScanInterval != 0 {
		t.Errorf("非法值时 ScanInterval = %v，期望 0（关闭）", c.ScanInterval)
	}
	// 负值/零值同样关闭。
	t.Setenv("VIDRIVE_SCAN_INTERVAL", "-1h")
	if c := Load(); c.ScanInterval != 0 {
		t.Errorf("负值时 ScanInterval = %v，期望 0（关闭）", c.ScanInterval)
	}
}
