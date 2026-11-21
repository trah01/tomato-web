package fqsign

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"strconv"
)

// x-argus：有序 protobuf bean → PKCS7 → Simon → 551 帧 → AES-128-CBC → base64。

const (
	argusSignKeyHex = "fc78e0a9657a0c748ce51559903ccf03510e51d3cff232d71343e88a321c5304"
	argusAESKeyHex  = "d31e3718288a1027baab59f146a09a9c"
	argusAESIvHex   = "ea180a0336ed352fcd24e4d50018ae54"
)

var (
	argus551Prefix = []byte{0xf2, 0x81}
	argus551Magic8 = []byte{0xf2, 0xf7, 0xfc, 0xff, 0xf2, 0xf7, 0xfc, 0xff}
	argus551Tail9  = []byte{0xea, 0xfb, 0x2c, 0xfe, 0x85, 0x68, 0x51, 0x91, 0x54}
)

// ArgusOptions 是生成 x-argus 的输入。
type ArgusOptions struct {
	// QueryString 必须是实际发出的原始查询串（顺序即签名输入）。
	QueryString string
	// StubHex 是请求体的 MD5（小写 hex）；GET 请求留空。
	StubHex   string
	Timestamp int64
	AID       int64
	LicenseID int64
	DeviceID  string
	// InstallID 写入 bean 字段 16，可为空。
	InstallID string
	// RandField 是 bean 字段 3 的 nonce；为 0 时调用方需自行随机。
	RandField int32
}

func argusSignKeyList() [4]uint64 {
	key, _ := hex.DecodeString(argusSignKeyHex)
	var out [4]uint64
	for i := 0; i < 4; i++ {
		var v uint64
		for j := 0; j < 8; j++ {
			v |= uint64(key[i*8+j]) << (j * 8)
		}
		out[i] = v
	}
	return out
}

// ArgusBodyHash 返回 sm3(stub)[:6]；stub 为空时按 16 个零字节计算。
func ArgusBodyHash(stubHex string) [6]byte {
	var sum [32]byte
	if stubHex == "" {
		sum = SM3Sum(make([]byte, 16))
	} else {
		raw, err := hex.DecodeString(stubHex)
		if err != nil {
			raw = nil
		}
		sum = SM3Sum(raw)
	}
	var out [6]byte
	copy(out[:], sum[:6])
	return out
}

// ArgusQueryHash 返回 sm3(query)[:6]；query 为空时按 16 个零字节计算。
func ArgusQueryHash(query string) [6]byte {
	var sum [32]byte
	if query == "" {
		sum = SM3Sum(make([]byte, 16))
	} else {
		sum = SM3Sum([]byte(query))
	}
	var out [6]byte
	copy(out[:], sum[:6])
	return out
}

func argusSimonsPadded(protobuf []byte) []byte {
	key := argusSignKeyList()
	out := make([]byte, len(protobuf))
	for i := 0; i+16 <= len(protobuf); i += 16 {
		var a, b uint64
		for j := 0; j < 8; j++ {
			a |= uint64(protobuf[i+j]) << (j * 8)
			b |= uint64(protobuf[i+8+j]) << (j * 8)
		}
		ct := SimonEnc([2]uint64{a, b}, key)
		for j := 0; j < 8; j++ {
			out[i+j] = byte((ct[0] >> (j * 8)) & 255)
			out[i+8+j] = byte((ct[1] >> (j * 8)) & 255)
		}
	}
	return out
}

func pkcs7Align(data *[]byte) {
	padLen := 16 - len(*data)%16
	*data = append(*data, make([]byte, padLen)...)
	for i := len(*data) - padLen; i < len(*data); i++ {
		(*data)[i] = byte(padLen)
	}
}

func aesCBCEncrypt(data, key, iv []byte) []byte {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(out, data)
	return out
}

// argusEncrypt551 是当前上游接受的封装。
func argusEncrypt551(bean *ProtoBuf) string {
	protobuf := bean.ToBuf()
	pkcs7Align(&protobuf)
	encPb := argusSimonsPadded(protobuf)

	un := make([]byte, 0, 8+len(encPb)+9)
	un = append(un, argus551Magic8...)
	un = append(un, encPb...)
	un = append(un, argus551Tail9...)

	buf := make([]byte, len(un))
	copy(buf, argus551Magic8)
	for i := 8; i < len(un); i++ {
		buf[i] = un[i] ^ argus551Magic8[i%8]
	}
	reverseBytes(buf)
	buf = append(buf, 'a', 'o')
	pkcs7Align(&buf)

	aesKey, _ := hex.DecodeString(argusAESKeyHex)
	aesIv, _ := hex.DecodeString(argusAESIvHex)
	out := aesCBCEncrypt(buf, aesKey, aesIv)

	raw := append(append([]byte(nil), argus551Prefix...), out...)
	return base64.StdEncoding.EncodeToString(raw)
}

// Argus 生成 x-argus 头。
func Argus(p ArgusOptions) string {
	aid := p.AID
	if aid == 0 {
		aid = 1967
	}
	licenseID := p.LicenseID
	if licenseID == 0 {
		licenseID = 1_611_921_764
	}

	bean := NewProto()
	bean.PutVarint(1, 1_077_940_818)
	bean.PutVarint(2, 2)
	bean.PutVarint(3, int64(p.RandField))
	bean.PutUTF8(4, strconv.FormatInt(aid, 10))
	bean.PutUTF8(5, p.DeviceID)
	bean.PutUTF8(6, strconv.FormatInt(licenseID, 10))
	bean.PutUTF8(7, queryParam(p.QueryString, "version_name"))
	bean.PutUTF8(8, "v04.04.05-ov-android")
	bean.PutVarint(9, 134_744_640)
	bean.PutBytes(10, make([]byte, 8))
	bean.PutVarint(11, 0)
	bean.PutVarint(12, p.Timestamp<<1)
	bodyHash := ArgusBodyHash(p.StubHex)
	bean.PutBytes(13, bodyHash[:])
	queryHash := ArgusQueryHash(p.QueryString)
	bean.PutBytes(14, queryHash[:])

	sub15 := NewProto()
	sub15.PutVarint(1, 1)
	sub15.PutVarint(2, 1)
	sub15.PutVarint(3, 1)
	sub15.PutVarint(7, 0xC792_ECCC)
	bean.PutProto(15, sub15)

	bean.PutUTF8(16, p.InstallID)
	bean.PutUTF8(20, "none")
	bean.PutVarint(21, 738)

	sub23 := NewProto()
	sub23.PutUTF8(1, "NX551J")
	sub23.PutVarint(2, 8196)
	sub23.PutVarint(4, 0x80E0_D800)
	bean.PutProto(23, sub23)

	bean.PutVarint(25, 2)

	return argusEncrypt551(bean)
}
