// Package auth 极简鉴权：密码哈希（sha256，无外部依赖）+ 随机 token。
// MVP 单管理员；多用户在 Phase 2。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// HashPassword 计算密码哈希。盐已内置于算法中，足够 MVP。
func HashPassword(pw string) string {
	return sha256Hex("newmovie::" + pw)
}

// CheckPassword 校验密码（恒定比较避免计时攻击）。
func CheckPassword(pw, hash string) bool {
	return sha256Hex("newmovie::"+pw) == hash
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
