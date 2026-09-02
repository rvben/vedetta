//go:build cgo_onnxruntime

package detect

/*
#cgo LDFLAGS: -lonnxruntime
#cgo CFLAGS: -Wall

#include <onnxruntime_c_api.h>
#include <stdlib.h>
#include <string.h>

// The ORT C API exposes function pointers in the OrtApi struct.
// CGo cannot call C function pointers directly, so we wrap each one.

static const OrtApi* get_ort_api(void) {
	return OrtGetApiBase()->GetApi(ORT_API_VERSION);
}

static OrtStatus* ort_CreateEnv(const OrtApi* api,
	OrtLoggingLevel level, const char* name, OrtEnv** out) {
	return api->CreateEnv(level, name, out);
}

static OrtStatus* ort_CreateSessionOptions(const OrtApi* api,
	OrtSessionOptions** out) {
	return api->CreateSessionOptions(out);
}

static OrtStatus* ort_SetIntraOpNumThreads(const OrtApi* api,
	OrtSessionOptions* opts, int threads) {
	return api->SetIntraOpNumThreads(opts, threads);
}

static OrtStatus* ort_SetGraphOptLevel(const OrtApi* api,
	OrtSessionOptions* opts, GraphOptimizationLevel level) {
	return api->SetSessionGraphOptimizationLevel(opts, level);
}

static OrtStatus* ort_CreateSessionFromArray(const OrtApi* api,
	const OrtEnv* env, const void* data, size_t len,
	const OrtSessionOptions* opts, OrtSession** out) {
	return api->CreateSessionFromArray(env, data, len, opts, out);
}

static OrtStatus* ort_CreateCpuMemoryInfo(const OrtApi* api,
	OrtAllocatorType alloc, OrtMemType memtype, OrtMemoryInfo** out) {
	return api->CreateCpuMemoryInfo(alloc, memtype, out);
}

static OrtStatus* ort_CreateTensorWithData(const OrtApi* api,
	const OrtMemoryInfo* info, void* data, size_t data_len,
	const int64_t* shape, size_t shape_len,
	ONNXTensorElementDataType dtype, OrtValue** out) {
	return api->CreateTensorWithDataAsOrtValue(
		info, data, data_len, shape, shape_len, dtype, out);
}

static OrtStatus* ort_Run(const OrtApi* api,
	OrtSession* sess, const OrtRunOptions* opts,
	const char* const* input_names, const OrtValue* const* inputs, size_t ninputs,
	const char* const* output_names, size_t noutputs, OrtValue** outputs) {
	return api->Run(sess, opts, input_names, inputs, ninputs,
		output_names, noutputs, outputs);
}

static OrtStatus* ort_GetTensorMutableData(const OrtApi* api,
	OrtValue* val, void** out) {
	return api->GetTensorMutableData(val, out);
}

static OrtStatus* ort_GetTensorTypeAndShape(const OrtApi* api,
	const OrtValue* val, OrtTensorTypeAndShapeInfo** out) {
	return api->GetTensorTypeAndShape(val, out);
}

static OrtStatus* ort_GetTensorShapeElementCount(const OrtApi* api,
	const OrtTensorTypeAndShapeInfo* info, size_t* out) {
	return api->GetTensorShapeElementCount(info, out);
}

static OrtStatus* ort_SessionGetInputName(const OrtApi* api,
	const OrtSession* sess, size_t idx, OrtAllocator* alloc, char** out) {
	return api->SessionGetInputName(sess, idx, alloc, out);
}

static OrtStatus* ort_SessionGetOutputName(const OrtApi* api,
	const OrtSession* sess, size_t idx, OrtAllocator* alloc, char** out) {
	return api->SessionGetOutputName(sess, idx, alloc, out);
}

static OrtStatus* ort_GetAllocatorWithDefaultOptions(const OrtApi* api,
	OrtAllocator** out) {
	return api->GetAllocatorWithDefaultOptions(out);
}

static void ort_ReleaseSessionOptions(const OrtApi* api, OrtSessionOptions* p) {
	api->ReleaseSessionOptions(p);
}

static void ort_ReleaseSession(const OrtApi* api, OrtSession* p) {
	api->ReleaseSession(p);
}

static void ort_ReleaseEnv(const OrtApi* api, OrtEnv* p) {
	api->ReleaseEnv(p);
}

static void ort_ReleaseMemoryInfo(const OrtApi* api, OrtMemoryInfo* p) {
	api->ReleaseMemoryInfo(p);
}

static void ort_ReleaseValue(const OrtApi* api, OrtValue* p) {
	api->ReleaseValue(p);
}

static void ort_ReleaseTensorTypeAndShapeInfo(const OrtApi* api,
	OrtTensorTypeAndShapeInfo* p) {
	api->ReleaseTensorTypeAndShapeInfo(p);
}

// ort_check extracts the error message from an OrtStatus and releases it.
// Returns: NULL on success, or a malloc'd error string the caller must free.
// On malloc failure, returns a pointer to the static fallback string.
static const char* ort_oom_msg = "ORT error (out of memory copying message)";

// ort_oom_sentinel hands the fallback pointer to Go. cgo can call a static
// function but cannot bind a static variable, which has no external linkage, so
// referring to ort_oom_msg directly from Go fails at link time.
static const char* ort_oom_sentinel(void) {
	return ort_oom_msg;
}

static const char* ort_check(const OrtApi* api, OrtStatus* status) {
	if (status == NULL) return NULL;
	const char* msg = api->GetErrorMessage(status);
	size_t len = strlen(msg);
	char* copy = (char*)malloc(len + 1);
	if (copy == NULL) {
		api->ReleaseStatus(status);
		return ort_oom_msg;
	}
	memcpy(copy, msg, len + 1);
	api->ReleaseStatus(status);
	return copy;
}

// ort_alloc_free frees a string allocated by the ORT allocator.
static void ort_alloc_free(OrtAllocator* alloc, void* ptr) {
	alloc->Free(alloc, ptr);
}
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

// CAPIBackend wraps the ONNX Runtime C API for high-performance inference.
// Build with: go build -tags cgo_onnxruntime
//
// Not safe for concurrent use. Each goroutine needs its own instance.
type CAPIBackend struct {
	api     *C.OrtApi
	env     *C.OrtEnv
	session *C.OrtSession
	memInfo *C.OrtMemoryInfo

	inputName  *C.char
	outputName *C.char

	inputShape [4]C.int64_t

	// Reusable output buffer to avoid allocation per inference.
	outputBuf []float32
}

// capiBackendCompiledIn reports that this build carries the C ONNX Runtime
// backend, so selecting it is worth attempting at runtime.
const capiBackendCompiledIn = true

// NewCAPIBackend creates a C ONNX Runtime inference backend.
func NewCAPIBackend(modelData []byte) (*CAPIBackend, error) {
	if len(modelData) == 0 {
		return nil, fmt.Errorf("empty model data")
	}

	b := &CAPIBackend{}

	b.api = C.get_ort_api()
	if b.api == nil {
		return nil, fmt.Errorf("onnxruntime C API: failed to get API")
	}

	envName := C.CString("vedetta")
	defer C.free(unsafe.Pointer(envName))
	if err := b.check(C.ort_CreateEnv(b.api, C.ORT_LOGGING_LEVEL_WARNING, envName, &b.env)); err != nil {
		b.Close()
		return nil, fmt.Errorf("create env: %w", err)
	}

	var opts *C.OrtSessionOptions
	if err := b.check(C.ort_CreateSessionOptions(b.api, &opts)); err != nil {
		b.Close()
		return nil, fmt.Errorf("create session options: %w", err)
	}
	defer C.ort_ReleaseSessionOptions(b.api, opts)

	threads := runtime.NumCPU()
	if threads > 8 {
		threads = 8
	}
	if err := b.check(C.ort_SetIntraOpNumThreads(b.api, opts, C.int(threads))); err != nil {
		b.Close()
		return nil, fmt.Errorf("set threads: %w", err)
	}
	if err := b.check(C.ort_SetGraphOptLevel(b.api, opts, C.ORT_ENABLE_ALL)); err != nil {
		b.Close()
		return nil, fmt.Errorf("set optimization level: %w", err)
	}

	if err := b.check(C.ort_CreateSessionFromArray(
		b.api, b.env,
		unsafe.Pointer(&modelData[0]), C.size_t(len(modelData)),
		opts, &b.session,
	)); err != nil {
		b.Close()
		return nil, fmt.Errorf("create session: %w", err)
	}

	if err := b.check(C.ort_CreateCpuMemoryInfo(
		b.api, C.OrtArenaAllocator, C.OrtMemTypeDefault, &b.memInfo,
	)); err != nil {
		b.Close()
		return nil, fmt.Errorf("create memory info: %w", err)
	}

	// Read input/output names from the model.
	var alloc *C.OrtAllocator
	if err := b.check(C.ort_GetAllocatorWithDefaultOptions(b.api, &alloc)); err != nil {
		b.Close()
		return nil, fmt.Errorf("get allocator: %w", err)
	}

	var namePtr *C.char
	if err := b.check(C.ort_SessionGetInputName(b.api, b.session, 0, alloc, &namePtr)); err != nil {
		b.Close()
		return nil, fmt.Errorf("get input name: %w", err)
	}
	b.inputName = C.CString(C.GoString(namePtr))
	C.ort_alloc_free(alloc, unsafe.Pointer(namePtr))

	if err := b.check(C.ort_SessionGetOutputName(b.api, b.session, 0, alloc, &namePtr)); err != nil {
		b.Close()
		return nil, fmt.Errorf("get output name: %w", err)
	}
	b.outputName = C.CString(C.GoString(namePtr))
	C.ort_alloc_free(alloc, unsafe.Pointer(namePtr))

	b.inputShape = [4]C.int64_t{1, 3, 640, 640}

	return b, nil
}

// Run executes inference using the C ONNX Runtime.
func (b *CAPIBackend) Run(input []float32) ([]float32, error) {
	if len(input) != inputTensorSize {
		return nil, fmt.Errorf("input size %d, want %d", len(input), inputTensorSize)
	}

	// Pin Go memory so the GC won't relocate the input slice while C holds
	// a pointer to it (CreateTensorWithData stores the pointer, ort_Run reads it).
	var pinner runtime.Pinner
	pinner.Pin(&input[0])
	defer pinner.Unpin()

	var inputTensor *C.OrtValue
	if err := b.check(C.ort_CreateTensorWithData(
		b.api, b.memInfo,
		unsafe.Pointer(&input[0]),
		C.size_t(len(input)*4),
		&b.inputShape[0], 4,
		C.ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT,
		&inputTensor,
	)); err != nil {
		return nil, fmt.Errorf("create input tensor: %w", err)
	}
	defer C.ort_ReleaseValue(b.api, inputTensor)

	var outputTensor *C.OrtValue
	if err := b.check(C.ort_Run(
		b.api, b.session, nil,
		&b.inputName, &inputTensor, 1,
		&b.outputName, 1, &outputTensor,
	)); err != nil {
		return nil, fmt.Errorf("run inference: %w", err)
	}
	defer C.ort_ReleaseValue(b.api, outputTensor)

	var outputData unsafe.Pointer
	if err := b.check(C.ort_GetTensorMutableData(b.api, outputTensor, &outputData)); err != nil {
		return nil, fmt.Errorf("get output data: %w", err)
	}

	var info *C.OrtTensorTypeAndShapeInfo
	if err := b.check(C.ort_GetTensorTypeAndShape(b.api, outputTensor, &info)); err != nil {
		return nil, fmt.Errorf("get output shape: %w", err)
	}
	defer C.ort_ReleaseTensorTypeAndShapeInfo(b.api, info)

	var outputSize C.size_t
	if err := b.check(C.ort_GetTensorShapeElementCount(b.api, info, &outputSize)); err != nil {
		return nil, fmt.Errorf("get output element count: %w", err)
	}
	n := int(outputSize)

	// Reuse output buffer across runs.
	if cap(b.outputBuf) < n {
		b.outputBuf = make([]float32, n)
	}
	b.outputBuf = b.outputBuf[:n]

	C.memcpy(
		unsafe.Pointer(&b.outputBuf[0]),
		outputData,
		C.size_t(n*4),
	)

	return b.outputBuf, nil
}

// Close releases all C ONNX Runtime resources. Safe to call multiple times.
func (b *CAPIBackend) Close() {
	if b.api == nil {
		return
	}
	if b.inputName != nil {
		C.free(unsafe.Pointer(b.inputName))
		b.inputName = nil
	}
	if b.outputName != nil {
		C.free(unsafe.Pointer(b.outputName))
		b.outputName = nil
	}
	if b.memInfo != nil {
		C.ort_ReleaseMemoryInfo(b.api, b.memInfo)
		b.memInfo = nil
	}
	if b.session != nil {
		C.ort_ReleaseSession(b.api, b.session)
		b.session = nil
	}
	if b.env != nil {
		C.ort_ReleaseEnv(b.api, b.env)
		b.env = nil
	}
}

// Name returns the backend identifier.
func (b *CAPIBackend) Name() string {
	return "ONNX Runtime (C API)"
}

func (b *CAPIBackend) check(status *C.OrtStatus) error {
	msg := C.ort_check(b.api, status)
	if msg == nil {
		return nil
	}
	goMsg := C.GoString(msg)
	// Don't free the static OOM fallback string.
	if msg != C.ort_oom_sentinel() {
		C.free(unsafe.Pointer(msg))
	}
	return fmt.Errorf("%s", goMsg)
}
