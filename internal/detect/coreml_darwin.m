#import <CoreML/CoreML.h>
#import <Foundation/Foundation.h>

#include <stdlib.h>
#include <string.h>

#include "coreml_darwin.h"

// VDCoreMLModel keeps a loaded model together with the input array it reuses
// for every prediction.
@interface VDCoreMLModel : NSObject
@property(nonatomic, strong) MLModel *model;
@property(nonatomic, strong) MLMultiArray *input;
@property(nonatomic, copy) NSString *inputName;
@property(nonatomic, copy) NSString *outputName;
@property(nonatomic, copy) NSArray<NSNumber *> *outputShape;
@property(nonatomic) size_t inputCount;
@property(nonatomic) size_t outputCount;
@end

@implementation VDCoreMLModel
@end

static void vd_set_error(char **err, NSString *context, NSError *error) {
    if (err == NULL) {
        return;
    }
    NSString *message = context;
    if (error != nil) {
        message = [NSString stringWithFormat:@"%@: %@", context, error.localizedDescription];
    }
    *err = strdup(message.UTF8String);
}

// vd_shape_matches reports whether a model's shape is exactly the wanted one.
static BOOL vd_shape_matches(NSArray<NSNumber *> *shape, const size_t *want, size_t dims) {
    if (shape.count != dims) {
        return NO;
    }
    for (size_t i = 0; i < dims; i++) {
        if (shape[i].integerValue < 0 || (size_t)shape[i].integerValue != want[i]) {
            return NO;
        }
    }
    return YES;
}

static NSString *vd_shape_string(const size_t *shape, size_t dims) {
    NSMutableArray<NSString *> *parts = [NSMutableArray arrayWithCapacity:dims];
    for (size_t i = 0; i < dims; i++) {
        [parts addObject:[NSString stringWithFormat:@"%zu", shape[i]]];
    }
    return [parts componentsJoinedByString:@","];
}

static size_t vd_scalar_count(const size_t *shape, size_t dims) {
    size_t count = 1;
    for (size_t i = 0; i < dims; i++) {
        count *= shape[i];
    }
    return count;
}

int vd_coreml_default_model_supported(void) {
    if (@available(macOS 15.0, *)) {
        return 1;
    }
    return 0;
}

// vd_is_dense reports whether an array stores its scalars in row-major order
// with no gaps, which is what lets its buffer be copied as one block.
static BOOL vd_is_dense(NSArray<NSNumber *> *shape, NSArray<NSNumber *> *strides) {
    if (shape.count != strides.count) {
        return NO;
    }
    NSInteger expected = 1;
    for (NSInteger i = (NSInteger)shape.count - 1; i >= 0; i--) {
        if (strides[i].integerValue != expected) {
            return NO;
        }
        expected *= shape[i].integerValue;
    }
    return YES;
}

int vd_coreml_compile(const char *src, const char *dst, char **err) {
    @autoreleasepool {
        NSURL *source = [NSURL fileURLWithPath:@(src)];
        NSURL *destination = [NSURL fileURLWithPath:@(dst)];

        NSError *error = nil;
        NSURL *compiled = [MLModel compileModelAtURL:source error:&error];
        if (compiled == nil) {
            vd_set_error(err, @"compile model", error);
            return -1;
        }

        // Core ML writes the compiled model to a temporary directory the system
        // may clear, so it is moved to where the caller keeps it.
        NSFileManager *files = [NSFileManager defaultManager];
        if (![files moveItemAtURL:compiled toURL:destination error:&error]) {
            [files removeItemAtURL:compiled error:nil];
            vd_set_error(err, @"store compiled model", error);
            return -1;
        }
        return 0;
    }
}

void *vd_coreml_open(const char *path, const size_t *wantInput, size_t inputDims, const size_t *wantOutput,
                     size_t outputDims, char **err) {
    @autoreleasepool {
        MLModelConfiguration *config = [[MLModelConfiguration alloc] init];
        // The GPU, with the CPU for anything it cannot run. The Neural Engine is
        // left out on purpose: it computes in half precision, and for this model
        // it measured several times slower than the GPU.
        config.computeUnits = MLComputeUnitsCPUAndGPU;

        NSError *error = nil;
        MLModel *model = [MLModel modelWithContentsOfURL:[NSURL fileURLWithPath:@(path)]
                                           configuration:config
                                                   error:&error];
        if (model == nil) {
            vd_set_error(err, @"load model", error);
            return NULL;
        }

        NSDictionary<NSString *, MLFeatureDescription *> *inputs = model.modelDescription.inputDescriptionsByName;
        NSDictionary<NSString *, MLFeatureDescription *> *outputs = model.modelDescription.outputDescriptionsByName;
        if (inputs.count != 1 || outputs.count != 1) {
            vd_set_error(err,
                         [NSString stringWithFormat:@"model has %lu inputs and %lu outputs, want one of each",
                                                    (unsigned long)inputs.count, (unsigned long)outputs.count],
                         nil);
            return NULL;
        }

        MLFeatureDescription *inputDesc = inputs.allValues.firstObject;
        MLFeatureDescription *outputDesc = outputs.allValues.firstObject;
        MLMultiArrayConstraint *inputShape = inputDesc.multiArrayConstraint;
        MLMultiArrayConstraint *outputShape = outputDesc.multiArrayConstraint;
        if (inputDesc.type != MLFeatureTypeMultiArray || outputDesc.type != MLFeatureTypeMultiArray ||
            inputShape.dataType != MLMultiArrayDataTypeFloat32 || outputShape.dataType != MLMultiArrayDataTypeFloat32) {
            vd_set_error(err, @"model input and output must be float32 arrays", nil);
            return NULL;
        }

        if (!vd_shape_matches(inputShape.shape, wantInput, inputDims) ||
            !vd_shape_matches(outputShape.shape, wantOutput, outputDims)) {
            vd_set_error(err,
                         [NSString stringWithFormat:@"model maps (%@) to (%@), want (%@) to (%@)",
                                                    [inputShape.shape componentsJoinedByString:@","],
                                                    [outputShape.shape componentsJoinedByString:@","],
                                                    vd_shape_string(wantInput, inputDims),
                                                    vd_shape_string(wantOutput, outputDims)],
                         nil);
            return NULL;
        }

        MLMultiArray *input = [[MLMultiArray alloc] initWithShape:inputShape.shape
                                                         dataType:MLMultiArrayDataTypeFloat32
                                                            error:&error];
        if (input == nil) {
            vd_set_error(err, @"allocate input", error);
            return NULL;
        }

        VDCoreMLModel *handle = [[VDCoreMLModel alloc] init];
        handle.model = model;
        handle.input = input;
        handle.inputName = inputDesc.name;
        handle.outputName = outputDesc.name;
        handle.outputShape = outputShape.shape;
        handle.inputCount = vd_scalar_count(wantInput, inputDims);
        handle.outputCount = vd_scalar_count(wantOutput, outputDims);

        return (void *)CFBridgingRetain(handle);
    }
}

int vd_coreml_run(void *ptr, const float *input, float *output, char **err) {
    @autoreleasepool {
        VDCoreMLModel *handle = (__bridge VDCoreMLModel *)ptr;
        size_t inputBytes = handle.inputCount * sizeof(float);
        size_t outputBytes = handle.outputCount * sizeof(float);

        __block BOOL copied = NO;
        [handle.input getMutableBytesWithHandler:^(void *bytes, NSInteger size, NSArray<NSNumber *> *strides) {
            if ((size_t)size == inputBytes && vd_is_dense(handle.input.shape, strides)) {
                memcpy(bytes, input, inputBytes);
                copied = YES;
            }
        }];
        if (!copied) {
            vd_set_error(err, @"input array is not a dense float32 buffer of the model's size", nil);
            return -1;
        }

        NSError *error = nil;
        MLDictionaryFeatureProvider *features =
            [[MLDictionaryFeatureProvider alloc] initWithDictionary:@{handle.inputName : handle.input} error:&error];
        if (features == nil) {
            vd_set_error(err, @"build input", error);
            return -1;
        }

        id<MLFeatureProvider> result = [handle.model predictionFromFeatures:features error:&error];
        if (result == nil) {
            vd_set_error(err, @"predict", error);
            return -1;
        }

        MLMultiArray *array = [result featureValueForName:handle.outputName].multiArrayValue;
        // The shape is compared again here because a model's description gives
        // the shape it defaults to, which a prediction is not bound to return.
        if (array == nil || array.dataType != MLMultiArrayDataTypeFloat32 ||
            ![array.shape isEqualToArray:handle.outputShape] || !vd_is_dense(array.shape, array.strides)) {
            vd_set_error(err, @"output is not a dense float32 array of the model's shape", nil);
            return -1;
        }

        copied = NO;
        [array getBytesWithHandler:^(const void *bytes, NSInteger size) {
            if ((size_t)size >= outputBytes) {
                memcpy(output, bytes, outputBytes);
                copied = YES;
            }
        }];
        if (!copied) {
            vd_set_error(err, @"output buffer is smaller than the model's output", nil);
            return -1;
        }
        return 0;
    }
}

void vd_coreml_close(void *ptr) {
    if (ptr != NULL) {
        CFBridgingRelease(ptr);
    }
}
