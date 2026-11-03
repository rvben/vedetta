"""Export the pinned official YAMNet weights to Vedetta's local Go backend.

No downloads. Preserve TensorFlow's Apache-2.0 license with the output bundle.
The conversion retains all 521 outputs and checks each against the reference.
"""
import argparse
import hashlib
import json
from pathlib import Path

import numpy as np
import onnx
from onnx import helper, numpy_helper, TensorProto
import onnxruntime as ort
from scipy.signal import firwin
import tensorflow as tf

from replay import class_index, model_fingerprint


def export(directory, output):
    tf.config.threading.set_intra_op_parallelism_threads(1)
    tf.config.threading.set_inter_op_parallelism_threads(1)
    reference = tf.saved_model.load(str(directory))
    weights = {v.name.removesuffix(":0"): v.numpy() for v in reference._yamnet.variables}
    nodes, tensors = [], []
    current = "mel"

    def constant(name, data):
        tensors.append(numpy_helper.from_array(data.astype(np.float32), name))
        return name

    def conv(name, stride, depthwise=False):
        nonlocal current
        key = "depthwise_kernel" if depthwise else "kernel"
        kernel = weights[name+"/"+key]
        kernel = kernel.transpose(2, 3, 0, 1) if depthwise else kernel.transpose(3, 2, 0, 1)
        scale = 1 / np.sqrt(weights[name+"/bn/moving_variance"] + 1e-4)
        bias = weights[name+"/bn/beta"] - weights[name+"/bn/moving_mean"] * scale
        kernel = kernel * scale[:, None, None, None]
        nodes.append(helper.make_node("Conv", [current, constant(name+"_w", kernel), constant(name+"_b", bias)], [name], strides=[stride, stride], auto_pad="SAME_UPPER", group=kernel.shape[0] if depthwise else 1))
        current = name+"_relu"
        nodes.append(helper.make_node("Relu", [name], [current]))

    conv("layer1/conv", 2)
    for layer in range(2, 15):
        conv(f"layer{layer}/depthwise_conv", 2 if layer in (3, 5, 7, 13) else 1, True)
        conv(f"layer{layer}/pointwise_conv", 1)
    nodes.append(helper.make_node("GlobalAveragePool", [current], ["pooled"]))
    # A 1x1 convolution is exactly the reference Dense on pooled embeddings.
    dense = weights["dense/kernel"].T[:, :, None, None]
    nodes.append(helper.make_node("Conv", ["pooled", constant("dense_w", dense), constant("dense_b", weights["dense/bias"])], ["logits"]))
    nodes.append(helper.make_node("Sigmoid", ["logits"], ["scores"]))
    graph = helper.make_graph(nodes, "YAMNet", [helper.make_tensor_value_info("mel", TensorProto.FLOAT, [1, 1, 96, 64])], [helper.make_tensor_value_info("scores", TensorProto.FLOAT, [1, 521, 1, 1])], tensors)
    model = helper.make_model(graph, opset_imports=[helper.make_opsetid("", 13)], ir_version=8)
    onnx.checker.check_model(model)
    output.mkdir(parents=True, exist_ok=True)
    # Keep the official license with every generated model bundle.
    (output/"LICENSE").write_bytes((Path(__file__).parent/"YAMNET-LICENSE").read_bytes())
    (output/"NOTICE").write_text("YAMNet: Copyright 2019 The TensorFlow Authors.\nConverted from the official YAMNet SavedModel; Apache License 2.0.\nhttps://github.com/tensorflow/models/tree/master/research/audioset/yamnet\n")
    data = model.SerializeToString()
    (output/"yamnet.onnx").write_bytes(data)
    mel = tf.signal.linear_to_mel_weight_matrix(64, 257, 16000, 125, 7500).numpy()
    manifest = dict(version=1, model="yamnet.onnx", sha256=hashlib.sha256(data).hexdigest(), source_sha256=model_fingerprint(directory), cry_index=class_index(directory/"assets/yamnet_class_map.csv"), mel_weights=mel.flatten().tolist(), resample=(firwin(41, .5, window=("kaiser", 5.0))*2).tolist())
    manifest_data = (json.dumps(manifest, separators=(",", ":"))+"\n").encode()
    (output/"manifest.json").write_bytes(manifest_data)
    options = ort.SessionOptions()
    options.intra_op_num_threads = 1
    session = ort.InferenceSession(data, options, providers=["CPUExecutionProvider"])
    maximum = 0
    # Real audio parity is checked separately by the Go fixture generator.
    for waveform in (np.zeros(15600, np.float32), np.random.default_rng(42).normal(0, .05, 15600).astype(np.float32)):
        scores, _, spectrogram = reference(waveform)
        got = session.run(None, {"mel": spectrogram.numpy()[:96][None, None]})[0].reshape(-1)
        error = float(np.max(np.abs(got-scores.numpy()[0])))
        maximum = max(maximum, error)
    if maximum > 1e-4:
        raise ValueError(f"conversion parity failed: {maximum}")
    print(json.dumps(dict(manifest_sha256=hashlib.sha256(manifest_data).hexdigest(), max_score_error=maximum)))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("reference", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    export(args.reference, args.output)
