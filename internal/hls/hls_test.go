package hls

import (
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func containsAll(t *testing.T, args []string, want ...string) {
	t.Helper()
	for _, w := range want {
		found := false
		for _, a := range args {
			if a == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("BuildArgs 缺少预期参数 %q；实际：%v", w, args)
		}
	}
}

func TestBuildArgsRemuxCopy(t *testing.T) {
	args := BuildArgs("remux", false, -1)
	containsAll(t, args, "-c", "copy", "-f", "hls", "-hls_time", "6",
		"-hls_playlist_type", "event", "-hls_segment_filename", SegTmpl, PlaylistName)
}

func TestBuildArgsRemuxAAC(t *testing.T) {
	args := BuildArgs("remux", true, -1)
	containsAll(t, args, "-c:v", "copy", "-c:a", "aac")
	if has(args, "-c") {
		t.Errorf("remux+aac 不应出现 -c copy 整体覆盖：%v", args)
	}
}

func TestBuildArgsTranscode(t *testing.T) {
	args := BuildArgs("transcode", false, -1)
	containsAll(t, args, "-c:v", "libx264", "-preset", "veryfast",
		"-crf", "20", "-force_key_frames", "expr:gte(t,n_forced*6)")
}

func has(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestKeyForDeterministic(t *testing.T) {
	a := KeyFor("http://x/1.mkv", "remux", -1)
	b := KeyFor("http://x/1.mkv", "remux", -1)
	c := KeyFor("http://x/1.mkv", "transcode", -1)
	if a != b {
		t.Errorf("同输入应得相同 key：%s != %s", a, b)
	}
	if a == c {
		t.Errorf("不同模式应得不同 key")
	}
	// 64 位 hex = 32 字节 sha256
	if _, err := hex.DecodeString(a); err != nil || len(a) != 64 {
		t.Errorf("key 应为 64 位 hex：%q", a)
	}
}

func TestRewritePlaylist(t *testing.T) {
	in := []byte("#EXTM3U\n#EXTINF:6.0,\nseg_00000.ts\n#EXT-X-ENDLIST\n")
	out := RewritePlaylist(in, "TOK123", "")
	got := string(out)
	if !strings.Contains(got, "/api/play/hls/seg/seg_00000.ts?key=&token=TOK123") {
		t.Errorf("分片未改写为带 key/token 的分片端点：%q", got)
	}
	if !strings.Contains(got, "#EXTINF:6.0,") {
		t.Errorf("注释行不应被改写：%q", got)
	}
	// 空 token + 空 key：原样返回
	if string(RewritePlaylist(in, "", "")) != string(in) {
		t.Errorf("空 token 且空 key 应原样返回")
	}
	// 仅 key、无 token：不应出现 &token=
	keyed := RewritePlaylist(in, "", "DEADBEEF")
	if !strings.Contains(string(keyed), "/api/play/hls/seg/seg_00000.ts?key=DEADBEEF") {
		t.Errorf("仅 key 时应出现 ?key=DEADBEEF：%q", string(keyed))
	}
	if strings.Contains(string(keyed), "&token=") {
		t.Errorf("无 token 时不应出现 &token=：%q", string(keyed))
	}
}

// TestManagerGenerate 真实跑一次 ffmpeg（-c copy → TS）验证 HLS 全链路：
// 生成测试 mp4 → Manager.Acquire → 等待索引 → 等待分片 → 校验分片存在且非空。
// 仅依赖 ffmpeg 的「拷贝」能力（无需 libx264），沙箱即可验证。
func TestManagerGenerate(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("未安装 ffmpeg，跳过 HLS 生成集成测试")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "sample.mkv")
	// 造一个 13 秒测试片：MKV 容器可从管道流式读取（对标 NewMovie 真实 remux 源），
	// 视频用原生 mpeg2video（TS 兼容、沙箱无需 libx264）。真实场景下源多为 MKV(h264)，
	// 同样走 -c copy 入 TS；此处仅用沙箱可用的编码器验证 HLS 全链路。
	cmd := exec.Command(bin, "-y", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration=13:size=320x240:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=13",
		"-c:v", "mpeg2video", "-c:a", "aac", "-shortest", "-f", "matroska", src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成测试片失败: %v\n%s", err, out)
	}

	mgr := New(filepath.Join(dir, "hls"))
	defer mgr.Stop()
	// opener 直接读本地文件，模拟 openPlaySource 取流。
	open := func() (io.ReadCloser, error) {
		return os.Open(src)
	}
	key, err := mgr.Acquire("file://"+src, "remux", false, -1, open)
	if err != nil {
		t.Fatalf("Acquire 失败: %v", err)
	}

	plPath, err := mgr.WaitPlaylist(key, PlaylistWait)
	if err != nil {
		t.Fatalf("等待索引失败: %v", err)
	}
	content, err := os.ReadFile(plPath)
	if err != nil {
		t.Fatalf("读索引失败: %v", err)
	}
	if !strings.Contains(string(content), "seg_00000.ts") {
		t.Fatalf("索引未引用首个分片：\n%s", content)
	}

	// 12 秒 / 6 秒 ≈ 2~3 个分片，至少应有 seg_00000.ts 与 seg_00001.ts。
	for _, name := range []string{"seg_00000.ts", "seg_00001.ts"} {
		segPath, err := mgr.WaitSegment(key, name, SegmentWait)
		if err != nil {
			t.Fatalf("等待分片 %s 失败: %v", name, err)
		}
		fi, err := os.Stat(segPath)
		if err != nil {
			t.Fatalf("分片 %s 不存在: %v", name, err)
		}
		if fi.Size() < 1000 {
			t.Errorf("分片 %s 过小（%d 字节），可能不是合法 TS", name, fi.Size())
		}
	}
}
