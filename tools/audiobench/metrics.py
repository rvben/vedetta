"""Dependency-free episode metrics for fully reviewed replay recordings."""
import math


def validate_manifest(rows, require_subject_holdout=True):
    seen = {}
    for row in rows:
        if not row.get("reviewed"):
            raise ValueError("every recording must be fully reviewed, including negatives")
        duration = float(row["duration"])
        if not math.isfinite(duration) or duration <= 0:
            raise ValueError("invalid duration")
        last = 0
        for start, end in row["cry_intervals"]:
            if not (math.isfinite(start) and math.isfinite(end) and last <= start < end <= duration):
                raise ValueError("cry intervals must be ordered, disjoint and inside the recording")
            last = end
        if row.get("split") not in ("train", "validation", "test"):
            raise ValueError("split must be train, validation or test")
        # Prevent leakage from overlapping chunks, infants and sessions. These
        # identifiers are local pseudonyms; the benchmark never transmits them.
        for field in ("recording_group", "subject_group", "session_group"):
            value = row.get(field)
            if not value:
                raise ValueError(f"missing {field}")
            if field == "subject_group" and not require_subject_holdout:
                continue
            key = (field, value)
            if key in seen and seen[key] != row["split"]:
                raise ValueError(f"{field} occurs in multiple splits")
            seen[key] = row["split"]


def episodes(windows, threshold, hits=2, release=3, hop=0.48):
    """Onset after consecutive hits; close after consecutive low scores.

    Windows carry end timestamps: no future samples may contribute. A gap
    resets the detector and ends any open episode; missing audio is not silence.
    """
    result = []
    high = low = 0
    active = None
    prev = None
    for end, score in windows:
        if not math.isfinite(end) or not math.isfinite(score) or not 0 <= score <= 1:
            raise ValueError("non-finite or invalid model output")
        if prev is not None:
            if end <= prev:
                raise ValueError("window timestamps must increase")
            if end - prev > hop * 1.5:
                if active is not None:
                    result.append((active, prev))
                active, high, low = None, 0, 0
        prev = end
        if score >= threshold:
            high += 1
            low = 0
            if active is None and high >= hits:
                active = end  # decision time, not retrospectively backdated onset
        else:
            high = 0
            low += 1
            if active is not None and low >= release:
                result.append((active, end))
                active, low = None, 0
    if active is not None:
        result.append((active, prev))
    return result


def summarize(rows, scores, threshold, hop=0.48):
    missed = total = false = 0
    negative_seconds = 0
    latencies = []
    for row, windows in zip(rows, scores, strict=True):
        truth = row["cry_intervals"]
        detected = episodes(windows, threshold, hop=hop)
        total += len(truth)
        negative_seconds += row["duration"] - sum(b-a for a, b in truth)
        for start, end in truth:
            # A decision before the labeled onset cannot count as detection of
            # that cry merely because a long false episode overlaps it.
            onsets = [a for a, _ in detected if start <= a < end]
            if onsets:
                latencies.append(min(onsets)-start)
            else:
                missed += 1
        false += sum(not any(start <= a < end for start, end in truth) for a, _ in detected)
    latencies.sort()
    return {
        "threshold": threshold,
        "cry_episodes": total,
        "missed_episodes": missed,
        "false_episodes": false,
        "reviewed_negative_hours": negative_seconds / 3600,
        "false_alerts_per_8h": false * 28800 / negative_seconds if negative_seconds else None,
        "onset_to_decision_p50_s": latencies[len(latencies)//2] if latencies else None,
    }


def summarize_clips(rows, scores, threshold):
    """Weak-label smoke classification; a crossing is not a live alert."""
    positives = negatives = missed = false = 0
    for row, windows in zip(rows, scores, strict=True):
        values = [score for _, score in windows]
        if not values or any(not math.isfinite(v) or not 0 <= v <= 1 for v in values):
            raise ValueError("empty or invalid clip scores")
        detected = any(v >= threshold for v in values)
        if row["cry"]:
            positives += 1
            missed += not detected
        else:
            negatives += 1
            false += detected
    return dict(threshold=threshold, cry_clips=positives, missed_cry_clips=missed,
                negative_clips=negatives, false_positive_clips=false)
