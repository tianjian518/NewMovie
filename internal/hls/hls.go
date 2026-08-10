// Package hls 实现「请求驱动」的 HLS 按需生成（参考 Plex/Jellyfin 的 HLS 转码思路，
// 亦对标 Lunarr 的 HLS 流式方案）。
//
// 设计取舍：
//   - 复用 NewMovie 既有的 pipe:0 取流（openPlaySource 的 SSRF 守卫不变），ffmpeg 把源
//     切成分片写到磁盘缓存目录；浏览器经 hls.js 拉取这些静态分片。
//   - 单 ffmpeg 全量切片（而非「每分片独立起进程」）：与现有 remux/transcode 的 pipe 模型一致，
//     分片作为文件天然支持 Range 与拖动，起播快、seek 精准（分片边界即 GOP，独立可解）。
//   - 会话按「源 + 模式」去重（key=sha256(src|mode)）：同一文件重复播放不会起多个 ffmpeg；
//     TTL 过期后清理目录并杀掉残留进程，避免磁盘/CPU 堆积。
//
// 两种模式（与 selector 的 L2/L3 对应）：
//   - remux：ffmpeg -c copy（仅换容器为 TS），零重编码、几乎零开销，MKV→HLS 秒播；
//     音轨不兼容浏览器（DTS/TrueHD/Atmos）时转 AAC。
//   - transcode：ffmpeg libx264 + aac，HEVC→H.264 人人可播（依赖 ffmpeg 带 libx264）。
package hls

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// SegTime 单个 HLS 分片时长（秒）。分片越小拖动越精细、起播越快，但分片数越多、
	// 索引越大、ffmpeg 切片开销略增。6s 是 Plex/Jellyfin/Emby 的通用取值，兼顾流畅与开销。
	SegTime = 6
	// PlaylistName HLS 索引文件名（单码率时即主播放列表）。
	PlaylistName = "index.m3u8"
	// SegTmpl 分片文件名模板（ffmpeg -hls_segment_filename）。
	SegTmpl = "seg_%05d.ts"

	defaultMaxSessions = 4
	defaultTTL         = 2 * time.Hour
	PlaylistWait       = 60 * time.Second
	SegmentWait        = 120 * time.Second
	pollInterval       = 200 * time.Millisecond
)

// Manager 管理 HLS 按需生成会话。
type Manager struct {
	dir         string
	maxSessions int
	ttl         time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
	seq      uint64 // 单调递增序号，用于淘汰最久未访问的会话
}

// Session 单个 HLS 生成会话：一个 ffmpeg 进程把某源切成分片写到 dir。
type Session struct {
	key     string
	mode    string
	aac     bool
	atrack  int // >=0 时仅抽取该音轨；<0 保留全部
	dir     string
	open    func() (io.ReadCloser, error)
	cmd     *exec.Cmd
	src     io.ReadCloser
	started time.Time
	last    time.Time
	seq     uint64

	doneOnce sync.Once
	done     chan struct{}
	mu       sync.Mutex
	err      error
}

// New 构造 Manager。dir 为分片缓存根目录（不存在则创建）。
func New(dir string) *Manager {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "newmovie-hls")
	}
	_ = os.MkdirAll(dir, 0o755)
	return &Manager{
		dir:         dir,
		maxSessions: defaultMaxSessions,
		ttl:         defaultTTL,
		sessions:    map[string]*Session{},
	}
}

// Dir 返回缓存根目录。
func (m *Manager) Dir() string { return m.dir }

// KeyFor 计算会话 key（源 + 模式 + 音轨 的稳定哈希）。幂等：相同输入得相同 key。
// atrack<0 表示不指定音轨（保留全部）。
func KeyFor(raw, mode string, atrack int) string {
	h := sha256.Sum256([]byte(raw + "|" + mode + "|" + strconv.Itoa(atrack)))
	return hex.EncodeToString(h[:])
}

// Acquire 取得（或复用）某源的 HLS 会话。首次调用会启动后台 ffmpeg 切片，
// 后续调用幂等返回已有 key。open 用于打开播放源（SSRF 守卫后的取流），由调用方提供。
// atrack>=0 时仅抽取该音轨（多音轨 MKV 选语言用），不同音轨对应不同切片会话。
func (m *Manager) Acquire(raw, mode string, aac bool, atrack int, open func() (io.ReadCloser, error)) (string, error) {
	key := KeyFor(raw, mode, atrack)
	m.mu.Lock()
	if s, ok := m.sessions[key]; ok {
		s.touchLocked(m.nextSeqLocked())
		m.mu.Unlock()
		return key, nil
	}
	// 超出并发上限：先淘汰已完成的，再淘汰最久未访问的（保活正在看的会话）。
	if len(m.sessions) >= m.maxSessions {
		m.evictLocked()
	}
	sdir := filepath.Join(m.dir, key)
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("创建 HLS 缓存目录失败: %w", err)
	}
	s := &Session{
		key:     key,
		mode:    mode,
		aac:     aac,
		atrack:  atrack,
		dir:     sdir,
		open:    open,
		started: time.Now(),
		last:    time.Now(),
		seq:     m.nextSeqLocked(),
		done:    make(chan struct{}),
	}
	m.sessions[key] = s
	m.mu.Unlock()

	go s.run()
	return key, nil
}

// WaitPlaylist 等待并取回索引文件路径。超时或生成失败返回错误。
func (m *Manager) WaitPlaylist(key string, timeout time.Duration) (string, error) {
	s := m.session(key)
	if s == nil {
		return "", os.ErrNotExist
	}
	path := filepath.Join(s.dir, PlaylistName)
	deadline := time.Now().Add(timeout)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			s.touch()
			return path, nil
		}
		if e := s.loadErr(); e != nil {
			return "", e
		}
		if time.Now().After(deadline) {
			if e := s.loadErr(); e != nil {
				return "", e
			}
			return "", fmt.Errorf("HLS 索引超时未生成（源可能不可达或 ffmpeg 失败）")
		}
		time.Sleep(pollInterval)
	}
}

// WaitSegment 等待并取回分片文件路径。分片已写入磁盘才返回，供 http.ServeContent 带 Range 服务。
func (m *Manager) WaitSegment(key, name string, timeout time.Duration) (string, error) {
	s := m.session(key)
	if s == nil {
		return "", os.ErrNotExist
	}
	path := filepath.Join(s.dir, name)
	deadline := time.Now().Add(timeout)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			s.touch()
			return path, nil
		}
		if e := s.loadErr(); e != nil {
			return "", e
		}
		if time.Now().After(deadline) {
			if e := s.loadErr(); e != nil {
				return "", e
			}
			return "", fmt.Errorf("HLS 分片 %s 超时未生成", name)
		}
		time.Sleep(pollInterval)
	}
}

// Stop 杀掉所有 ffmpeg 并清理缓存目录（测试/进程退出时用）。
func (m *Manager) Stop() {
	m.mu.Lock()
	for _, s := range m.sessions {
		s.killLocked()
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	_ = os.RemoveAll(m.dir)
}

// StartCleanup 启动后台清理：周期性删除 TTL 内未访问的会话（先杀 ffmpeg 再删目录）。
func (m *Manager) StartCleanup(interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			m.cleanupOnce()
		}
	}()
}

func (m *Manager) cleanupOnce() {
	m.mu.Lock()
	now := time.Now()
	var expired []*Session
	for _, s := range m.sessions {
		s.mu.Lock()
		stale := now.Sub(s.last) > m.ttl
		s.mu.Unlock()
		if stale {
			expired = append(expired, s)
		}
	}
	for _, s := range expired {
		delete(m.sessions, s.key)
	}
	m.mu.Unlock()
	for _, s := range expired {
		s.kill()
		_ = os.RemoveAll(s.dir)
	}
}

func (m *Manager) session(key string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[key]
}

func (m *Manager) nextSeqLocked() uint64 {
	m.seq++
	return m.seq
}

// evictLocked 淘汰一个会话以腾出并发额度：优先已完成（ffmpeg 已退出），否则最久未访问。
// 调用方须持锁。
func (m *Manager) evictLocked() {
	var oldest *Session
	var doneCandidate *Session
	for _, s := range m.sessions {
		if doneCandidate == nil && s.isDone() {
			doneCandidate = s
		}
		if oldest == nil || s.seq < oldest.seq {
			oldest = s
		}
	}
	victim := doneCandidate
	if victim == nil {
		victim = oldest
	}
	if victim == nil {
		return
	}
	delete(m.sessions, victim.key)
	victim.killLocked()
	go func() { _ = os.RemoveAll(victim.dir) }()
}

// --- Session ---

func (s *Session) run() {
	defer s.closeDone()
	src, err := s.open()
	if err != nil {
		s.storeErr(fmt.Errorf("打开播放源失败: %w", err))
		return
	}
	s.src = src
	defer src.Close()

	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		s.storeErr(fmt.Errorf("未找到 ffmpeg: %w", err))
		return
	}
	args := BuildArgs(s.mode, s.aac, s.atrack)
	cmd := exec.Command(bin, args...)
	// 关键：ffmpeg 的分片（index.m3u8 / seg_*.ts）用相对路径写出，必须切到会话缓存目录，
	// 否则会落到进程 cwd（生产即服务运行目录），导致 WaitPlaylist 在 s.dir 里找不到分片。
	cmd.Dir = s.dir
	cmd.Stdin = src
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	s.mu.Lock()
	s.cmd = cmd
	s.mu.Unlock()
	if err := cmd.Start(); err != nil {
		s.storeErr(fmt.Errorf("启动 ffmpeg 失败: %w", err))
		return
	}
	if err := cmd.Wait(); err != nil {
		if stderr.Len() > 0 {
			log.Printf("[hls] ffmpeg 退出(%v): %s", err, stderr.String())
		}
		// 仅当连索引都没生成才记为错误；否则已产出的分片足够播放（event 播放列表尚未 ENDLIST）。
		if _, statErr := os.Stat(filepath.Join(s.dir, PlaylistName)); statErr != nil {
			s.storeErr(fmt.Errorf("ffmpeg 转封装失败: %w", err))
		}
	}
}

func (s *Session) touch() {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

func (s *Session) touchLocked(seq uint64) {
	s.mu.Lock()
	s.last = time.Now()
	s.seq = seq
	s.mu.Unlock()
}

func (s *Session) loadErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *Session) storeErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

func (s *Session) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Session) closeDone() {
	s.doneOnce.Do(func() { close(s.done) })
}

func (s *Session) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killLocked()
}

func (s *Session) killLocked() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// BuildArgs 构造 ffmpeg HLS 参数（移植并适配自 Lunarr 的 ffmpegHlsArgs 思路）。
//   - remux：视频拷贝（-c copy），仅换容器为 TS；音轨不兼容浏览器（DTS/TrueHD/Atmos）时转 AAC。
//   - transcode：libx264 + aac，GOP 与分片时长对齐（force_key_frames），保证每个分片独立可解。
//   - atrack>=0：显式映射视频+该音轨（多音轨 MKV 选语言用），其余流丢弃。
// 输出：index.m3u8 + seg_*.ts（event 播放列表，ffmpeg 边切边追加，结束写 #EXT-X-ENDLIST）。
func BuildArgs(mode string, aac bool, atrack int) []string {
	args := []string{"-loglevel", "error", "-i", "pipe:0"}
	if mode == "transcode" {
		args = append(args,
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p",
			"-c:a", "aac", "-b:a", "192k",
			// GOP 与分片对齐：每 SegTime 秒强制一个关键帧，分片边界即 GOP，seek 精准、首帧可解。
			"-force_key_frames", "expr:gte(t,n_forced*"+strconv.Itoa(SegTime)+")",
		)
	} else {
		if aac {
			args = append(args, "-c:v", "copy", "-c:a", "aac", "-b:a", "320k")
		} else {
			args = append(args, "-c", "copy")
		}
	}
	// 指定音轨：仅映射视频 + 该音轨（否则 ffmpeg 默认可能复制全部音轨，浪费且部分浏览器只读第一条）。
	if atrack >= 0 {
		args = append(args, "-map", "0:v:0", "-map", "0:a:"+strconv.Itoa(atrack))
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", strconv.Itoa(SegTime),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_filename", SegTmpl,
		PlaylistName,
	)
	return args
}

// RewritePlaylist 把索引里的分片相对路径改写为经本服务分片端点的绝对 URL，并注入 key 与 token。
// 分片端点用 ?key=<会话key> 定位会话（key 由 源+模式+音轨 稳定哈希，见 KeyFor），
// 用 ?token= 兜底鉴权（浏览器 <video> 拉分片带不上 Authorization 头，与 remux/transcode 的
// appendToken 思路一致）。相对路径改为 /api/play/hls/seg/<name>?key=...&token=...，
// 使分片请求与播放列表请求解耦于路径中的 key，音轨切换（atrack）也能正确落到新会话。
func RewritePlaylist(content []byte, token, key string) []byte {
	if token == "" && key == "" {
		return content
	}
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "#") && strings.HasPrefix(trim, "seg_") && strings.HasSuffix(trim, ".ts") {
			u := "/api/play/hls/seg/" + trim + "?key=" + key
			if token != "" {
				u += "&token=" + token
			}
			lines[i] = u
		}
	}
	return []byte(strings.Join(lines, "\n"))
}
