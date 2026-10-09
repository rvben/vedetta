#!/usr/bin/env python3
"""Download public benchmark assets into a user-selected local directory."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import csv
import hashlib
import json
from pathlib import Path
import tarfile
import urllib.request

EFFICIENTAT_REV = "a425fdce92572e602a1d5634799bd9f1f2efa806"
ESC_REV = "33c8ce9eb2cf0b1c2f8bcf322eb349b6be34dbb6"
ASSETS = {
    "yamnet.tar.gz": ("https://tfhub.dev/google/yamnet/1?tf-hub-format=compressed", "b80da2a1a56926fb0767205051a200dd7b3beaf3ea1ea126c42a53943996e5e0"),
    "mn10.pt": ("https://github.com/fschmid56/EfficientAT/releases/download/v0.0.1/mn10_as_mAP_471.pt", "0bd7dc2443af498c289a2e739f02ebb515d6aa3fd3ab9db539c86123ae368a4e"),
    "efficientat.tar.gz": (f"https://codeload.github.com/fschmid56/EfficientAT/tar.gz/{EFFICIENTAT_REV}", "0c1231cc451566e7d4205eb470e2d6b37aa3ac525285d5bdfaec297417813d10"),
}


def download(url, target, sha=None):
    if not target.exists():
        request = urllib.request.Request(url, headers={"User-Agent": "VedettaAudioBenchmark/1.0"})
        with urllib.request.urlopen(request, timeout=60) as response:
            data = response.read()
        temporary = target.with_suffix(target.suffix+".part")
        temporary.write_bytes(data)
        temporary.replace(target)
    actual = hashlib.sha256(target.read_bytes()).hexdigest()
    if sha and actual != sha:
        raise ValueError(f"SHA-256 mismatch for {target.name}; remove the file before retrying")
    return actual


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("directory", type=Path)
    p.add_argument("--models", action="store_true")
    p.add_argument("--smoke", action="store_true", help="ESC-50 fold 5; CC BY-NC 3.0, for noncommercial local evaluation only")
    args = p.parse_args()
    args.directory.mkdir(parents=True, exist_ok=True)
    if args.models:
        for name, (url, sha) in ASSETS.items():
            download(url, args.directory/name, sha)
        for archive, dest in (("yamnet.tar.gz", "yamnet"), ("efficientat.tar.gz", "source")):
            with tarfile.open(args.directory/archive) as tar:
                tar.extractall(args.directory/dest, filter="data")
        source = args.directory/"source"/f"EfficientAT-{EFFICIENTAT_REV}"
        print(f"EfficientAT source: {source}")
    if args.smoke:
        base = f"https://raw.githubusercontent.com/karolpiczak/ESC-50/{ESC_REV}"
        download(base+"/LICENSE", args.directory/"ESC-50-LICENSE")
        download(base+"/meta/esc50.csv", args.directory/"esc50.csv")
        with (args.directory/"esc50.csv").open(newline="") as f:
            rows = [r for r in csv.DictReader(f) if r["fold"] == "5" and r["category"] in {"crying_baby", "laughing", "coughing", "breathing", "snoring", "cat"}]
        audio = args.directory/"smoke"
        audio.mkdir(exist_ok=True)
        def fetch(row):
            filename = row["filename"]
            sha = download(base+"/audio/"+filename, audio/filename)
            return {"path": "smoke/"+filename, "duration": 5, "cry": row["category"] == "crying_baby", "category": row["category"], "source_file": row["src_file"], "sha256": sha}
        with ThreadPoolExecutor(max_workers=4) as pool:
            manifest = list(pool.map(fetch, rows))
        (args.directory/"smoke.json").write_text(json.dumps(manifest, indent=2)+"\n")
        print(f"Downloaded {len(manifest)} weak-label smoke clips. Attribution: {args.directory/'ESC-50-LICENSE'}")


if __name__ == "__main__": main()
