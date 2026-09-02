package onnxruntime

// Minimal protobuf wire format decoder for ONNX model files.
// Only supports the subset needed to parse ONNX ModelProto.

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Wire types
const (
	wireVarint = 0
	wire64Bit  = 1
	wireBytes  = 2
	wire32Bit  = 5
)

// protoReader provides sequential reading of protobuf wire format.
type protoReader struct {
	data []byte
	pos  int
}

func newProtoReader(data []byte) *protoReader {
	return &protoReader{data: data}
}

func (r *protoReader) remaining() int {
	return len(r.data) - r.pos
}

func (r *protoReader) readVarint() (uint64, error) {
	var val uint64
	var shift uint
	for {
		if r.pos >= len(r.data) {
			return 0, fmt.Errorf("proto: unexpected EOF reading varint")
		}
		b := r.data[r.pos]
		r.pos++
		val |= uint64(b&0x7F) << shift
		if b < 0x80 {
			return val, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, fmt.Errorf("proto: varint overflow")
		}
	}
}

func (r *protoReader) readTag() (fieldNum int, wireType int, err error) {
	v, err := r.readVarint()
	if err != nil {
		return 0, 0, err
	}
	return int(v >> 3), int(v & 0x7), nil
}

func (r *protoReader) readBytes() ([]byte, error) {
	length, err := r.readVarint()
	if err != nil {
		return nil, err
	}
	if r.pos+int(length) > len(r.data) {
		return nil, fmt.Errorf("proto: bytes length %d exceeds remaining %d", length, r.remaining())
	}
	data := r.data[r.pos : r.pos+int(length)]
	r.pos += int(length)
	return data, nil
}

func (r *protoReader) readFixed32() (uint32, error) {
	if r.pos+4 > len(r.data) {
		return 0, fmt.Errorf("proto: unexpected EOF reading fixed32")
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *protoReader) readFixed64() (uint64, error) {
	if r.pos+8 > len(r.data) {
		return 0, fmt.Errorf("proto: unexpected EOF reading fixed64")
	}
	v := binary.LittleEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	return v, nil
}

func (r *protoReader) skip(wireType int) error {
	switch wireType {
	case wireVarint:
		_, err := r.readVarint()
		return err
	case wire64Bit:
		if r.pos+8 > len(r.data) {
			return fmt.Errorf("proto: unexpected EOF skipping 64-bit")
		}
		r.pos += 8
	case wireBytes:
		_, err := r.readBytes()
		return err
	case wire32Bit:
		if r.pos+4 > len(r.data) {
			return fmt.Errorf("proto: unexpected EOF skipping 32-bit")
		}
		r.pos += 4
	default:
		return fmt.Errorf("proto: unknown wire type %d", wireType)
	}
	return nil
}

// readPackedFloat32 reads a packed repeated float field.
func readPackedFloat32(data []byte) []float32 {
	n := len(data) / 4
	result := make([]float32, n)
	for i := range n {
		bits := binary.LittleEndian.Uint32(data[i*4:])
		result[i] = math.Float32frombits(bits)
	}
	return result
}

// readPackedFloat64 reads a packed repeated double field.
func readPackedFloat64(data []byte) []float64 {
	n := len(data) / 8
	result := make([]float64, n)
	for i := range n {
		bits := binary.LittleEndian.Uint64(data[i*8:])
		result[i] = math.Float64frombits(bits)
	}
	return result
}

// readPackedInt64 reads a packed repeated int64 (varint-encoded) field.
func readPackedInt64(data []byte) ([]int64, error) {
	r := newProtoReader(data)
	var result []int64
	for r.remaining() > 0 {
		v, err := r.readVarint()
		if err != nil {
			return nil, err
		}
		result = append(result, int64(v))
	}
	return result, nil
}

// readPackedInt32 reads a packed repeated int32 (varint-encoded) field.
// Negative values arrive sign-extended to 64 bits, so the low 32 bits carry
// the value.
func readPackedInt32(data []byte) ([]int32, error) {
	r := newProtoReader(data)
	var result []int32
	for r.remaining() > 0 {
		v, err := r.readVarint()
		if err != nil {
			return nil, err
		}
		result = append(result, int32(v))
	}
	return result, nil
}

// readPackedUint64 reads a packed repeated uint64 (varint-encoded) field.
func readPackedUint64(data []byte) ([]uint64, error) {
	r := newProtoReader(data)
	var result []uint64
	for r.remaining() > 0 {
		v, err := r.readVarint()
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}
