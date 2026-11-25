#ifndef VEDETTA_COREML_DARWIN_H
#define VEDETTA_COREML_DARWIN_H

#include <stddef.h>

// Every function that can fail reports the reason through err as a malloc'd
// string the caller frees. err is left untouched on success.

// vd_coreml_compile compiles the model package at src for this machine and
// moves the result to dst, which must not exist. Returns 0 on success.
int vd_coreml_compile(const char *src, const char *dst, char **err);

// vd_coreml_default_model_supported reports whether this system can load the
// default model, which targets macOS 15.
int vd_coreml_default_model_supported(void);

// vd_coreml_open loads the compiled model at path and returns an opaque handle,
// or NULL on failure. The model must take one float32 array of exactly
// inputShape and return one of exactly outputShape; a model with any other
// layout is refused, because its scalars would be read in the wrong order.
void *vd_coreml_open(const char *path, const size_t *inputShape, size_t inputDims, const size_t *outputShape,
                     size_t outputDims, char **err);

// vd_coreml_run copies one input array from input into the model, runs it, and
// copies one output array to output. Returns 0 on success. A handle
// serves one call at a time.
int vd_coreml_run(void *handle, const float *input, float *output, char **err);

// vd_coreml_close releases a handle from vd_coreml_open. NULL is accepted.
void vd_coreml_close(void *handle);

#endif
