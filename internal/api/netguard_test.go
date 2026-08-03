package api

import (
	"context"
	"errors"
	"net"
	"testing"
)

// TestIsBlockedIP 逐项校验 SSRF 拦截清单：回环 / 私网 / 链路本地 / 云元数据 /
// 运营商级 NAT / 基准测试网段 / 广播 都应被拦；公网地址放行。
func TestIsBlockedIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},                 // 回环
		{"::1", true},                       // IPv6 回环
		{"10.0.0.1", true},                  // 私网 A
		{"172.16.0.1", true},                // 私网 B
		{"192.168.1.1", true},               // 私网 C
		{"169.254.169.254", true},           // 云元数据
		{"169.254.0.1", true},               // 链路本地
		{"fe80::1", true},                   // IPv6 链路本地
		{"100.64.0.1", true},                // 运营商级 NAT (100.64/10)
		{"100.127.255.254", true},           // 运营商级 NAT 上界
		{"192.0.0.1", true},                 // IETF 协议分配
		{"198.18.0.1", true},                // 基准测试网段（沙箱 DNS 劫持落点）
		{"198.19.255.254", true},            // 基准测试网段上界
		{"0.0.0.0", true},                   // 0.0.0.0/8
		{"255.255.255.255", true},           // 广播
		{"8.8.8.8", false},                  // 公网，放行
		{"1.1.1.1", false},                  // 公网，放行
		{"93.184.216.34", false},            // example.com，放行
	}
	for _, c := range cases {
		got := isBlockedIP(net.ParseIP(c.ip))
		if got != c.want {
			t.Errorf("isBlockedIP(%s)=%v, want %v", c.ip, got, c.want)
		}
	}
}

// TestGuardedDial_StrictBlocksLoopback 严格模式下直接给 IP 也应被拦。
func TestGuardedDial_StrictBlocksLoopback(t *testing.T) {
	dial := guardedDial(false)
	_, err := dial(context.Background(), "tcp", "127.0.0.1:80")
	if !errors.Is(err, errBlockedTarget) {
		t.Fatalf("期望 errBlockedTarget，得到 %v", err)
	}
}

// TestGuardedDial_StrictBlocksLocalhostHostname 主机名形式也会被解析后校验，
// 解析到回环即拦截——这正是修复 DNS-rebind TOCTOU 后应有的行为。
func TestGuardedDial_StrictBlocksLocalhostHostname(t *testing.T) {
	dial := guardedDial(false)
	_, err := dial(context.Background(), "tcp", "localhost:80")
	if !errors.Is(err, errBlockedTarget) {
		t.Fatalf("期望 errBlockedTarget，得到 %v", err)
	}
}

// TestGuardedDial_AllowPrivateDials 已配置存储源（allowPrivate=true）可直连内网。
func TestGuardedDial_AllowPrivateDials(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	dial := guardedDial(true)
	conn, err := dial(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("allowPrivate 下应可 dial 本地监听：%v", err)
	}
	conn.Close()
}
