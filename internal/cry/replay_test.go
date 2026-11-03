package cry

import (
	"errors"
	"testing"
)

func TestLocalReplayUsesLiveWindowTimingAndNeverPadsShortAudio(t *testing.T) {
	calls := 0
	model := scoreFunc(func(pcm []int16) (float32, error) {
		calls++
		if len(pcm) != 16000 {
			t.Fatal("context differs from live monitor")
		}
		if calls == 1 && pcm[0] != 7 {
			t.Fatal("first samples lost")
		}
		if calls == 2 && pcm[0] != 9 {
			t.Fatal("overlap differs from live monitor")
		}
		return .8, nil
	})
	pcm := make([]int16, 19840)
	pcm[0] = 7
	pcm[3840] = 9
	windows, err := ReplayPCM(model, pcm)
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 2 || windows[0].End != 2 || windows[1].End != 2.48 {
		t.Fatalf("backdated or extra decisions: %+v", windows)
	}
	if _, err := ReplayPCM(model, make([]int16, 15999)); err == nil {
		t.Fatal("invented context for short recording")
	}
	failure := errors.New("inference failed")
	if _, err := ReplayPCM(scoreFunc(func([]int16) (float32, error) { return 0, failure }), pcm); !errors.Is(err, failure) {
		t.Fatal("inference error hidden")
	}
}
