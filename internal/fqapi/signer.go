package fqapi

import (
	"crypto/md5"
	"encoding/hex"
	"net/url"
)

// Signer 生成番茄 App 端请求签名头。
//
// 本版本只保留本地纯算实现（PureSigner）；签名输入与请求参数的对应关系
// 随上游 App 版本变化，改动前先看 pure.go。
type Signer interface {
	Sign(query url.Values, cookie string) map[string]string
}

// md5hex 返回小写十六进制 MD5，用于 x-ss-stub 请求头。
func md5hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
