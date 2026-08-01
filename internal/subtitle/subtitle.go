// Package subtitle 字幕处理：同目录外挂字幕的语言识别，以及把各类字幕
// 统一转换成浏览器可直接播放的 WebVTT（.vtt）。
//
// 转换策略：
//   - .vtt  直接透传（已是 WebVTT）。
//   - .srt  纯 Go 转换（假设 UTF-8，覆盖绝大多数现代字幕）；非 UTF-8 时回退到 ffmpeg。
//   - .ass/.ssa 经 ffmpeg 转 WebVTT（浏览器原生不支持 ASS）。
//
// 这样播放器只需认一种格式（WebVTT），服务端把脏活揽了。
package subtitle

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
)

// langAliases 把文件名里可能出现的语言标记归一为 ISO 639-1 代码。
var langAliases = map[string]string{
	"zh": "zh", "chi": "zh", "chs": "zh", "cht": "zh", "sc": "zh", "tc": "zh",
	"cn": "zh", "gb": "zh", "big5": "zh", "zhcn": "zh", "zh-cn": "zh", "zhtw": "zh", "zh-tw": "zh",
	"en": "en", "eng": "en",
	"ja": "ja", "jpn": "ja",
	"ko": "ko", "kor": "ko",
	"fr": "fr", "fre": "fr", "fra": "fr",
	"de": "de", "ger": "de", "deu": "de",
	"es": "es", "spa": "es",
	"ru": "ru", "rus": "ru",
	"pt": "pt", "por": "pt",
	"it": "it", "ita": "it",
	"th": "th",
	"vi": "vi",
	"ar": "ar",
	"pl": "pl", "pol": "pl",
	"nl": "nl", "nld": "nl",
	"tr": "tr", "tur": "tr",
}

// langNames 显示名（优先中文，便于国内用户）。
var langNames = map[string]string{
	"zh": "简体中文", "en": "English", "ja": "日本語", "ko": "한국어",
	"fr": "Français", "de": "Deutsch", "es": "Español", "ru": "Русский",
	"pt": "Português", "it": "Italiano", "th": "ไทย", "vi": "Tiếng Việt",
	"ar": "العربية", "pl": "Polski", "nl": "Nederlands", "tr": "Türkçe",
}

// cjkLangNames 文件名里直接写中文语言名的情况。
var cjkLangNames = map[string]string{
	"简体中文": "zh", "简中": "zh", "中文": "zh", "国语": "zh",
	"繁體中文": "zh", "繁中": "zh", "粵語": "zh", "粤语": "zh",
	"英语": "en", "英文": "en",
	"日语": "ja", "日文": "ja",
	"韩语": "ko", "韩文": "ko",
}

// DetectLang 从字幕文件名推断语言代码与显示名。
// 例：Movie.zh.srt -> ("zh","简体中文")；Movie.eng.ass -> ("en","English")；
// 无语言标记（Movie.srt）-> ("und","默认")。
func DetectLang(filename string) (lang, title string) {
	base := filename
	if i := strings.LastIndex(filename, "."); i > 0 {
		base = filename[:i]
	}
	parts := strings.Split(base, ".")
	if len(parts) >= 2 {
		tok := strings.ToLower(strings.Trim(parts[len(parts)-1], "-_ "))
		if iso, ok := langAliases[tok]; ok {
			return iso, Name(iso)
		}
	}
	// 中文语言名直接出现在文件名里。
	for name, iso := range cjkLangNames {
		if strings.Contains(base, name) {
			return iso, Name(iso)
		}
	}
	return "und", "默认"
}

// Name 返回语言代码的显示名。
func Name(iso string) string {
	if n, ok := langNames[iso]; ok {
		return n
	}
	if iso == "und" {
		return "默认"
	}
	return strings.ToUpper(iso)
}

// IsSubtitleExt 判断扩展名是否为受支持的字幕格式。
func IsSubtitleExt(ext string) bool {
	switch strings.ToLower(ext) {
	case "srt", "vtt", "ass", "ssa":
		return true
	}
	return false
}

var counterRE = regexp.MustCompile(`^\d+$`)

// ConvertSRT 把 SRT（UTF-8）转换成 WebVTT，纯 Go 实现，无需外部依赖。
func ConvertSRT(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	if _, err := io.WriteString(w, "WEBVTT\n\n"); err != nil {
		return err
	}
	var buf bytes.Buffer
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
		start := 0
		// 跳过纯数字计数行（SRT 块首行常为序号）。
		if len(lines) > 0 && counterRE.MatchString(strings.TrimSpace(lines[0])) {
			start = 1
		}
		for i := start; i < len(lines); i++ {
			line := lines[i]
			// 时间轴 00:00:01,000 --> 00:00:04,000 的逗号改点。
			if i == start && strings.Contains(line, "-->") {
				line = strings.ReplaceAll(line, ",", ".")
			}
			io.WriteString(w, line)
			io.WriteString(w, "\n")
		}
		io.WriteString(w, "\n")
		buf.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
	}
	flush()
	return sc.Err()
}

// Convert 把任意受支持字幕转换为 WebVTT 写入 w。
// ext 为小写扩展名（srt/vtt/ass/ssa）。
func Convert(ext string, data []byte, w io.Writer) error {
	switch strings.ToLower(ext) {
	case "vtt":
		_, err := w.Write(data)
		return err
	case "srt":
		// 先尝试 UTF-8 纯 Go 转换；若源不是合法 UTF-8，回退 ffmpeg（能识别 GBK 等）。
		if utf8Valid(data) {
			return ConvertSRT(bytes.NewReader(data), w)
		}
		return convertViaFFmpeg(data, w)
	case "ass", "ssa":
		return convertViaFFmpeg(data, w)
	}
	return fmt.Errorf("不支持的字幕格式: %s", ext)
}

// convertViaFFmpeg 用 ffmpeg 把字幕转成 WebVTT（ASS 必须走这步；非 UTF-8 的 SRT 也走这步）。
func convertViaFFmpeg(data []byte, w io.Writer) error {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return fmt.Errorf("服务端未安装 ffmpeg，无法转换该字幕")
	}
	cmd := exec.Command(bin, "-loglevel", "error", "-i", "pipe:0", "-f", "webvtt", "pipe:1")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var ferr bytes.Buffer
	cmd.Stderr = &ferr
	if err := cmd.Start(); err != nil {
		return err
	}
	if _, err := io.Copy(w, out); err != nil {
		_ = cmd.Wait()
		return err
	}
	if err := cmd.Wait(); err != nil && ferr.Len() > 0 {
		return fmt.Errorf("ffmpeg 转字幕失败: %s", strings.TrimSpace(ferr.String()))
	}
	return nil
}

// utf8Valid 判断字节是否整体为合法 UTF-8（SRT 多为 UTF-8；GBK 等会命中失败）。
func utf8Valid(b []byte) bool {
	for i := 0; i < len(b); {
		r, size := decodeRune(b[i:])
		if r == 0xFFFD && size == 1 {
			return false
		}
		i += size
	}
	return true
}

// decodeRune 轻量 UTF-8 解码（仅用于合法性判断）。
func decodeRune(b []byte) (rune, int) {
	if len(b) == 0 {
		return 0xFFFD, 1
	}
	c := b[0]
	switch {
	case c < 0x80:
		return rune(c), 1
	case c >= 0xC2 && c <= 0xDF && len(b) >= 2 && b[1] >= 0x80 && b[1] <= 0xBF:
		return rune(c&0x1F)<<6 | rune(b[1]&0x3F), 2
	case c >= 0xE0 && c <= 0xEF && len(b) >= 3 &&
		b[1] >= 0x80 && b[1] <= 0xBF && b[2] >= 0x80 && b[2] <= 0xBF:
		return rune(c&0x0F)<<12 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F), 3
	case c >= 0xF0 && c <= 0xF4 && len(b) >= 4 &&
		b[1] >= 0x80 && b[1] <= 0xBF && b[2] >= 0x80 && b[2] <= 0xBF && b[3] >= 0x80 && b[3] <= 0xBF:
		return rune(c&0x07)<<18 | rune(b[1]&0x3F)<<12 | rune(b[2]&0x3F)<<6 | rune(b[3]&0x3F), 4
	}
	return 0xFFFD, 1
}
