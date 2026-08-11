// Package reid holds the pure embedding-matching and centroid-maintenance
// policy used by face recognition and object re-identification. It contains no
// I/O: callers fetch candidates and persist results, while the similarity
// decisions and centroid math live here so they can be tested in isolation.
package reid

import (
	"math"

	"github.com/rvben/vedetta/internal/detect"
)

const (
	// FaceClusterThreshold is the minimum similarity for grouping two
	// unmatched faces into a new unnamed person.
	FaceClusterThreshold = 0.62
	// PersonCentroidUpdateWeight controls how much a newly matched face
	// contributes when updating a person's recognition centroid.
	PersonCentroidUpdateWeight float32 = 0.3
)

// Candidate is a stored centroid an embedding can be matched against.
type Candidate struct {
	ID       int64
	Centroid []float32
	Ignore   bool
	// Threshold is a per-candidate cosine-similarity minimum. When zero the
	// caller's default threshold applies. A known object can set a stricter
	// value to avoid being matched to look-alikes (e.g. one parked car vs.
	// another of the same make/color).
	Threshold float64
}

// BestMatch returns the ID and cosine similarity of the most-similar candidate
// that clears its effective threshold (its own Threshold when set, otherwise
// the default threshold). Candidates that are ignored or have an empty centroid
// are skipped. When nothing qualifies it returns (0, 0).
//
// Selecting the best among those that pass (rather than gating the global best)
// is what makes per-candidate thresholds correct: a high-similarity candidate
// with a strict threshold can be rejected while a lower-similarity one with a
// lenient threshold is chosen.
func BestMatch(embedding []float32, candidates []Candidate, threshold float64) (int64, float64) {
	var bestID int64
	var bestSim float64
	for _, c := range candidates {
		if c.Ignore || len(c.Centroid) == 0 {
			continue
		}
		effThreshold := threshold
		if c.Threshold > 0 {
			effThreshold = c.Threshold
		}
		sim := detect.CosineSimilarity(embedding, c.Centroid)
		if sim >= effThreshold && sim > bestSim {
			bestSim = sim
			bestID = c.ID
		}
	}
	return bestID, bestSim
}

// BlendCentroid merges newEmbedding into old using an exponential running
// average (weight alpha on the new sample) and L2-normalizes the result. When
// old is empty or a different length than newEmbedding, newEmbedding is
// returned unchanged so the caller stores the fresh sample directly.
func BlendCentroid(old, newEmbedding []float32, alpha float32) []float32 {
	if len(old) == 0 || len(old) != len(newEmbedding) {
		return newEmbedding
	}
	merged := make([]float32, len(old))
	var norm float64
	for i := range merged {
		merged[i] = (1-alpha)*old[i] + alpha*newEmbedding[i]
		norm += float64(merged[i]) * float64(merged[i])
	}
	return normalize(merged, norm)
}

// AverageNormalized returns the L2-normalized midpoint of a and b. When the
// lengths differ it returns a unchanged.
func AverageNormalized(a, b []float32) []float32 {
	if len(a) != len(b) {
		return a
	}
	out := make([]float32, len(a))
	var norm float64
	for i := range out {
		out[i] = (a[i] + b[i]) / 2
		norm += float64(out[i]) * float64(out[i])
	}
	return normalize(out, norm)
}

// normalize scales v to unit length in place when its squared norm is non-zero,
// then returns it. A near-zero norm is left unscaled to avoid dividing by zero.
func normalize(v []float32, norm float64) []float32 {
	if norm > 1e-10 {
		invNorm := float32(1.0 / math.Sqrt(norm))
		for i := range v {
			v[i] *= invNorm
		}
	}
	return v
}
