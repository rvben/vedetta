"""Create local reference fixtures; do not commit audio, weights or fixtures."""
import argparse
import json
from pathlib import Path

import numpy as np
from scipy.signal import resample_poly
import tensorflow as tf

from replay import load_pcm


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("reference", type=Path)
    parser.add_argument("manifest", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    tf.config.threading.set_intra_op_parallelism_threads(1)
    tf.config.threading.set_inter_op_parallelism_threads(1)
    model = tf.saved_model.load(str(args.reference))
    args.output.mkdir(parents=True, exist_ok=True)
    inputs = [("silence", np.zeros(16000, dtype=np.int16)), ("noise", (np.random.default_rng(7).normal(0, .1, 16000)*32768).astype(np.int16))]
    for row in json.loads(args.manifest.read_text()):
        waveform = load_pcm(args.manifest.parent/row["path"], "pcma")
        inputs.append((row["path"], np.clip(waveform[:16000]*32768, -32768, 32767).astype(np.int16)))
    for index, (name, pcm) in enumerate(inputs):
        waveform = resample_poly(pcm.astype(np.float32)/32768, 2, 1).astype(np.float32)
        scores, _, mel = model(waveform)
        fixture = dict(name=name, pcm=pcm.tolist(), scores=scores.numpy().tolist(), mel=mel.numpy().tolist())
        (args.output/f"{index:03}.json").write_text(json.dumps(fixture))
    print(f"Created {len(inputs)} local parity fixtures")


if __name__ == "__main__": main()
