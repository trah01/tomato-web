package fqsign

// 只写不读的精简 protobuf 编码器，字段顺序即写入顺序。
//
// writeVarint 先把值掩到 32 位，这是上游的字节布局要求（x-argus 的
// field 3 nonce 为负数时依赖这条规则）。

type protoField struct {
	idx   int
	typ   uint8
	vUint uint64
	blob  int // typ==2 时指向 blobs 下标
}

// ProtoBuf 是待编码的字段序列。
type ProtoBuf struct {
	fields []protoField
	blobs  [][]byte
}

// NewProto 创建空 bean。
func NewProto() *ProtoBuf { return &ProtoBuf{} }

// PutVarint 写入 varint 字段（typ=0）。
func (p *ProtoBuf) PutVarint(idx int, v int64) {
	p.fields = append(p.fields, protoField{idx: idx, typ: 0, vUint: uint64(v), blob: -1})
}

// PutBytes 写入 length-delimited 字段（typ=2）。
func (p *ProtoBuf) PutBytes(idx int, data []byte) {
	p.blobs = append(p.blobs, append([]byte(nil), data...))
	p.fields = append(p.fields, protoField{idx: idx, typ: 2, blob: len(p.blobs) - 1})
}

// PutUTF8 写入字符串字段（typ=2）。
func (p *ProtoBuf) PutUTF8(idx int, s string) { p.PutBytes(idx, []byte(s)) }

// PutProto 写入嵌套 bean（typ=2）。
func (p *ProtoBuf) PutProto(idx int, nested *ProtoBuf) { p.PutBytes(idx, nested.ToBuf()) }

// ToBuf 编码成字节流。
func (p *ProtoBuf) ToBuf() []byte {
	out := make([]byte, 0, 128)
	for _, f := range p.fields {
		key := uint64(f.idx)<<3 | uint64(f.typ&7)
		writeVarint(&out, key)
		switch f.typ {
		case 0:
			writeVarint(&out, f.vUint)
		case 2:
			blob := p.blobs[f.blob]
			writeVarint(&out, uint64(len(blob)))
			out = append(out, blob...)
		}
	}
	return out
}

func writeVarint(dst *[]byte, v uint64) {
	v &= 0xFFFF_FFFF
	for v >= 128 {
		*dst = append(*dst, byte(v&127)|128)
		v >>= 7
	}
	*dst = append(*dst, byte(v&127))
}
