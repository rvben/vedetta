package cry

import "time"

// episodeState confirms onset twice and release three times. Every decision
// uses fresh consecutive windows from one continuous stream. Notification
// cooldown is independent: it must never hide crying or discard its evidence.
type episodeState struct {
	threshold float32
	high, low int
	active    bool
	last      time.Time
	epoch     uint64
}

func (s *episodeState) reset(_ time.Time) bool {
	ended := s.active
	s.active = false
	s.high = 0
	s.low = 0
	s.last = time.Time{}
	s.epoch = 0
	return ended
}

func (s *episodeState) update(now time.Time, epoch uint64, score float32) (start, end bool) {
	if !s.last.IsZero() && (epoch != s.epoch || !now.After(s.last) || now.Sub(s.last) > 720*time.Millisecond) {
		end = s.reset(s.last)
	}
	s.last = now
	s.epoch = epoch
	if score >= s.threshold {
		s.high++
		s.low = 0
		if !s.active && s.high >= 2 {
			s.active = true
			start = true
		}
	} else if score < s.threshold*.7 {
		s.high = 0
		s.low++
		if s.active && s.low >= 3 {
			s.active = false
			end = true
			s.low = 0
		}
	} else {
		s.high = 0
		s.low = 0
	}
	return start, end
}
