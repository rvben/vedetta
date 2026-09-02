package onnxruntime

import (
	"encoding/binary"
	"fmt"
	"math"
)

// ONNX data types (from onnx.proto TensorProto.DataType)
const (
	onnxFloat    = 1
	onnxUint8    = 2
	onnxInt8     = 3
	onnxUint16   = 4
	onnxInt16    = 5
	onnxInt32    = 6
	onnxInt64    = 7
	onnxBool     = 9
	onnxFloat16  = 10
	onnxDouble   = 11
	onnxUint32   = 12
	onnxUint64   = 13
	onnxBFloat16 = 16
)

// ONNX attribute types (from onnx.proto AttributeProto.AttributeType)
const (
	attrFloat   = 1
	attrInt     = 2
	attrString  = 3
	attrTensor  = 4
	attrFloats  = 6
	attrInts    = 7
	attrStrings = 8
)

// ModelProto is the top-level ONNX model container.
type ModelProto struct {
	IRVersion    int64
	OpsetVersion int64
	Graph        *GraphProto
}

// GraphProto contains the computation graph.
type GraphProto struct {
	Nodes        []*NodeProto
	Initializers []*TensorProto
	Inputs       []*ValueInfoProto
	Outputs      []*ValueInfoProto
}

// NodeProto represents a single operation in the graph.
type NodeProto struct {
	Inputs  []string
	Outputs []string
	Name    string
	OpType  string
	Attrs   []*AttributeProto
}

// AttributeProto holds an operator attribute.
type AttributeProto struct {
	Name   string
	Type   int32
	F      float32
	I      int64
	S      []byte
	T      *TensorProto
	Floats []float32
	Ints   []int64
}

// TensorProto holds a tensor (weights/constants). Exactly one data field is
// populated: raw_data, or the typed field that onnx.proto assigns to the data
// type. int32_data carries every integer type narrower than 64 bits, and
// uint64_data carries uint32 as well as uint64.
type TensorProto struct {
	Name       string
	Dims       []int64
	DataType   int32
	RawData    []byte
	FloatData  []float32
	Int32Data  []int32
	Int64Data  []int64
	DoubleData []float64
	Uint64Data []uint64
}

// ElementCount returns the number of elements the shape calls for. A tensor
// with no dims is a scalar and holds one element.
func (tp *TensorProto) ElementCount() int64 {
	n := int64(1)
	for _, d := range tp.Dims {
		n *= d
	}
	return n
}

// ValueInfoProto describes a graph input/output.
type ValueInfoProto struct {
	Name  string
	Shape []int64
}

// ParseModel parses an ONNX model from raw protobuf bytes.
func ParseModel(data []byte) (*ModelProto, error) {
	model := &ModelProto{}
	r := newProtoReader(data)

	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, fmt.Errorf("model: %w", err)
		}

		switch {
		case fieldNum == 1 && wireType == wireVarint: // ir_version
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			model.IRVersion = int64(v)

		case fieldNum == 7 && wireType == wireBytes: // graph
			graphData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			graph, err := parseGraphProto(graphData)
			if err != nil {
				return nil, fmt.Errorf("model.graph: %w", err)
			}
			model.Graph = graph

		case fieldNum == 8 && wireType == wireBytes: // opset_import
			opsetData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			// Parse opset version from OperatorSetIdProto
			opR := newProtoReader(opsetData)
			for opR.remaining() > 0 {
				fn, wt, err := opR.readTag()
				if err != nil {
					break
				}
				if fn == 2 && wt == wireVarint {
					v, _ := opR.readVarint()
					model.OpsetVersion = int64(v)
				} else {
					_ = opR.skip(wt)
				}
			}

		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}

	if model.Graph == nil {
		return nil, fmt.Errorf("model: no graph found")
	}
	return model, nil
}

func parseGraphProto(data []byte) (*GraphProto, error) {
	graph := &GraphProto{}
	r := newProtoReader(data)

	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		switch {
		case fieldNum == 1 && wireType == wireBytes: // node
			nodeData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			node, err := parseNodeProto(nodeData)
			if err != nil {
				return nil, fmt.Errorf("graph.node: %w", err)
			}
			graph.Nodes = append(graph.Nodes, node)

		case fieldNum == 5 && wireType == wireBytes: // initializer
			tensorData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			tensor, err := parseTensorProto(tensorData)
			if err != nil {
				return nil, fmt.Errorf("graph.initializer: %w", err)
			}
			graph.Initializers = append(graph.Initializers, tensor)

		case fieldNum == 11 && wireType == wireBytes: // input
			viData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			vi, err := parseValueInfoProto(viData)
			if err != nil {
				return nil, err
			}
			graph.Inputs = append(graph.Inputs, vi)

		case fieldNum == 12 && wireType == wireBytes: // output
			viData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			vi, err := parseValueInfoProto(viData)
			if err != nil {
				return nil, err
			}
			graph.Outputs = append(graph.Outputs, vi)

		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return graph, nil
}

func parseNodeProto(data []byte) (*NodeProto, error) {
	node := &NodeProto{}
	r := newProtoReader(data)

	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		switch {
		case fieldNum == 1 && wireType == wireBytes: // input
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			node.Inputs = append(node.Inputs, string(b))

		case fieldNum == 2 && wireType == wireBytes: // output
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			node.Outputs = append(node.Outputs, string(b))

		case fieldNum == 3 && wireType == wireBytes: // name
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			node.Name = string(b)

		case fieldNum == 4 && wireType == wireBytes: // op_type
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			node.OpType = string(b)

		case fieldNum == 5 && wireType == wireBytes: // attribute
			attrData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			attr, err := parseAttributeProto(attrData)
			if err != nil {
				return nil, err
			}
			node.Attrs = append(node.Attrs, attr)

		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return node, nil
}

func parseAttributeProto(data []byte) (*AttributeProto, error) {
	attr := &AttributeProto{}
	r := newProtoReader(data)

	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		switch {
		case fieldNum == 1 && wireType == wireBytes: // name
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			attr.Name = string(b)

		case fieldNum == 2 && wireType == wire32Bit: // f (float)
			v, err := r.readFixed32()
			if err != nil {
				return nil, err
			}
			attr.F = math.Float32frombits(v)

		case fieldNum == 3 && wireType == wireVarint: // i (int64)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			attr.I = int64(v)

		case fieldNum == 4 && wireType == wireBytes: // s (bytes/string)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			attr.S = b

		case fieldNum == 5 && wireType == wireBytes: // t (tensor)
			tensorData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			tensor, err := parseTensorProto(tensorData)
			if err != nil {
				return nil, err
			}
			attr.T = tensor

		case fieldNum == 7 && wireType == wireBytes: // floats (packed)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			attr.Floats = readPackedFloat32(b)

		case fieldNum == 7 && wireType == wire32Bit: // floats (unpacked)
			v, err := r.readFixed32()
			if err != nil {
				return nil, err
			}
			attr.Floats = append(attr.Floats, math.Float32frombits(v))

		case fieldNum == 8 && wireType == wireBytes: // ints (packed)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			ints, err := readPackedInt64(b)
			if err != nil {
				return nil, err
			}
			attr.Ints = ints

		case fieldNum == 8 && wireType == wireVarint: // ints (unpacked)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			attr.Ints = append(attr.Ints, int64(v))

		case fieldNum == 20 && wireType == wireVarint: // type
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			attr.Type = int32(v)

		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return attr, nil
}

func parseTensorProto(data []byte) (*TensorProto, error) {
	tp := &TensorProto{}
	r := newProtoReader(data)

	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		switch {
		case fieldNum == 1 && wireType == wireBytes: // dims (packed)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			dims, err := readPackedInt64(b)
			if err != nil {
				return nil, err
			}
			tp.Dims = append(tp.Dims, dims...)

		case fieldNum == 1 && wireType == wireVarint: // dims (unpacked)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			tp.Dims = append(tp.Dims, int64(v))

		case fieldNum == 2 && wireType == wireVarint: // data_type
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			tp.DataType = int32(v)

		case fieldNum == 4 && wireType == wireBytes: // float_data (packed)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			tp.FloatData = append(tp.FloatData, readPackedFloat32(b)...)

		case fieldNum == 4 && wireType == wire32Bit: // float_data (unpacked)
			v, err := r.readFixed32()
			if err != nil {
				return nil, err
			}
			tp.FloatData = append(tp.FloatData, math.Float32frombits(v))

		case fieldNum == 5 && wireType == wireBytes: // int32_data (packed)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			ints, err := readPackedInt32(b)
			if err != nil {
				return nil, err
			}
			tp.Int32Data = append(tp.Int32Data, ints...)

		case fieldNum == 5 && wireType == wireVarint: // int32_data (unpacked)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			tp.Int32Data = append(tp.Int32Data, int32(v))

		case fieldNum == 8 && wireType == wireBytes: // name
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			tp.Name = string(b)

		case fieldNum == 9 && wireType == wireBytes: // raw_data
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			tp.RawData = b

		case fieldNum == 10 && wireType == wireBytes: // double_data (packed)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			tp.DoubleData = append(tp.DoubleData, readPackedFloat64(b)...)

		case fieldNum == 10 && wireType == wire64Bit: // double_data (unpacked)
			v, err := r.readFixed64()
			if err != nil {
				return nil, err
			}
			tp.DoubleData = append(tp.DoubleData, math.Float64frombits(v))

		case fieldNum == 11 && wireType == wireBytes: // uint64_data (packed)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			uints, err := readPackedUint64(b)
			if err != nil {
				return nil, err
			}
			tp.Uint64Data = append(tp.Uint64Data, uints...)

		case fieldNum == 11 && wireType == wireVarint: // uint64_data (unpacked)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			tp.Uint64Data = append(tp.Uint64Data, v)

		case fieldNum == 7 && wireType == wireBytes: // int64_data (packed)
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			ints, err := readPackedInt64(b)
			if err != nil {
				return nil, err
			}
			tp.Int64Data = append(tp.Int64Data, ints...)

		case fieldNum == 7 && wireType == wireVarint: // int64_data (unpacked)
			v, err := r.readVarint()
			if err != nil {
				return nil, err
			}
			tp.Int64Data = append(tp.Int64Data, int64(v))

		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return tp, nil
}

func parseValueInfoProto(data []byte) (*ValueInfoProto, error) {
	vi := &ValueInfoProto{}
	r := newProtoReader(data)

	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}

		switch {
		case fieldNum == 1 && wireType == wireBytes: // name
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			vi.Name = string(b)

		case fieldNum == 2 && wireType == wireBytes: // type (TypeProto)
			typeData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			shape, err := parseTypeProtoShape(typeData)
			if err == nil {
				vi.Shape = shape
			}

		default:
			if err := r.skip(wireType); err != nil {
				return nil, err
			}
		}
	}
	return vi, nil
}

// parseTypeProtoShape extracts the shape from a TypeProto message.
// TypeProto → tensor_type (field 1) → TensorTypeProto → shape (field 2) → TensorShapeProto → dim[]
func parseTypeProtoShape(data []byte) ([]int64, error) {
	r := newProtoReader(data)
	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}
		if fieldNum == 1 && wireType == wireBytes { // tensor_type
			ttData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			return parseTensorTypeShape(ttData)
		}
		_ = r.skip(wireType)
	}
	return nil, nil
}

func parseTensorTypeShape(data []byte) ([]int64, error) {
	r := newProtoReader(data)
	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}
		if fieldNum == 2 && wireType == wireBytes { // shape
			shapeData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			return parseTensorShapeProto(shapeData)
		}
		_ = r.skip(wireType)
	}
	return nil, nil
}

func parseTensorShapeProto(data []byte) ([]int64, error) {
	var dims []int64
	r := newProtoReader(data)
	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return nil, err
		}
		if fieldNum == 1 && wireType == wireBytes { // dim (repeated Dimension)
			dimData, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			dim, err := parseDimensionProto(dimData)
			if err != nil {
				return nil, err
			}
			dims = append(dims, dim)
		} else {
			_ = r.skip(wireType)
		}
	}
	return dims, nil
}

func parseDimensionProto(data []byte) (int64, error) {
	r := newProtoReader(data)
	for r.remaining() > 0 {
		fieldNum, wireType, err := r.readTag()
		if err != nil {
			return 0, err
		}
		if fieldNum == 1 && wireType == wireVarint { // dim_value
			v, err := r.readVarint()
			if err != nil {
				return 0, err
			}
			return int64(v), nil
		}
		_ = r.skip(wireType)
	}
	return 0, nil
}

// ToFloat32 converts a TensorProto to a float32 slice. Values arrive either as
// raw_data, a little-endian byte image of the tensor, or in the repeated field
// that onnx.proto assigns to the data type, so both forms are handled.
//
// A tensor carrying no data at all yields an empty slice; callers compare the
// result against the shape to decide whether that is legal.
func (tp *TensorProto) ToFloat32() ([]float32, error) {
	// Half precision has no decoder here. Reading its bit pattern as an integer
	// would produce plausible-looking weights that are silently wrong.
	if tp.DataType == onnxFloat16 || tp.DataType == onnxBFloat16 {
		return nil, fmt.Errorf("unsupported tensor data type: %d (half precision)", tp.DataType)
	}

	if len(tp.RawData) > 0 {
		return tp.rawDataToFloat32()
	}
	return tp.typedDataToFloat32()
}

// rawDataToFloat32 decodes the raw_data byte image according to the data type.
func (tp *TensorProto) rawDataToFloat32() ([]float32, error) {
	raw := tp.RawData

	switch tp.DataType {
	case onnxFloat:
		return readPackedFloat32(raw), nil

	case onnxDouble:
		n := len(raw) / 8
		result := make([]float32, n)
		for i := range n {
			bits := binary.LittleEndian.Uint64(raw[i*8:])
			result[i] = float32(math.Float64frombits(bits))
		}
		return result, nil

	case onnxInt8:
		result := make([]float32, len(raw))
		for i, b := range raw {
			result[i] = float32(int8(b))
		}
		return result, nil

	case onnxUint8, onnxBool:
		result := make([]float32, len(raw))
		for i, b := range raw {
			result[i] = float32(b)
		}
		return result, nil

	case onnxInt16:
		n := len(raw) / 2
		result := make([]float32, n)
		for i := range n {
			result[i] = float32(int16(binary.LittleEndian.Uint16(raw[i*2:])))
		}
		return result, nil

	case onnxUint16:
		n := len(raw) / 2
		result := make([]float32, n)
		for i := range n {
			result[i] = float32(binary.LittleEndian.Uint16(raw[i*2:]))
		}
		return result, nil

	case onnxInt32:
		n := len(raw) / 4
		result := make([]float32, n)
		for i := range n {
			result[i] = float32(int32(binary.LittleEndian.Uint32(raw[i*4:])))
		}
		return result, nil

	case onnxUint32:
		n := len(raw) / 4
		result := make([]float32, n)
		for i := range n {
			result[i] = float32(binary.LittleEndian.Uint32(raw[i*4:]))
		}
		return result, nil

	case onnxInt64:
		n := len(raw) / 8
		result := make([]float32, n)
		for i := range n {
			result[i] = float32(int64(binary.LittleEndian.Uint64(raw[i*8:])))
		}
		return result, nil

	case onnxUint64:
		n := len(raw) / 8
		result := make([]float32, n)
		for i := range n {
			result[i] = float32(binary.LittleEndian.Uint64(raw[i*8:]))
		}
		return result, nil

	default:
		return nil, fmt.Errorf("unsupported tensor data type: %d", tp.DataType)
	}
}

// typedDataToFloat32 reads whichever repeated data field is populated.
// int32_data carries bool, int8, uint8, int16, uint16 and int32 alike, and
// uint64_data carries uint32 as well as uint64, so the populated field rather
// than the data type decides how the values are read.
func (tp *TensorProto) typedDataToFloat32() ([]float32, error) {
	switch {
	case len(tp.FloatData) > 0:
		return tp.FloatData, nil

	case len(tp.Int32Data) > 0:
		result := make([]float32, len(tp.Int32Data))
		for i, v := range tp.Int32Data {
			result[i] = float32(v)
		}
		return result, nil

	case len(tp.Int64Data) > 0:
		result := make([]float32, len(tp.Int64Data))
		for i, v := range tp.Int64Data {
			result[i] = float32(v)
		}
		return result, nil

	case len(tp.DoubleData) > 0:
		result := make([]float32, len(tp.DoubleData))
		for i, v := range tp.DoubleData {
			result[i] = float32(v)
		}
		return result, nil

	case len(tp.Uint64Data) > 0:
		result := make([]float32, len(tp.Uint64Data))
		for i, v := range tp.Uint64Data {
			result[i] = float32(v)
		}
		return result, nil

	default:
		return []float32{}, nil
	}
}
