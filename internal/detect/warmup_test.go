package detect

import (
	"encoding/binary"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rvben/vedetta/internal/config"
)

// ======================================================================
// Protobuf encoders (test-only) for building synthetic ONNX models
// ======================================================================

func pbVarint(v uint64) []byte {
	var buf [10]byte
	n := 0
	for v >= 0x80 {
		buf[n] = byte(v) | 0x80
		v >>= 7
		n++
	}
	buf[n] = byte(v)
	return buf[:n+1]
}

func pbTag(fieldNum, wireType int) []byte {
	return pbVarint(uint64(fieldNum<<3 | wireType))
}

func pbBytes(fieldNum int, data []byte) []byte {
	out := pbTag(fieldNum, 2)
	out = append(out, pbVarint(uint64(len(data)))...)
	return append(out, data...)
}

func pbString(fieldNum int, s string) []byte {
	return pbBytes(fieldNum, []byte(s))
}

func pbVarintField(fieldNum int, v uint64) []byte {
	return append(pbTag(fieldNum, 0), pbVarint(v)...)
}

func pbPackedInt64(vals []int64) []byte {
	var buf []byte
	for _, v := range vals {
		buf = append(buf, pbVarint(uint64(v))...)
	}
	return buf
}

// pbFloatTensor builds a TensorProto holding float data.
func pbFloatTensor(name string, dims []int64, data []float32) []byte {
	var tp []byte
	if len(dims) > 0 {
		tp = append(tp, pbBytes(1, pbPackedInt64(dims))...)
	}
	tp = append(tp, pbVarintField(2, 1)...) // data_type = FLOAT
	if len(data) > 0 {
		packed := make([]byte, len(data)*4)
		for i, v := range data {
			binary.LittleEndian.PutUint32(packed[i*4:], math.Float32bits(v))
		}
		tp = append(tp, pbBytes(4, packed)...)
	}
	tp = append(tp, pbString(8, name)...)
	return tp
}

// pbNode builds a NodeProto.
func pbNode(name, opType string, inputs, outputs []string, attrs []byte) []byte {
	var node []byte
	for _, inp := range inputs {
		node = append(node, pbString(1, inp)...)
	}
	for _, out := range outputs {
		node = append(node, pbString(2, out)...)
	}
	node = append(node, pbString(3, name)...)
	node = append(node, pbString(4, opType)...)
	if len(attrs) > 0 {
		node = append(node, pbBytes(5, attrs)...)
	}
	return node
}

// pbAttrInt builds an AttributeProto holding an int.
func pbAttrInt(name string, val int64) []byte {
	var attr []byte
	attr = append(attr, pbString(1, name)...)
	attr = append(attr, pbVarintField(3, uint64(val))...)
	attr = append(attr, pbVarintField(20, 2)...) // type = INT
	return attr
}

// pbModel builds a ModelProto around one node.
func pbModel(node []byte, initializers [][]byte, inputs, outputs []string) []byte {
	var graph []byte
	graph = append(graph, pbBytes(1, node)...)
	for _, init := range initializers {
		graph = append(graph, pbBytes(5, init)...)
	}
	for _, inp := range inputs {
		graph = append(graph, pbBytes(11, pbString(1, inp))...)
	}
	for _, out := range outputs {
		graph = append(graph, pbBytes(12, pbString(1, out))...)
	}

	var model []byte
	model = append(model, pbVarintField(1, 9)...)
	model = append(model, pbBytes(7, graph)...)
	model = append(model, pbBytes(8, pbVarintField(2, 20))...)
	return model
}

// gatherPanicModel builds a model that loads cleanly and then panics during
// inference: Gather along axis 5 of a 4-dimensional input slices the shape out
// of range. This is the shape of a model whose defect only appears once frames
// start arriving.
func gatherPanicModel() []byte {
	idx := pbFloatTensor("idx", []int64{1}, []float32{0})
	node := pbNode("gather0", "Gather", []string{"X", "idx"}, []string{"Y"}, pbAttrInt("axis", 5))
	return pbModel(node, [][]byte{idx}, []string{"X"}, []string{"Y"})
}

// reluModel builds a model that runs but returns the input size, which is not
// the YOLOv8 output layout the detector decodes.
func reluModel() []byte {
	node := pbNode("relu0", "Relu", []string{"X"}, []string{"Y"}, nil)
	return pbModel(node, nil, []string{"X"}, []string{"Y"})
}

// ======================================================================
// Warm-up
// ======================================================================

// A model that panics on its first inference must fail while the backend is
// being built. Without the warm-up it loads clean and every frame turns into a
// recovered panic with no detections.
func TestNewGoBackend_RejectsModelThatPanicsAtInference(t *testing.T) {
	_, err := NewGoBackend(gatherPanicModel())
	if err == nil {
		t.Fatal("expected NewGoBackend to reject a model that panics during inference")
	}
	if !strings.Contains(err.Error(), "warm-up") {
		t.Errorf("error %q does not say the warm-up failed", err)
	}
}

// A model with the wrong output layout is not the detector's model, and saying
// so at load time beats returning nothing on every frame.
func TestNewGoBackend_RejectsWrongOutputSize(t *testing.T) {
	_, err := NewGoBackend(reluModel())
	if err == nil {
		t.Fatal("expected NewGoBackend to reject a model whose output is not the YOLOv8 layout")
	}
	if !strings.Contains(err.Error(), "warm-up") {
		t.Errorf("error %q does not say the warm-up failed", err)
	}
}

// The bundled model must survive its own warm-up.
func TestNewGoBackend_AcceptsBundledModel(t *testing.T) {
	b, err := NewGoBackend(loadTestModel(t))
	if err != nil {
		t.Fatalf("NewGoBackend on the bundled model: %v", err)
	}
	b.Close()
}

// A detector handed a model that cannot run reports itself unavailable, so the
// camera falls back to motion-only instead of calling a backend that fails on
// every frame.
func TestDetectorNew_UnusableModelIsNotAvailable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.onnx")
	if err := os.WriteFile(path, gatherPanicModel(), 0o600); err != nil {
		t.Fatal(err)
	}

	// New falls back to auto-download when a model cannot be used, which must
	// not reach the network from a test.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	defer slog.SetDefault(prev)

	d := New(config.DetectConfig{
		ModelPath:      path,
		Backend:        "go",
		ScoreThreshold: 0.5,
	})
	defer d.Close()

	if d.Available() {
		t.Fatal("detector reports a model that cannot run as available")
	}
}
