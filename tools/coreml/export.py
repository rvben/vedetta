"""Convert the YOLO ONNX model into the Core ML package the coreml backend loads.

ONNX Runtime's Core ML execution provider translates an ONNX graph into a Core
ML ML Program and can keep the result on disk. This script runs that
translation once and packages its output, so the Core ML model is derived from
the same ONNX file every other backend runs and no second training export has
to be kept in step with it.

Run through `make coreml-model`, on macOS. Two exports of one ONNX file compute
the same function but are not the same bytes, because the translation lays the
weights out in an order that varies between runs. The checksum the backend pins
therefore names one particular export, and the backend's parity test is what
ties that export to the ONNX model.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import sys
import tempfile
import zipfile
from pathlib import Path

import numpy as np
import onnxruntime as ort

PACKAGE_NAME = "yolov8n.mlpackage"
AUTHOR = "com.microsoft.OnnxRuntime"

# A package manifest names its two items by UUID. The values only have to be
# distinct within the package.
MODEL_ITEM_ID = "00000000-0000-4000-8000-000000000001"
WEIGHTS_ITEM_ID = "00000000-0000-4000-8000-000000000002"

# Zip entries need a modification time; when the export ran is of no use to
# anyone reading the archive.
ZIP_TIMESTAMP = (2020, 1, 1, 0, 0, 0)


def translate(onnx_path: Path, cache_dir: Path) -> Path:
    """Runs the Core ML translation and returns the package directory it wrote."""
    if "CoreMLExecutionProvider" not in ort.get_available_providers():
        raise SystemExit("this onnxruntime build has no Core ML execution provider; run on macOS")

    options = ort.SessionOptions()
    # A node Core ML cannot run would stay behind on the CPU provider, and the
    # saved package would then hold only part of the model. Refusing the
    # fallback turns that into an error here.
    options.add_session_config_entry("session.disable_cpu_ep_fallback", "1")
    session = ort.InferenceSession(
        str(onnx_path),
        options,
        providers=[
            (
                "CoreMLExecutionProvider",
                {
                    "ModelFormat": "MLProgram",
                    "ModelCacheDirectory": str(cache_dir),
                    "RequireStaticInputShapes": "1",
                },
            )
        ],
    )

    # The package is written when the session first runs the model.
    model_input = session.get_inputs()[0]
    session.run(None, {model_input.name: np.zeros(model_input.shape, np.float32)})

    packages = [manifest.parent for manifest in cache_dir.rglob("Manifest.json")]
    if len(packages) != 1:
        raise SystemExit(
            f"expected the whole model in one Core ML package, found {len(packages)}"
        )
    return packages[0]


def package_files(package: Path) -> dict[str, bytes]:
    """Returns the archive entries for a translated package, keyed by path."""
    data_dir = package / "Data" / AUTHOR
    manifest = {
        "fileFormatVersion": "1.0.0",
        "itemInfoEntries": {
            MODEL_ITEM_ID: {
                "author": AUTHOR,
                "description": "CoreML Model Specification",
                "name": "model.mlmodel",
                "path": f"{AUTHOR}/model.mlmodel",
            },
            WEIGHTS_ITEM_ID: {
                "author": AUTHOR,
                "description": "CoreML Model Weights",
                "name": "weights",
                "path": f"{AUTHOR}/weights",
            },
        },
        "rootModelIdentifier": MODEL_ITEM_ID,
    }
    return {
        f"{PACKAGE_NAME}/Manifest.json": (json.dumps(manifest, indent=4, sort_keys=True) + "\n").encode(),
        f"{PACKAGE_NAME}/Data/{AUTHOR}/model.mlmodel": (data_dir / "model.mlmodel").read_bytes(),
        f"{PACKAGE_NAME}/Data/{AUTHOR}/weights/weight.bin": (data_dir / "weights" / "weight.bin").read_bytes(),
    }


def write_archive(files: dict[str, bytes], out_path: Path) -> None:
    out_path.parent.mkdir(parents=True, exist_ok=True)
    # Stored, not deflated: the weights do not compress.
    with zipfile.ZipFile(out_path, "w", zipfile.ZIP_STORED) as archive:
        for name in sorted(files):
            info = zipfile.ZipInfo(name, ZIP_TIMESTAMP)
            info.external_attr = 0o644 << 16
            archive.writestr(info, files[name])


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("onnx", type=Path, help="ONNX model to convert")
    parser.add_argument("out", type=Path, help="zip archive to write")
    args = parser.parse_args()

    with tempfile.TemporaryDirectory() as cache:
        files = package_files(translate(args.onnx, Path(cache)))
    write_archive(files, args.out)

    digest = hashlib.sha256(args.out.read_bytes()).hexdigest()
    print(f"onnxruntime {ort.__version__}")
    print(f"{args.out}  {args.out.stat().st_size} bytes  sha256 {digest}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
