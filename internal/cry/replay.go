package cry

import (
	"fmt"
	"math"
	"time"
)

type ReplayWindow struct {
	End              float64 `json:"end"`
	Score            float32 `json:"score"`
	InferenceSeconds float64 `json:"inference_seconds"`
}

// ReplayPCM scores local 8 kHz mono audio with the same two-second context
// and 480 ms hop as the live monitor. It never connects or sends alerts.
func ReplayPCM(model Scorer, pcm []int16) ([]ReplayWindow, error) {
	if len(pcm) < 16000 || len(pcm) > 8000*3600 {
		return nil, fmt.Errorf("replay requires between 2 seconds and 1 hour of mono 8 kHz audio")
	}
	var windows []ReplayWindow
	for start := 0; start+16000 <= len(pcm); start += 3840 {
		before := time.Now()
		score, err := model.TryScore(pcm[start : start+16000])
		if err != nil {
			return nil, err
		}
		if math.IsNaN(float64(score)) || math.IsInf(float64(score), 0) || score < 0 || score > 1 {
			return nil, fmt.Errorf("invalid replay score")
		}
		windows = append(windows, ReplayWindow{End: float64(start+16000) / 8000, Score: score, InferenceSeconds: time.Since(before).Seconds()})
	}
	return windows, nil
}
