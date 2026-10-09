#!/usr/bin/env python3
"""Local CPU benchmark. No RTSP connections, uploads or alert delivery."""
import argparse
import contextlib
import csv
import hashlib
import importlib.metadata
import io
import json
import math
import os
from pathlib import Path
import platform
import resource
import sys
import time
import wave

from metrics import summarize, summarize_clips, validate_manifest

CRY_MID = "/t/dd00002"


def model_fingerprint(path):
    if path.is_file():
        with path.open("rb") as f:
            return hashlib.file_digest(f, "sha256").hexdigest()
    digest = hashlib.sha256()
    for file in sorted(p for p in path.rglob("*") if p.is_file()):
        digest.update(str(file.relative_to(path)).encode()+b"\0")
        with file.open("rb") as f:
            digest.update(hashlib.file_digest(f, "sha256").digest())
    return digest.hexdigest()


def class_index(path):
    with open(path, newline="") as f:
        rows = list(csv.DictReader(f))
    matches = [int(r["index"]) for r in rows if r["mid"] == CRY_MID]
    if len(matches) != 1:
        raise ValueError("class map must contain exactly one infant cry MID")
    return matches[0]


def load_pcm(path, codec):
    import numpy as np
    from scipy.signal import resample_poly
    with wave.open(str(path), "rb") as f:
        if f.getsampwidth() != 2 or f.getcomptype() != "NONE":
            raise ValueError("replay input must be uncompressed PCM16 WAV")
        rate, channels = f.getframerate(), f.getnchannels()
        pcm = np.frombuffer(f.readframes(f.getnframes()), dtype="<i2").reshape(-1, channels)
    pcm = pcm.astype(np.float32).mean(axis=1) / 32768
    # Both models receive the same camera-bandwidth waveform. Polyphase
    # antialiasing is required when reducing sample rate, unlike interpolation.
    divisor = math.gcd(rate, 8000)
    pcm = resample_poly(pcm, 8000//divisor, rate//divisor).astype(np.float32)
    if codec != "pcm":
        # FFmpeg's G.711 encoder/decoder provides a reproducible camera-codec
        # round trip. Never normalize amplitude or gate quiet windows.
        import subprocess
        fmt = "alaw" if codec == "pcma" else "mulaw"
        raw = np.clip(pcm*32768, -32768, 32767).astype("<i2").tobytes()
        encoded = subprocess.run(["ffmpeg", "-v", "error", "-f", "s16le", "-ar", "8000", "-ac", "1", "-i", "pipe:0", "-f", fmt, "pipe:1"], input=raw, capture_output=True, check=True).stdout
        decoded = subprocess.run(["ffmpeg", "-v", "error", "-f", fmt, "-ar", "8000", "-ac", "1", "-i", "pipe:0", "-f", "s16le", "pipe:1"], input=encoded, capture_output=True, check=True).stdout
        pcm = np.frombuffer(decoded, dtype="<i2").astype(np.float32) / 32768
    return pcm


class YAMNet:
    def __init__(self, directory, threads):
        import tensorflow as tf
        tf.config.threading.set_intra_op_parallelism_threads(threads)
        tf.config.threading.set_inter_op_parallelism_threads(1)
        self.model = tf.saved_model.load(str(directory))
        self.index = class_index(directory / "assets/yamnet_class_map.csv")

    def score(self, pcm):
        from scipy.signal import resample_poly
        scores, _, _ = self.model(resample_poly(pcm, 2, 1).astype("float32"))
        # Equal context with EfficientAT; clip-level average, not a
        # probability. YAMNet's native padding remains part of its reference.
        return float(scores.numpy()[:, self.index].mean())


class EfficientAT:
    def __init__(self, source, checkpoint, threads, export, samples=40000):
        import numpy as np
        import torch
        torch.set_num_threads(threads)
        torch.set_num_interop_threads(1)
        sys.path.insert(0, str(source.resolve()))
        # Upstream helpers read metadata relative to cwd at import time.
        cwd = os.getcwd()
        try:
            os.chdir(source)
            from models.mn.model import get_model
            from models.preprocess import AugmentMelSTFT
            with contextlib.redirect_stdout(io.StringIO()):
                self.model = get_model().eval()
                self.preprocess = AugmentMelSTFT().eval()
        finally:
            os.chdir(cwd)
        self.model.load_state_dict(torch.load(checkpoint, map_location="cpu", weights_only=True))
        self.index = class_index(source / "metadata/class_labels_indices.csv")
        self.ort = None
        self.max_parity_error = 0.0
        if export:
            import onnxruntime as ort
            dummy = self.features(np.random.default_rng(0).normal(0, 0.05, samples).astype(np.float32))
            with torch.no_grad():
                original_logits = self.model(dummy)[0].clone()
                torch.onnx.export(self.model, dummy, str(export), input_names=["mel"], output_names=["logits", "embedding"], opset_version=17, dynamo=False)
                if not torch.allclose(original_logits, self.model(dummy)[0], atol=1e-5, rtol=1e-5):
                    raise ValueError("ONNX export changed reference model outputs")
            options = ort.SessionOptions()
            options.intra_op_num_threads = threads
            options.inter_op_num_threads = 1
            self.ort = ort.InferenceSession(str(export), sess_options=options, providers=["CPUExecutionProvider"])

    def features(self, pcm):
        import torch
        from scipy.signal import resample_poly
        waveform = torch.from_numpy(resample_poly(pcm, 4, 1).astype("float32"))[None]
        with torch.no_grad():
            return self.preprocess(waveform)[:, None]

    def score(self, pcm):
        import numpy as np
        import torch
        features = self.features(pcm)
        with torch.no_grad():
            logits, _ = self.model(features)
        if self.ort:
            converted = self.ort.run(None, {"mel": features.numpy()})[0]
            if not np.isfinite(converted).all() or not np.isfinite(logits.numpy()).all():
                raise ValueError("non-finite ONNX/reference logits")
            error = float(np.max(np.abs(converted - logits.numpy())))
            self.max_parity_error = max(self.max_parity_error, error)
            if error > 1e-4:
                raise ValueError(f"ONNX parity failed: max logit error {error}")
        return float(logits.sigmoid()[0, self.index])


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("manifest", type=Path)
    p.add_argument("--model", choices=["yamnet", "efficientat"], required=True)
    p.add_argument("--yamnet", type=Path)
    p.add_argument("--efficientat-source", type=Path)
    p.add_argument("--checkpoint", type=Path)
    p.add_argument("--export-onnx", type=Path, help="EfficientAT core only; check logits on every replay window")
    p.add_argument("--codec", choices=["pcma", "pcmu", "pcm"], default="pcma")
    p.add_argument("--threads", type=int, default=1)
    p.add_argument("--window", type=float, default=5, help="common context in seconds; short EfficientAT contexts require separate validation")
    p.add_argument("--hop", type=float, default=0.48)
    p.add_argument("--output", type=Path, required=True)
    p.add_argument("--smoke", action="store_true", help="weak clip labels only; never report episode accuracy or camera-night rates")
    p.add_argument("--split", choices=["validation", "test"], default="test")
    p.add_argument("--threshold", type=float, default=0.2, help="lock on validation data before evaluating the test split")
    p.add_argument("--within-subject", action="store_true", help="same infant may occur in different sessions/splits; not cross-infant generalization")
    args = p.parse_args()
    if args.threads < 1 or args.threads > 16:
        p.error("threads must be 1..16")
    if not math.isfinite(args.window) or not 0.975 <= args.window <= 10 or not math.isfinite(args.hop) or not 0.001 <= args.hop <= args.window:
        p.error("window must be .975..10 s and hop .001..window s")
    size, hop = round(args.window*8000), round(args.hop*8000)
    if not math.isfinite(args.threshold) or not 0 <= args.threshold <= 1:
        p.error("threshold must be a finite score in 0..1")
    if args.model == "yamnet" and not args.yamnet:
        p.error("--yamnet is required")
    if args.model == "efficientat" and (not args.efficientat_source or not args.checkpoint):
        p.error("--efficientat-source and --checkpoint are required")
    if args.export_onnx and args.model != "efficientat":
        p.error("ONNX export is currently supported for EfficientAT only")
    import numpy as np
    rows = json.loads(args.manifest.read_text())
    if args.smoke:
        if not rows or any(type(r.get("cry")) is not bool for r in rows):
            raise ValueError("smoke manifest needs boolean cry labels")
    else:
        validate_manifest(rows, require_subject_holdout=not args.within_subject)
        rows = [r for r in rows if r["split"] == args.split]
    if not rows:
        raise ValueError("empty manifest")
    model = YAMNet(args.yamnet, args.threads) if args.model == "yamnet" else EfficientAT(args.efficientat_source, args.checkpoint, args.threads, args.export_onnx, size)
    for _ in range(3):
        model.score(np.zeros(size, dtype=np.float32))
    scores, durations = [], []
    for row in rows:
        path = args.manifest.parent / row["path"]
        pcm = load_pcm(path, args.codec)
        if abs(len(pcm)/8000 - row["duration"]) > 0.02:
            raise ValueError(f"WAV duration differs from annotations: {path.name}")
        if len(pcm) < size:
            raise ValueError("recording shorter than the benchmark window")
        windows = []
        for start in range(0, len(pcm)-size+1, hop):
            before = time.perf_counter()
            score = model.score(pcm[start:start+size])
            if not math.isfinite(score) or not 0 <= score <= 1:
                raise ValueError("non-finite or invalid cry score")
            durations.append(time.perf_counter()-before)
            windows.append(((start+size)/8000, score))
        scores.append(windows)
    report = {
        "model": args.model, "codec": args.codec, "window_s": size/8000, "hop_s": hop/8000,
        "threads": args.threads, "host": {"system": platform.system(), "machine": platform.machine(), "processor": platform.processor()},
        "manifest_sha256": hashlib.sha256(args.manifest.read_bytes()).hexdigest(),
        "model_sha256": model_fingerprint(args.yamnet if args.model == "yamnet" else args.checkpoint),
        "versions": {name: importlib.metadata.version(name) for name in (["numpy", "scipy", "tensorflow"] if args.model == "yamnet" else ["numpy", "scipy", "torch", "torchaudio", "torchvision", "onnx", "onnxruntime"])},
        "windows": len(durations), "inference_including_preprocessing_p50_ms": float(np.percentile(durations, 50)*1000),
        "inference_including_preprocessing_p95_ms": float(np.percentile(durations, 95)*1000),
        "peak_process_rss_mib": resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / (1024**2 if sys.platform == "darwin" else 1024),
        "max_onnx_logit_error": getattr(model, "max_parity_error", None),
        "timing_includes_parity_double_inference": bool(args.export_onnx),
        "evaluation": "weak-label smoke; no real nursery accuracy claim" if args.smoke else f"reviewed episodes ({args.split})",
        "generalization": "not measured" if args.smoke else ("within-subject, held-out sessions" if args.within_subject else "held-out subjects and sessions"),
        "thresholds": (
            [summarize_clips(rows, scores, t) for t in (0.1, 0.2, 0.3, 0.5)]
            if args.smoke else
            [summarize(rows, scores, t, hop=hop/8000) for t in
             ((0.1, 0.2, 0.3, 0.5) if args.split == "validation" else (args.threshold,))]
        ),
        "scores": scores,
    }
    args.output.write_text(json.dumps(report, indent=2)+"\n")
    print(json.dumps({k:v for k,v in report.items() if k != "scores"}, indent=2))


if __name__ == "__main__":
    main()
