#!/usr/bin/env python3
"""Prepare local annotations for human review; never infer ground-truth labels."""
import argparse
import json
from pathlib import Path
import wave


def review_template(paths, output_dir):
    import os
    rows = []
    for index, path in enumerate(paths):
        path = Path(path).resolve()
        with wave.open(str(path), 'rb') as source:
            if source.getsampwidth() != 2 or source.getcomptype() != 'NONE':
                raise ValueError('review input must be PCM16 WAV')
            duration = source.getnframes() / source.getframerate()
            if duration < 2:
                raise ValueError('review recordings must contain at least two seconds')
        rows.append(dict(path=os.path.relpath(path, output_dir), duration=duration,
                         reviewed=False, cry_intervals=[], split='validation',
                         recording_group=f'recording-{index + 1}',
                         subject_group='nursery-1', session_group='replace-with-session-id'))
    if not rows:
        raise ValueError('provide local WAV recordings')
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('recordings', nargs='+', type=Path)
    parser.add_argument('--output', required=True, type=Path)
    args = parser.parse_args()
    if args.output.exists():
        parser.error('refusing to overwrite existing review annotations')
    rows = review_template(args.recordings, args.output.resolve().parent)
    with args.output.open('x') as output:
        json.dump(rows, output, indent=2)
        output.write('\n')
    print('Listen to every recording. Mark all crying intervals, including missed cries, assign sessions and validation/test splits, then set reviewed=true. No labels were inferred.')


if __name__ == '__main__':
    main()
