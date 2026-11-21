package fqapi

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"strconv"
	"time"

	"github.com/local/fanqieweb/internal/fqsign"
)

// PureSigner 是本地纯算签名器：不依赖任何外部服务。
//
// 与 RemoteSigner 一样实现 Signer；额外实现 SignBody，让 POST 请求体
// 的 MD5 参与 x-argus 的 body 哈希。
type PureSigner struct {
	AID       int64
	LicenseID int64
}

// NewPureSigner 创建纯算签名器（默认参数与番茄 App 一致）。
func NewPureSigner() *PureSigner {
	return &PureSigner{AID: 1967, LicenseID: 1611921764}
}

// Sign 实现 Signer：GET 请求（无请求体）。
func (p *PureSigner) Sign(q url.Values, cookie string) map[string]string {
	return p.sign(q, nil)
}

// SignBody 实现 BodySigner：带请求体的 POST。
func (p *PureSigner) SignBody(q url.Values, body []byte) map[string]string {
	return p.sign(q, body)
}

// BodySigner 由需要请求体参与签名的 Signer 实现。
type BodySigner interface {
	SignBody(q url.Values, body []byte) map[string]string
}

func (p *PureSigner) sign(q url.Values, body []byte) map[string]string {
	aid := p.AID
	if aid == 0 {
		aid = 1967
	}
	lc := p.LicenseID
	if lc == 0 {
		lc = 1611921764
	}

	ts := time.Now().Unix()
	// 签名输入必须与实际发出的查询串完全一致。
	query := q.Encode()

	stub := ""
	if len(body) > 0 {
		// x-ss-stub 由上游协议固定为请求体的 MD5，不能换成更强的哈希。
		sum := md5.Sum(body) // pi-lens-ignore: go-weak-hash -- 上游协议规定用 MD5 作请求体摘要
		stub = hex.EncodeToString(sum[:])
	}

	out := map[string]string{
		"x-ss-req-ticket": strconv.FormatInt(ts*1000, 10),
		"x-khronos":       strconv.FormatInt(ts, 10),
		"x-ladon":         fqsign.LadonEncrypt(ts, lc, aid, randomBytes(4)),
		"x-argus": fqsign.Argus(fqsign.ArgusOptions{
			QueryString: query,
			StubHex:     stub,
			Timestamp:   ts,
			AID:         aid,
			LicenseID:   lc,
			DeviceID:    q.Get("device_id"),
			RandField:   randomInt32(),
		}),
	}
	if stub != "" {
		out["x-ss-stub"] = stub
	}
	return out
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		for i := range b {
			b[i] = byte(i * 7 % 251)
		}
	}
	return b
}

func randomInt32() int32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return int32(time.Now().UnixNano() & 0x7fffffff)
	}
	return int32(binary.LittleEndian.Uint32(b[:]))
}
