package cry

import (
	"errors"
	"testing"
)

func constantWindow(level int16) []int16 {
	pcm := make([]int16, windowSamples)
	for i := range pcm {
		pcm[i] = level
	}
	return pcm
}

func TestConstantLevelNeedsEverySampleEqual(t *testing.T) {
	for _, level := range []int16{0, 8, -8, 32256, -32768} {
		if got, ok := constantLevel(constantWindow(level)); !ok || got != level {
			t.Fatalf("constantLevel(all %d) = %d, %v", level, got, ok)
		}
	}
	for _, at := range []int{0, 1, windowSamples / 2, windowSamples - 1} {
		pcm := constantWindow(8)
		pcm[at] = 9
		if _, ok := constantLevel(pcm); ok {
			t.Fatalf("one sample off by one at %d leaves the window constant", at)
		}
	}
}

// A model without a session fails any inference, so a returned score proves
// the window never reached it.
func TestRememberedConstantWindowIsScoredWithoutInference(t *testing.T) {
	m := &Model{}
	m.remember(8, 0.125)
	for name, score := range map[string]func([]int16) (float32, error){"Score": m.Score, "TryScore": m.TryScore} {
		got, err := score(constantWindow(8))
		if err != nil || got != 0.125 {
			t.Fatalf("%s(remembered) = %v, %v, want 0.125", name, got, err)
		}
		if _, err := score(constantWindow(-8)); err == nil {
			t.Fatalf("%s answered a constant window at another level without inference", name)
		}
		// A cry that starts in the last sample of an otherwise silent window.
		onset := constantWindow(8)
		onset[windowSamples-1] = 9
		if _, err := score(onset); err == nil {
			t.Fatalf("%s answered a window holding sound without inference", name)
		}
		if _, err := score(make([]int16, windowSamples-1)); !errors.Is(err, errWindowLength) {
			t.Fatalf("%s(short window) = %v, want errWindowLength", name, err)
		}
	}
	if len(m.constant) != 1 {
		t.Fatalf("failed inference left %d remembered levels, want 1", len(m.constant))
	}
}

func TestRememberedConstantWindowIsAnsweredWhileTheModelIsBusy(t *testing.T) {
	m := &Model{}
	m.remember(8, 0.125)
	m.mu.Lock()
	defer m.mu.Unlock()
	if got, err := m.TryScore(constantWindow(8)); err != nil || got != 0.125 {
		t.Fatalf("TryScore(remembered) while busy = %v, %v", got, err)
	}
	for name, pcm := range map[string][]int16{"another level": constantWindow(-8), "sound": append(constantWindow(8)[1:], 4000)} {
		if _, err := m.TryScore(pcm); !errors.Is(err, ErrBusy) {
			t.Fatalf("TryScore(%s) while busy = %v, want ErrBusy", name, err)
		}
	}
}

func TestRememberedLevelsAreBounded(t *testing.T) {
	m := &Model{}
	for level := range int16(3 * constantLevels) {
		m.remember(level, float32(level))
	}
	if len(m.constant) != constantLevels {
		t.Fatalf("remembered %d levels, want %d", len(m.constant), constantLevels)
	}
	if got, _, _, ok := m.remembered(constantWindow(2)); !ok || got != 2 {
		t.Fatalf("an early level reads %v, %v", got, ok)
	}
}

// The remembered score stands in for the model, so it must be the model's own
// score for that exact window. It needs the model directory in
// VEDETTA_CRY_MODEL.
func TestRememberedScoreIsTheModelScore(t *testing.T) {
	m := loadEnvModel(t)
	if _, _, _, ok := m.remembered(constantWindow(0)); !ok {
		t.Fatal("loading the model did not remember the all-zero window")
	}
	for _, level := range []int16{0, 8, -8, 32256} {
		pcm := constantWindow(level)
		first, err := m.Score(pcm)
		if err != nil {
			t.Fatal(err)
		}
		again, err := m.Score(pcm)
		if err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		direct, err := m.infer(pcm)
		m.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if first != direct || again != direct {
			t.Errorf("level %d: Score gives %v then %v, the model %v", level, first, again, direct)
		}
	}
}
