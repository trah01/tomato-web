package fqsign

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"testing"
)

// 黄金向量来自对照实现 core/testdata/vectors.json（由独立实现产出），
// 逐字节比对才能证明移植没有偏移。

func TestSM3Vectors(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty", nil, "1ab21d8355cfa17f8e61194831e81a8f22bec8c728fefb747ed035eb5082aa2b"},
		{"abc", []byte("abc"), "66c7f0f462eeedd9d1f2d46bdc10e4e24167c4875cf2f7a2297da02b8f4ba8e0"},
		{"zero16", make([]byte, 16), "106e34a2b8c7bb13156cfdd0d91379dcc47543dcf9787c68ae5eb582620ae6e8"},
		{"long", bytes.Repeat([]byte("a"), 100), "0c105d5a46a65fdf0a0938283db2517ea87f176de84786f443cb78802aaa03de"},
		{"query", []byte("aid=1967&device_id=123&iid=456"), "81c65adb29f5b7912512e81103813432471e6f72311425a111741459d03eee5f"},
	}
	for _, c := range cases {
		if got := hex.EncodeToString(func() []byte { s := SM3Sum(c.in); return s[:] }()); got != c.want {
			t.Errorf("sm3[%s] = %s, want %s", c.name, got, c.want)
		}
	}
}

func TestSimonVectors(t *testing.T) {
	pt := [2]uint64{1234567890123456789, 9876543210987654321}
	k := [4]uint64{1111111111111111111, 2222222222222222222, 3333333333333333333, 4444444444444444444}
	ct := SimonEnc(pt, k)
	if ct[0] != 6071150433487837324 || ct[1] != 2027321285990159935 {
		t.Fatalf("simon ct = %v, want [6071150433487837324 2027321285990159935]", ct)
	}
	if back := SimonDec(ct, k); back != pt {
		t.Fatalf("simon round trip = %v, want %v", back, pt)
	}

	var key [72]uint64
	key[0], key[1], key[2], key[3] = k[0], k[1], k[2], k[3]
	SimonKeyExpansion(&key)
	want := [4]uint64{13679004903447286117, 6988159389011166349, 12845848347567849394, 14469362449063813004}
	for i := 0; i < 4; i++ {
		if key[68+i] != want[i] {
			t.Errorf("keyexp[%d] = %d, want %d", 68+i, key[68+i], want[i])
		}
	}
}

func TestLadonVectors(t *testing.T) {
	rb, _ := hex.DecodeString("01020304")
	got := LadonEncrypt(1700000000, 1611921764, 1967, rb)
	if got != "AQIDBKM7Y7bWnOIUXmTybD6/MpGbuge4sBwUJsyHVivhtMsH" {
		t.Fatalf("ladon = %s", got)
	}

	raw := LadonEncryptRaw("53136271c432a1af377c3806c3112ddf", []byte("hello-world-data"))
	if hex.EncodeToString(raw) != "995103f804b4fa834266bf7ff4e382563c5745c587456a7ca3cb861c00f487e3" {
		t.Fatalf("ladon_raw = %s", hex.EncodeToString(raw))
	}
}

func TestArgusHashes(t *testing.T) {
	bodyNull := ArgusBodyHash("")
	if got := hex.EncodeToString(bodyNull[:]); got != "106e34a2b8c7" {
		t.Errorf("bodyhash(null) = %s", got)
	}
	bodyStub := ArgusBodyHash("43cd1e8bd4a1a9c1f4a6b2a7d5f8e9c0")
	if got := hex.EncodeToString(bodyStub[:]); got == "106e34a2b8c7" {
		t.Errorf("bodyhash(stub) 未随 stub 变化: %s", got)
	}
	queryNull := ArgusQueryHash("")
	if got := hex.EncodeToString(queryNull[:]); got != "106e34a2b8c7" {
		t.Errorf("queryhash(null) = %s", got)
	}
	queryHash := ArgusQueryHash("aid=1967&device_id=123&iid=456")
	// 自一致性：query 哈希就是 sm3(query) 的前 6 字节（sm3 本身已由黄金向量覆盖）。
	querySum := SM3Sum([]byte("aid=1967&device_id=123&iid=456"))
	if hex.EncodeToString(queryHash[:]) != hex.EncodeToString(querySum[:6]) {
		t.Errorf("queryhash 与 sm3(query)[:6] 不一致: %s", hex.EncodeToString(queryHash[:]))
	}
	// 交叉验证：protobuf_bean 黄金向量里的字段 14 就是这个查询串的结果。
	beanQueryHash := ArgusQueryHash("aid=1967&device_id=test_device_id&iid=456")
	if got := hex.EncodeToString(beanQueryHash[:]); got != "ac5095fc674e" {
		t.Errorf("queryhash(test_device_id) = %s, want ac5095fc674e", got)
	}
}

const testProtobufBean = "08d2a4808204100218f8acd191012204313936372a0e746573745f6465766963655f6964320a313631313932313736343a004200480052005a006080c49fd50c6a06106e34a2b8c77206ac5095fc674e7a0c08011001180138ccd9cbbc0c820100a201046e6f6e65a801e205ba01110a064e583535314a1084402080b0838708c80102"

// buildTestBean 复现黄金向量使用的 legacy bean 布局。
func buildTestBean() *ProtoBuf {
	bean := NewProto()
	bean.PutVarint(1, 1_077_940_818)
	bean.PutVarint(2, 2)
	bean.PutVarint(3, 305_419_896)
	bean.PutUTF8(4, "1967")
	bean.PutUTF8(5, "test_device_id")
	bean.PutUTF8(6, "1611921764")
	bean.PutUTF8(7, "")
	bean.PutUTF8(8, "")
	bean.PutVarint(9, 0)
	bean.PutUTF8(10, "")
	bean.PutUTF8(11, "")
	bean.PutVarint(12, 1_700_000_000<<1)
	bh := ArgusBodyHash("")
	bean.PutBytes(13, bh[:])
	qh := ArgusQueryHash("aid=1967&device_id=test_device_id&iid=456")
	bean.PutBytes(14, qh[:])

	sub15 := NewProto()
	sub15.PutVarint(1, 1)
	sub15.PutVarint(2, 1)
	sub15.PutVarint(3, 1)
	sub15.PutVarint(7, 0xC792_ECCC)
	bean.PutProto(15, sub15)

	bean.PutUTF8(16, "")
	bean.PutUTF8(20, "none")
	bean.PutVarint(21, 738)

	sub23 := NewProto()
	sub23.PutUTF8(1, "NX551J")
	sub23.PutVarint(2, 8196)
	sub23.PutVarint(4, 0x80E0_D800)
	bean.PutProto(23, sub23)

	bean.PutVarint(25, 2)
	return bean
}

func TestProtobufBeanVector(t *testing.T) {
	if got := hex.EncodeToString(buildTestBean().ToBuf()); got != testProtobufBean {
		t.Fatalf("protobuf bean\n got %s\nwant %s", got, testProtobufBean)
	}
}

// x-argus 551 的黄金向量。field3 是每次随机的 nonce，所以先从黄金输出
// 反解出 field3 再喂回去——这样仍能逐字节验证所有固定字段、编帧与常量。
func TestArgus551Vectors(t *testing.T) {
	golden := []struct {
		query string
		ts    int64
		want  string
	}{
		{"aid=1967&device_id=dev_aaaaaaaa&iid=111", 1_700_000_000,
			"8oHyrVYEy1xDCDlx64ko40qC6JC9VYY/ZkjJzEl50iSFDw8Yfph1hupLxfKrA9GswSEz0Yi4goA7OQ4u+H8ECdOaQDL4GduQ3LULSq9Q4iSEGbS3L6G24GXChtnmPr6n46M2PLlKpXvF7uE0lhH4yD+X69FpdM41gQwx5Y5U3NdwXFaxlqYFszyp7dlUE6BEBWqwDsiAH8Jk9TGKr/jfHt4fVmMc9zRyNRdGeV0x6aP//x7aV+8Po8a/9KK3UNG5KEpmIgzRT6NUttkBP9gRNUh3"},
		{"aid=1967&device_id=dev_bbbbbbbbbbbbbbbb&iid=222", 1_700_000_001,
			"8oGWgilKQHD4eCdd0VB0vBSbTAkv/3xWE2YWJkxic35CgXkxrUlMTz05HJAak5b5Xx9yxohnoAAcEX//MI9t7XBBtnPwOTaJzkJgBLaYOOotfI7YuqhsEbE5BB363bhKz/Qw/+MI5gH6pgdAXfsRj68u7Qwu3gqCY1NYstqNZFFVrph7dWtXDBYM1aoVEyfkYD2haoaOr9D/gl1zdzAAzbW7we6lr9/zFApbuNsbr2dagdUrQc51QNN0HBOtIkpVBVunDwXM8qR271Ei+Pa4EgKy"},
		{"aid=1967&device_id=X&iid=333", 1_234_567_890,
			"8oEOCRTb0LQf+M+MsliYu4M9kBzAlzXvFgL/Ub198hyoBGxYfNXLAENmH+K/10JQ+Rbhm2zRxNtNv5wVd885xcb6C6F10zp/9jVeGHGz8plz8W3UXHHNrFKjJptjQElGDbyaVLSKt56+HfwO7ENYMfjVv/5PNi1upVciiHqyOPDDAtwi/fZkwBoU1kC7RolMOuedn4lCg/s0CjnNvxFT0QpF/K0yoW3Zpu30tkadIET7WbZ3+7KT3c1yAA6RKWI4mmw="},
		{"aid=1967&device_id=test_device_id&iid=456&version_name=6.5.1.32", 1_700_000_000,
			"8oHu8LnZm0OtJJIaSr1427D+yeH70h5aZTVjoSW7MfFi/hk1LhjTqsEEWYpwcHfK4/2SXuJxTtopRcPeHzVpeFpUD4B4yxEG7REejpCZ1OqeAaLsw/lMebwUjUHrUjnAZCKq0QyimnwdiTiYIsH3GfiK/xlPcFcARqwX7RkI1qnnWUgiJVDEacSm8yhBat6MvDVOa7+U2MgcLu7Q26pFzX8VX5F+ryP76ETs1nRGLmlhHkwyiKb6CETstvGQnJgocgZBNNvlHD2/P+4LWjFMvhNK"},
	}

	for _, g := range golden {
		f3 := recoverArgus551Field3(t, g.want)
		got := Argus(ArgusOptions{
			QueryString: g.query,
			Timestamp:   g.ts,
			AID:         1967,
			LicenseID:   1_611_921_764,
			DeviceID:    queryParam(g.query, "device_id"),
			RandField:   f3,
		})
		if got != g.want {
			t.Errorf("argus551 query=%s ts=%d\n got %s\nwant %s", g.query, g.ts, got, g.want)
		}
	}
}

// recoverArgus551Field3 把 x-argus 551 blob 反解回 bean 并取出字段 3（tag 0x18）。
func recoverArgus551Field3(t *testing.T, argusB64 string) int32 {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(argusB64)
	if err != nil {
		t.Fatalf("golden base64: %v", err)
	}
	signKey, _ := hex.DecodeString(argusSignKeyHex)
	aesKey, _ := hex.DecodeString(argusAESKeyHex)
	aesIv, _ := hex.DecodeString(argusAESIvHex)

	blk, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	pt := make([]byte, len(raw)-2)
	cipher.NewCBCDecrypter(blk, aesIv).CryptBlocks(pt, raw[2:])
	pad := int(pt[len(pt)-1])
	body := pt[:len(pt)-pad]
	body = body[:len(body)-2]

	rev := append([]byte(nil), body...)
	reverseBytes(rev)
	un := make([]byte, len(rev))
	copy(un[:8], rev[:8])
	for i := 8; i < len(rev); i++ {
		un[i] = rev[i] ^ rev[i%8]
	}
	encPb := un[8:]
	encPb = encPb[:len(encPb)/16*16]

	var kl [4]uint64
	for i := 0; i < 4; i++ {
		for j := 0; j < 8; j++ {
			kl[i] |= uint64(signKey[i*8+j]) << (j * 8)
		}
	}
	proto := make([]byte, 0, len(encPb))
	for i := 0; i+16 <= len(encPb); i += 16 {
		var a, b uint64
		for j := 0; j < 8; j++ {
			a |= uint64(encPb[i+j]) << (j * 8)
			b |= uint64(encPb[i+8+j]) << (j * 8)
		}
		p := SimonDec([2]uint64{a, b}, kl)
		for j := 0; j < 8; j++ {
			proto = append(proto, byte((p[0]>>(j*8))&255))
		}
		for j := 0; j < 8; j++ {
			proto = append(proto, byte((p[1]>>(j*8))&255))
		}
	}

	// 顺序扫描字段，找到 tag 0x18（字段 3，varint）。
	for i := 0; i < len(proto); {
		tag := int(proto[i])
		i++
		field := tag >> 3
		wire := tag & 7
		var v uint64
		shift := uint(0)
		for {
			if i >= len(proto) {
				t.Fatalf("argus bean 截断")
			}
			b := proto[i]
			i++
			v |= uint64(b&0x7f) << shift
			if b&0x80 == 0 {
				break
			}
			shift += 7
		}
		if wire == 0 && field == 3 {
			return int32(uint32(v))
		}
		if wire == 2 {
			if i >= len(proto) {
				t.Fatalf("argus bean 截断")
			}
			ln := int(v)
			i += ln
		}
	}
	t.Fatalf("argus bean 里没有字段 3")
	return 0
}

var _ = strconv.Itoa
