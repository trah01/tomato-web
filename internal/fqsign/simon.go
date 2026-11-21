package fqsign

import "math/bits"

// Simon 分组密码（64 位分组 / 128 位密钥 / 72 轮），x-argus 用它加密 bean。
//
// key 已经是 4 个 64 位字（来自签名密钥的小端拆分），扩展成 72 个轮密钥。

const simonZ uint64 = 0x3DC9_4C3A_046D_678B

// SimonKeyExpansion 就地扩展轮密钥，key[0..4] 为初始密钥。
func SimonKeyExpansion(key *[72]uint64) {
	for i := 4; i < 72; i++ {
		tmp := bits.RotateLeft64(key[i-1], -3)
		tmp ^= key[i-3]
		tmp ^= bits.RotateLeft64(tmp, -1)
		bitVal := (simonZ >> uint((i-4)%62)) & 1
		key[i] = ^key[i-4] ^ tmp ^ bitVal ^ 3
	}
}

// SimonEnc 加密一个 128 位分组。
func SimonEnc(pt [2]uint64, k [4]uint64) [2]uint64 {
	var key [72]uint64
	key[0], key[1], key[2], key[3] = k[0], k[1], k[2], k[3]
	SimonKeyExpansion(&key)

	xi, xi1 := pt[0], pt[1]
	for r := 0; r < 72; r++ {
		tmp := xi1
		f := bits.RotateLeft64(xi1, 1) & bits.RotateLeft64(xi1, 8)
		xi1 = xi ^ f ^ bits.RotateLeft64(xi1, 2) ^ key[r]
		xi = tmp
	}
	return [2]uint64{xi, xi1}
}

// SimonDec 是 SimonEnc 的逆运算，仅用于离线校验。
func SimonDec(ct [2]uint64, k [4]uint64) [2]uint64 {
	var key [72]uint64
	key[0], key[1], key[2], key[3] = k[0], k[1], k[2], k[3]
	SimonKeyExpansion(&key)

	left, right := ct[0], ct[1]
	for i := 71; i >= 0; i-- {
		tmp := left
		f := bits.RotateLeft64(left, 1) & bits.RotateLeft64(left, 8)
		left = right ^ f ^ bits.RotateLeft64(left, 2) ^ key[i]
		right = tmp
	}
	return [2]uint64{left, right}
}
