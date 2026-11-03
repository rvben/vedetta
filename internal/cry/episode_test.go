package cry

import (
	"testing"
	"time"
)

func TestEpisodeConfirmationAndHysteresis(t *testing.T) {
	s := episodeState{threshold: .2}
	now := time.Unix(100, 0)
	step := func(score float32) (bool, bool) {
		now = now.Add(480 * time.Millisecond)
		return s.update(now, 1, score)
	}
	if begin, _ := step(.3); begin {
		t.Fatal("single hit alerted")
	}
	if begin, _ := step(.3); !begin {
		t.Fatal("confirmed onset missing")
	}
	for range 10 {
		if begin, end := step(.4); begin || end {
			t.Fatal("ongoing cry duplicated or ended")
		}
	}
	if _, end := step(.17); end {
		t.Fatal("hysteresis did not hold")
	}
	for range 2 {
		if _, end := step(.01); end {
			t.Fatal("release too early")
		}
	}
	if _, end := step(.01); !end {
		t.Fatal("release missing")
	}
	step(.4)
	if begin, _ := step(.4); !begin {
		t.Fatal("notification cooldown must not hide a new crying episode")
	}
}

func TestEpisodeContinuityRequired(t *testing.T) {
	for _, change := range []string{"epoch", "gap", "backwards"} {
		t.Run(change, func(t *testing.T) {
			s := episodeState{threshold: .2}
			now := time.Unix(100, 0)
			s.update(now, 1, .9)
			now = now.Add(480 * time.Millisecond)
			s.update(now, 1, .9)
			epoch := uint64(1)
			switch change {
			case "epoch":
				epoch = 2
				now = now.Add(480 * time.Millisecond)
			case "gap":
				now = now.Add(2 * time.Second)
			case "backwards":
				now = now.Add(-time.Second)
			}
			begin, end := s.update(now, epoch, .9)
			if begin || !end {
				t.Fatal("discontinuity did not close/reset episode")
			}
		})
	}
}
