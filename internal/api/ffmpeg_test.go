package api

// 测试共享辅助：探测本机 ffmpeg 可用的 H.264 编码器。
//
// 不同构建的 ffmpeg 编码器集合不同（Ubuntu 发行版通常是 libx264，
// 某些精简构建只有 libopenh264），测试不能硬编码某一个编码器，
// 否则换一台机器 / CI 就会 fail。这里按优先级探测：
//   libx264（最通用）→ libopenh264 → 都没有则报错（调用方跳过）。
import (
	"os/exec"
	"strings"
	"testing"
)

// probeEncoder 检查 ffmpeg 是否带指定编码器。
func probeEncoder(name string) bool {
	out, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return false
	}
	// 编码器列表形如 " V..... libx264  libx264 H.264..."，用 " libx264 " 匹配避免误命中子串。
	return strings.Contains(string(out), " "+name+" ")
}

// h264EncoderForTest 返回一个可用的 H.264 编码器名；探测不到返回空串。
// 探测结果缓存，避免每个测试都 exec 一次。
var h264EncoderForTest = func() string {
	for _, enc := range []string{"libx264", "libopenh264"} {
		if probeEncoder(enc) {
			return enc
		}
	}
	return ""
}()

// requireH264Encoder 返回可用编码器；没有则 t.Skip 并说明原因。
func requireH264Encoder(t *testing.T) string {
	t.Helper()
	if h264EncoderForTest == "" {
		t.Skip("ffmpeg 没有可用的 H.264 编码器（libx264 / libopenh264），跳过")
	}
	return h264EncoderForTest
}
