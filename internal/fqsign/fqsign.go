// Package fqsign 是番茄 App 端签名头的纯算实现。
//
// 六件套里本项目用到三个：
//
//	x-khronos / x-ss-req-ticket  时间戳（调用方直接生成）
//	x-ladon                      LadonEncrypt
//	x-argus                      Argus
//
// 上游哪天收紧到必须 X-Helios / X-Medusa，纯算就会失效。
package fqsign

import (
	"math/bits"
	"net/url"
	"strconv"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func rotateLeft64(v uint64, n uint) uint64  { return bits.RotateLeft64(v, int(n)) }
func rotateRight64(v uint64, n uint) uint64 { return bits.RotateLeft64(v, -int(n)) }

func reverseBytes(b []byte) {
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
}

// queryParam 等价于 url.Values.Get：取第一个同名参数。
func queryParam(query, name string) string {
	if query == "" {
		return ""
	}
	vs, err := url.ParseQuery(query)
	if err != nil {
		return ""
	}
	return vs.Get(name)
}
