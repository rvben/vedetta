package onnxruntime

import (
	"fmt"
	"log/slog"
)

// Session holds a loaded ONNX model ready for inference.
type Session struct {
	model       *ModelProto
	inputNames  []string
	outputNames []string
	// Execution order: optimized nodes (fused ops applied).
	execOrder []*NodeProto
	// cachedAttrs holds pre-parsed attributes per node.
	cachedAttrs []*Attributes
	// initializers holds pre-loaded weight tensors by name.
	initializers map[string]*Tensor

	// Indexed tensor storage for Run() — avoids map[string]*Tensor overhead.
	// tensorIDs maps tensor names to integer indices in the values slice.
	tensorIDs   map[string]int
	numTensors  int
	initIDs     []int // indices of initializers in values slice
	initTensors []*Tensor
	inputIDs    []int // indices of user inputs (ordered by inputNames)
	outputIDs   []int // indices of outputs (ordered by outputNames)
	// Per-node: precomputed input/output indices into values slice.
	nodeInputIDs  [][]int
	nodeOutputIDs [][]int
	// Tracks which tensor IDs are safe to recycle (pooled, not output/init/input).
	recyclable []bool
	// Pre-resolved op functions — avoids map lookup during inference.
	nodeFuncs []OpFunc
	// Pre-allocated buffers reused across Run() calls.
	values   []*Tensor
	inputBuf []*Tensor
	// Previous run's output tensors — returned to free list at start of next run.
	prevOutputs [][]float32
}

// NewSession loads an ONNX model from raw bytes and prepares it for inference.
func NewSession(modelData []byte) (*Session, error) {
	model, err := ParseModel(modelData)
	if err != nil {
		return nil, fmt.Errorf("parse model: %w", err)
	}

	s := &Session{
		model:        model,
		initializers: make(map[string]*Tensor),
		tensorIDs:    make(map[string]int),
	}

	// Load initializers (model weights)
	for _, init := range model.Graph.Initializers {
		t, err := tensorFromProto(init)
		if err != nil {
			return nil, fmt.Errorf("load initializer %q: %w", init.Name, err)
		}
		s.initializers[init.Name] = t
	}

	// Collect input/output names
	for _, inp := range model.Graph.Inputs {
		if _, isInit := s.initializers[inp.Name]; !isInit {
			s.inputNames = append(s.inputNames, inp.Name)
		}
	}
	for _, out := range model.Graph.Outputs {
		s.outputNames = append(s.outputNames, out.Name)
	}

	// Apply graph optimizations (operator fusion)
	s.execOrder = fuseGraph(model.Graph.Nodes)

	// Pre-parse attributes and resolve op functions once at load time
	s.cachedAttrs = make([]*Attributes, len(s.execOrder))
	s.nodeFuncs = make([]OpFunc, len(s.execOrder))
	for i, node := range s.execOrder {
		attrs, err := nodeAttrsToAttributes(node.Attrs)
		if err != nil {
			return nil, fmt.Errorf("node %q (%s): %w", node.Name, node.OpType, err)
		}
		s.cachedAttrs[i] = attrs
		fn, ok := Registry[node.OpType]
		if !ok {
			return nil, fmt.Errorf("unsupported operator %q", node.OpType)
		}
		s.nodeFuncs[i] = fn
	}

	// Build tensor ID index for fast lookup
	s.buildTensorIndex()

	slog.Info("ONNX session loaded",
		"nodes", len(s.execOrder),
		"initializers", len(s.initializers),
		"inputs", s.inputNames,
		"outputs", s.outputNames,
		"opset", model.OpsetVersion,
	)

	return s, nil
}

// buildTensorIndex assigns integer IDs to all tensor names for indexed access.
func (s *Session) buildTensorIndex() {
	id := func(name string) int {
		if idx, ok := s.tensorIDs[name]; ok {
			return idx
		}
		idx := s.numTensors
		s.tensorIDs[name] = idx
		s.numTensors++
		return idx
	}

	// Assign IDs to initializers
	for name, t := range s.initializers {
		idx := id(name)
		s.initIDs = append(s.initIDs, idx)
		s.initTensors = append(s.initTensors, t)
	}

	// Assign IDs to inputs
	for _, name := range s.inputNames {
		s.inputIDs = append(s.inputIDs, id(name))
	}

	// Assign IDs to all node inputs/outputs
	s.nodeInputIDs = make([][]int, len(s.execOrder))
	s.nodeOutputIDs = make([][]int, len(s.execOrder))
	for i, node := range s.execOrder {
		inIDs := make([]int, len(node.Inputs))
		for j, name := range node.Inputs {
			if name == "" {
				inIDs[j] = -1
			} else {
				inIDs[j] = id(name)
			}
		}
		s.nodeInputIDs[i] = inIDs

		outIDs := make([]int, len(node.Outputs))
		for j, name := range node.Outputs {
			if name == "" {
				outIDs[j] = -1
			} else {
				outIDs[j] = id(name)
			}
		}
		s.nodeOutputIDs[i] = outIDs
	}

	// Assign IDs to outputs
	for _, name := range s.outputNames {
		s.outputIDs = append(s.outputIDs, id(name))
	}

	// Determine which tensor IDs are recyclable (not init, not input, not output)
	s.recyclable = make([]bool, s.numTensors)
	for i := range s.recyclable {
		s.recyclable[i] = true
	}
	for _, idx := range s.initIDs {
		s.recyclable[idx] = false
	}
	for _, idx := range s.inputIDs {
		s.recyclable[idx] = false
	}
	for _, idx := range s.outputIDs {
		s.recyclable[idx] = false
	}
}

// InputNames returns the model input tensor names.
func (s *Session) InputNames() []string {
	return s.inputNames
}

// OutputNames returns the model output tensor names.
func (s *Session) OutputNames() []string {
	return s.outputNames
}

// Run executes the model with the given input tensors and returns the output tensors.
func (s *Session) Run(inputs map[string]*Tensor) (map[string]*Tensor, error) {
	// Inference must not touch the process-global GC setting. Sessions are
	// per-camera and run concurrently, so a per-call SetGCPercent save/restore
	// races: one goroutine reads a value another already lowered and then
	// "restores" it, latching GC off process-wide and letting the heap ramp
	// until the memory guard restarts the process. GC pacing is governed
	// globally instead (GOGC plus the GOMEMLIMIT backstop set at startup).

	// Return previous run's output buffers to the free list
	for _, buf := range s.prevOutputs {
		putTensorData(buf)
	}
	s.prevOutputs = s.prevOutputs[:0]

	// Reuse the values slice across runs
	if s.values == nil {
		s.values = make([]*Tensor, s.numTensors)
		s.inputBuf = make([]*Tensor, 8)
	}
	values := s.values
	for i := range values {
		values[i] = nil
	}

	// Pre-populate initializers
	for i, idx := range s.initIDs {
		values[idx] = s.initTensors[i]
	}

	// Pre-populate user inputs
	for i, name := range s.inputNames {
		t, ok := inputs[name]
		if !ok {
			return nil, fmt.Errorf("input %q not provided", name)
		}
		values[s.inputIDs[i]] = t
	}

	// Execute nodes in order
	for i, node := range s.execOrder {
		// Gather inputs using precomputed IDs
		inIDs := s.nodeInputIDs[i]
		nIn := len(inIDs)
		var nodeInputs []*Tensor
		if nIn <= len(s.inputBuf) {
			nodeInputs = s.inputBuf[:nIn]
		} else {
			nodeInputs = make([]*Tensor, nIn)
		}
		for j, tid := range inIDs {
			if tid < 0 {
				nodeInputs[j] = nil
			} else {
				nodeInputs[j] = values[tid]
			}
		}

		// Execute using pre-resolved function pointer
		outputs, err := s.nodeFuncs[i](nodeInputs, s.cachedAttrs[i])
		if err != nil {
			return nil, fmt.Errorf("node %d (%s/%s): %w", i, node.Name, node.OpType, err)
		}

		// Store outputs using precomputed IDs
		outIDs := s.nodeOutputIDs[i]
		for j, tid := range outIDs {
			if tid >= 0 && j < len(outputs) {
				values[tid] = outputs[j]
			}
		}
	}

	// Collect requested outputs
	result := make(map[string]*Tensor, len(s.outputNames))
	for i, name := range s.outputNames {
		t := values[s.outputIDs[i]]
		if t == nil {
			return nil, fmt.Errorf("output %q not produced by graph", name)
		}
		result[name] = t
		// Track output buffers for recycling on next run
		if t.pooled {
			s.prevOutputs = append(s.prevOutputs, t.Data)
			t.pooled = false
		}
	}

	// Return pooled intermediate tensor buffers
	for idx, t := range values {
		if t != nil && t.pooled && s.recyclable[idx] {
			putTensorData(t.Data)
			t.pooled = false
		}
	}

	return result, nil
}

// fuseGraph applies graph-level optimizations to the node list.
// Currently fuses Conv + Sigmoid + Mul patterns into ConvSiLU.
func fuseGraph(nodes []*NodeProto) []*NodeProto {
	// Build output -> node index
	nodeByOutput := make(map[string]*NodeProto, len(nodes))
	for _, n := range nodes {
		for _, out := range n.Outputs {
			nodeByOutput[out] = n
		}
	}

	// Count consumers per tensor name
	consumers := make(map[string]int, len(nodes)*2)
	for _, n := range nodes {
		for _, inp := range n.Inputs {
			if inp != "" {
				consumers[inp]++
			}
		}
	}

	// Identify Sigmoid + Mul pairs that form SiLU: x * sigmoid(x)
	// Mark them for removal and tag the Conv that produces x as ConvSiLU
	fusedNodes := make(map[*NodeProto]bool)
	siluConvs := make(map[*NodeProto]string) // Conv node -> final output name

	for _, n := range nodes {
		if n.OpType != "Mul" || len(n.Inputs) < 2 {
			continue
		}

		// Check both input orderings: Mul(x, sigmoid(x)) or Mul(sigmoid(x), x)
		for sigIdx := range 2 {
			otherIdx := 1 - sigIdx
			sigNode, ok := nodeByOutput[n.Inputs[sigIdx]]
			if !ok || sigNode.OpType != "Sigmoid" {
				continue
			}

			sigInput := sigNode.Inputs[0]
			if n.Inputs[otherIdx] != sigInput {
				continue
			}

			// Found SiLU pattern. Check if the producer is Conv.
			producer, ok := nodeByOutput[sigInput]
			if !ok || producer.OpType != "Conv" {
				continue
			}

			// Verify Conv output is only consumed by Sigmoid and Mul (2 consumers)
			if consumers[sigInput] != 2 {
				continue
			}

			// Fuse: mark Conv as ConvSiLU, skip Sigmoid and Mul nodes
			siluConvs[producer] = n.Outputs[0]
			fusedNodes[sigNode] = true
			fusedNodes[n] = true
			break
		}
	}

	if len(siluConvs) == 0 {
		return nodes
	}

	// Rebuild node list with fusions applied
	result := make([]*NodeProto, 0, len(nodes)-2*len(siluConvs))
	for _, n := range nodes {
		if fusedNodes[n] {
			continue
		}
		if finalOutput, ok := siluConvs[n]; ok {
			// Replace Conv with ConvSiLU, output goes to the Mul's output name
			fused := &NodeProto{
				Name:    n.Name,
				OpType:  "ConvSiLU",
				Inputs:  n.Inputs,
				Outputs: []string{finalOutput},
				Attrs:   n.Attrs,
			}
			result = append(result, fused)
		} else {
			result = append(result, n)
		}
	}

	slog.Info("graph optimization", "fused_conv_silu", len(siluConvs),
		"nodes_before", len(nodes), "nodes_after", len(result))
	return result
}

// nodeAttrsToAttributes converts ONNX proto attributes to our Attributes type.
func nodeAttrsToAttributes(protoAttrs []*AttributeProto) (*Attributes, error) {
	attrs := NewAttributes()
	for _, a := range protoAttrs {
		switch {
		case a.Type == attrFloat || (a.Type == 0 && a.F != 0):
			attrs.Floats[a.Name] = a.F
		case a.Type == attrInt || (a.Type == 0 && a.I != 0):
			attrs.Ints[a.Name] = a.I
		case a.Type == attrString || (a.Type == 0 && len(a.S) > 0):
			attrs.Strings[a.Name] = string(a.S)
		case a.Type == attrTensor:
			if a.T != nil {
				t, err := tensorFromProto(a.T)
				if err != nil {
					return nil, fmt.Errorf("attribute %q: %w", a.Name, err)
				}
				attrs.Tensors[a.Name] = t
			}
		case a.Type == attrFloats:
			attrs.FloatLists[a.Name] = a.Floats
		case a.Type == attrInts:
			attrs.IntLists[a.Name] = a.Ints
		}
	}
	return attrs, nil
}

// tensorFromProto decodes a TensorProto and checks that its data fills the shape
// it declares. A tensor short of its declared element count loads without
// complaint and then indexes out of range during inference, which surfaces as a
// recovered panic on every frame rather than as a model that fails to load.
func tensorFromProto(tp *TensorProto) (*Tensor, error) {
	data, err := tp.ToFloat32()
	if err != nil {
		return nil, err
	}
	if want := tp.ElementCount(); int64(len(data)) != want {
		return nil, fmt.Errorf("shape %v needs %d elements, data holds %d", tp.Dims, want, len(data))
	}
	return NewTensor(tp.Dims, data), nil
}
