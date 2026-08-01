// Package auth 极简鉴权：密码哈希（sha256，无外部依赖）+ 随机 token。
// MVP 单管理员；多用户在 Phase 2。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"time"
)

// HashPassword 计算密码哈希。盐已内置于算法中，足够 MVP。
func HashPassword(pw string) string {
	return sha256Hex("newmovie::" + pw)
}

// CheckPassword 校验密码。
//
// 用 subtle.ConstantTimeCompare 而不是 `==`：Go 的字符串比较是短路的，
// 前缀错得越早返回越快，攻击者可以据此逐字节把哈希试出来。
// 注释以前写着「恒定比较」，实现却是 `==` —— 名不副实，这里补上。
func CheckPassword(pw, hash string) bool {
	got := sha256Hex("newmovie::" + pw)
	return subtle.ConstantTimeCompare([]byte(got), []byte(hash)) == 1
}

// NewToken 生成随机会话 token。
func NewToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// GenID 生成带前缀的唯一 ID（时间+随机，非加密）。
func GenID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b) + "-" + hex.EncodeToString([]byte(time.Now().Format("050102150405")))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
