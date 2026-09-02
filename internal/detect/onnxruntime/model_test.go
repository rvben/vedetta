package onnxruntime

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

// ======================================================================
// TensorProto data-field encoders (test-only)
// ======================================================================

// encodePackedInt32 encodes values the way protobuf packs repeated int32:
// each value is a varint of its two's-complement 64-bit form, so negatives
// occupy ten bytes.
func encodePackedInt32(vals []int32) []byte {
	var buf []byte
	for _, v := range vals {
		buf = append(buf, protoVarint(uint64(int64(v)))...)
	}
	return buf
}

func encodePackedFloat64(vals []float64) []byte {
	buf := make([]byte, len(vals)*8)
	for i, v := range vals {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(v))
	}
	return buf
}

func encodePackedUint64(vals []uint64) []byte {
	var buf []byte
	for _, v := range vals {
		buf = append(buf, protoVarint(v)...)
	}
	return buf
}

// buildTensorProtoFields builds a serialized TensorProto with dims, data_type,
// name and one caller-supplied data field.
func buildTensorProtoFields(name string, dims []int64, dataType int, data []byte) []byte {
	var tp []byte
	if len(dims) > 0 {
		tp = append(tp, protoBytes(1, encodePackedInt64(dims))...)
	}
	tp = append(tp, protoVarintField(2, uint64(dataType))...)
	tp = append(tp, data...)
	if name != "" {
		tp = append(tp, protoString(8, name)...)
	}
	return tp
}

// ======================================================================
// int32_data (field 5)
// ======================================================================

func TestTensorProtoInt32DataPacked(t *testing.T) {
	vals := []int32{1, -2, 3, -4, 5, 6}
	raw := buildTensorProtoFields("W", []int64{2, 3}, onnxInt32, protoBytes(5, encodePackedInt32(vals)))

	tp, err := parseTensorProto(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(tp.Int32Data) != len(vals) {
		t.Fatalf("Int32Data length = %d, want %d", len(tp.Int32Data), len(vals))
	}

	data, err := tp.ToFloat32()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(vals) {
		t.Fatalf("ToFloat32 length = %d, want %d", len(data), len(vals))
	}
	for i, want := range vals {
		if data[i] != float32(want) {
			t.Errorf("int32_data[%d] = %f, want %d", i, data[i], want)
		}
	}
}

func TestTensorProtoInt32DataUnpacked(t *testing.T) {
	vals := []int32{7, -8, 9}
	var fields []byte
	for _, v := range vals {
		fields = append(fields, protoVarintField(5, uint64(int64(v)))...)
	}
	raw := buildTensorProtoFields("W", []int64{3}, onnxInt32, fields)

	tp, err := parseTensorProto(raw)
	if err != nil {
		t.Fatal(err)
	}
	data, err := tp.ToFloat32()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(vals) {
		t.Fatalf("ToFloat32 length = %d, want %d", len(data), len(vals))
	}
	for i, want := range vals {
		if data[i] != float32(want) {
			t.Errorf("int32_data[%d] = %f, want %d", i, data[i], want)
		}
	}
}

// int32_data also carries the narrower integer types. A bool tensor stores 0/1
// there and must decode to 0/1 floats.
func TestTensorProtoInt32DataBool(t *testing.T) {
	raw := buildTensorProtoFields("mask", []int64{4}, onnxBool, protoBytes(5, encodePackedInt32([]int32{1, 0, 1, 1})))

	tp, err := parseTensorProto(raw)
	if err != nil {
		t.Fatal(err)
	}
	data, err := tp.ToFloat32()
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{1, 0, 1, 1}
	if len(data) != len(want) {
		t.Fatalf("ToFloat32 length = %d, want %d", len(data), len(want))
	}
	for i, w := range want {
		if data[i] != w {
			t.Errorf("bool[%d] = %f, want %f", i, data[i], w)
		}
	}
}

// ======================================================================
// double_data (field 10) and uint64_data (field 11)
// ======================================================================

func TestTensorProtoDoubleDataPacked(t *testing.T) {
	vals := []float64{1.5, -2.25, 3.75}
	raw := buildTensorProtoFields("D", []int64{3}, onnxDouble, protoBytes(10, encodePackedFloat64(vals)))

	tp, err := parseTensorProto(raw)
	if err != nil {
		t.Fatal(err)
	}
	data, err := tp.ToFloat32()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(vals) {
		t.Fatalf("ToFloat32 length = %d, want %d", len(data), len(vals))
	}
	for i, want := range vals {
		if data[i] != float32(want) {
			t.Errorf("double_data[%d] = %f, want %f", i, data[i], want)
		}
	}
}

func TestTensorProtoUint64DataPacked(t *testing.T) {
	vals := []uint64{10, 20, 30}
	raw := buildTensorProtoFields("U", []int64{3}, onnxUint64, protoBytes(11, encodePackedUint64(vals)))

	tp, err := parseTensorProto(raw)
	if err != nil {
		t.Fatal(err)
	}
	data, err := tp.ToFloat32()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != len(vals) {
		t.Fatalf("ToFloat32 length = %d, want %d", len(data), len(vals))
	}
	for i, want := range vals {
		if data[i] != float32(want) {
			t.Errorf("uint64_data[%d] = %f, want %d", i, data[i], want)
		}
	}
}

// Half precision is not decodable by this engine. Saying so beats returning a
// bit pattern reinterpreted as an integer, which looks like valid weights.
func TestTensorProtoFloat16Rejected(t *testing.T) {
	raw := buildTensorProtoFields("H", []int64{2}, onnxFloat16, protoBytes(5, encodePackedInt32([]int32{15360, 16384})))

	tp, err := parseTensorProto(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tp.ToFloat32(); err == nil {
		t.Fatal("expected an error for a float16 tensor")
	}
}

// ======================================================================
// Initializer element-count validation
// ======================================================================

func TestNewSessionInitializerLengthMismatch(t *testing.T) {
	// dims claim six elements, float_data supplies two.
	badInit := buildTensorProto("W", []int64{2, 3}, []float32{1, 2})
	nodeData := buildNodeProto("add0", "Add", []string{"X", "W"}, []string{"Y"}, nil)
	graphData := buildGraphProto([][]byte{nodeData}, [][]byte{badInit}, []string{"X", "W"}, []string{"Y"})
	modelData := buildModelProto(9, 20, graphData)

	_, err := NewSession(modelData)
	if err == nil {
		t.Fatal("expected NewSession to reject an initializer whose data does not fill its shape")
	}
	msg := err.Error()
	for _, want := range []string{`"W"`, "6", "2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

// An initializer that carries no data field at all is the shape M14 took in
// production: the tensor loads empty and the mismatch only surfaces mid-frame.
func TestNewSessionInitializerNoData(t *testing.T) {
	emptyInit := buildTensorProtoFields("W", []int64{2, 3}, onnxFloat, nil)
	nodeData := buildNodeProto("add0", "Add", []string{"X", "W"}, []string{"Y"}, nil)
	graphData := buildGraphProto([][]byte{nodeData}, [][]byte{emptyInit}, []string{"X", "W"}, []string{"Y"})
	modelData := buildModelProto(9, 20, graphData)

	if _, err := NewSession(modelData); err == nil {
		t.Fatal("expected NewSession to reject an initializer with no data")
	}
}

// The whole point of parsing int32_data: a model that encodes a constant that
// way must load and compute, not load empty.
func TestNewSessionInitializerInt32DataRuns(t *testing.T) {
	weight := buildTensorProtoFields("W", []int64{3}, onnxInt32, protoBytes(5, encodePackedInt32([]int32{10, 20, 30})))
	nodeData := buildNodeProto("add0", "Add", []string{"X", "W"}, []string{"Y"}, nil)
	graphData := buildGraphProto([][]byte{nodeData}, [][]byte{weight}, []string{"X", "W"}, []string{"Y"})
	modelData := buildModelProto(9, 20, graphData)

	session, err := NewSession(modelData)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	outputs, err := session.Run(map[string]*Tensor{"X": NewTensor([]int64{3}, []float32{1, 2, 3})})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertTensorApprox(t, outputs["Y"], []int64{3}, []float32{11, 22, 33}, eps)
}

// A Constant node whose tensor attribute does not fill its shape is the same
// defect one level down, and must not be swallowed.
func TestNewSessionConstantAttrLengthMismatch(t *testing.T) {
	badTensor := buildTensorProto("", []int64{4}, []float32{1, 2})
	var attr []byte
	attr = append(attr, protoString(1, "value")...)
	attr = append(attr, protoBytes(5, badTensor)...)
	attr = append(attr, protoVarintField(20, uint64(attrTensor))...)

	nodeData := buildNodeProto("const0", "Constant", nil, []string{"Y"}, attr)
	graphData := buildGraphProto([][]byte{nodeData}, nil, nil, []string{"Y"})
	modelData := buildModelProto(9, 20, graphData)

	_, err := NewSession(modelData)
	if err == nil {
		t.Fatal("expected NewSession to reject a Constant whose data does not fill its shape")
	}
	if !strings.Contains(err.Error(), "const0") {
		t.Errorf("error %q does not name the node", err)
	}
}
