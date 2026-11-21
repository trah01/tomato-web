package fqsign

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
)

// x-ladon：34 轮 ARX 自研块密码，输出 base64(随机4字节 ‖ 密文)。

// LadonExpandHashTable 用 32 位小写 md5 hex 生成 288 字节置换表。
func LadonExpandHashTable(md5Hex string) [288]byte {
	var ht [288]byte
	copy(ht[:], md5Hex)

	load := func(off int) uint64 {
		var v uint64
		for j := 0; j < 8; j++ {
			v |= uint64(ht[off+j]) << (j * 8)
		}
		return v
	}

	temp := []uint64{load(0), load(8), load(16), load(24)}
	bufB0, bufB8 := temp[0], temp[1]
	// 与上游的同构队列：push 刚算出的值，取队首作下一轮的 bufB8 后丢弃。
	temp = temp[2:]

	for i := 0; i < 34; i++ {
		x9 := bufB0
		x8 := rotateRight64(bufB8, 8)
		x8 += x9
		x8 ^= uint64(i)
		temp = append(temp, x8)

		x8 ^= rotateLeft64(x9, 3)

		for j := 0; j < 8; j++ {
			ht[(i+1)*8+j] = byte((x8 >> (j * 8)) & 255)
		}

		bufB0 = x8
		bufB8 = temp[0]
		temp = temp[1:]
	}
	return ht
}

// LadonEncryptBlock 加密一个 16 字节块。
func LadonEncryptBlock(ht *[288]byte, block []byte) [16]byte {
	var data0, data1 uint64
	for i := 0; i < 8; i++ {
		data0 |= uint64(block[i]) << (i * 8)
		data1 |= uint64(block[8+i]) << (i * 8)
	}

	for i := 0; i < 34; i++ {
		var hash uint64
		for j := 0; j < 8; j++ {
			hash |= uint64(ht[i*8+j]) << (j * 8)
		}
		sum := data0 + rotateRight64(data1, 8)
		data1 = hash ^ sum
		data0 = data1 ^ rotateLeft64(data0, 3)
	}

	var out [16]byte
	for i := 0; i < 8; i++ {
		out[i] = byte((data0 >> (i * 8)) & 255)
		out[8+i] = byte((data1 >> (i * 8)) & 255)
	}
	return out
}

// LadonEncryptRaw 对任意长度数据做 PKCS7 对齐后加密。
func LadonEncryptRaw(md5Hex string, data []byte) []byte {
	ht := LadonExpandHashTable(md5Hex)

	size := len(data)
	newSize := size + (16 - size%16)
	padVal := byte(newSize - size)
	input := make([]byte, newSize)
	for i := range input {
		input[i] = padVal
	}
	copy(input, data)

	out := make([]byte, 0, newSize)
	for i := 0; i < newSize/16; i++ {
		blk := LadonEncryptBlock(&ht, input[i*16:i*16+16])
		out = append(out, blk[:]...)
	}
	return out
}

// LadonEncrypt 生成 x-ladon 头：khronos-lcId-aid 用 md5(rand4+aid) 派生的表加密。
func LadonEncrypt(khronos, lcID, aid int64, randomBytes []byte) string {
	data := []byte(itoa(khronos) + "-" + itoa(lcID) + "-" + itoa(aid))

	keygen := append(append([]byte(nil), randomBytes...), []byte(itoa(aid))...)
	// x-ladon 的表种子由上游固定为 md5（不是本项目的安全哈希选择），
	// 换成 sha256 会产出上游不认的签名头，故此处必须用 md5。
	sum := md5.Sum(keygen) // pi-lens-ignore: go-weak-hash -- 上游协议固定，非安全用途
	md5Hex := hex.EncodeToString(sum[:])

	encrypted := LadonEncryptRaw(md5Hex, data)
	out := append(append([]byte(nil), randomBytes...), encrypted...)
	return base64.StdEncoding.EncodeToString(out)
}
