import unittest
from metrics import episodes, summarize, summarize_clips, validate_manifest


class MetricsTest(unittest.TestCase):
    def row(self, **changes):
        row = dict(reviewed=True, duration=10, cry_intervals=[[2, 6]], split="test", recording_group="r1", subject_group="s1", session_group="d1")
        row.update(changes)
        return row

    def test_decision_is_not_backdated(self):
        self.assertEqual(episodes([(2, .8), (2.48, .8), (2.96, .1), (3.44, .1), (3.92, .1)], .5), [(2.48, 3.92)])

    def test_packet_gap_resets_consecutive_hits(self):
        self.assertEqual(episodes([(2, .8), (4, .8)], .5), [])
        self.assertEqual(episodes([(2, .8), (2.48, .8), (4, .8), (4.48, .8)], .5), [(2.48, 2.48), (4.48, 4.48)])

    def test_long_false_episode_cannot_claim_future_cry(self):
        result = summarize([self.row()], [[(1, .8), (1.48, .8), (1.96, .8), (2.44, .8)]], .5)
        self.assertEqual(result["missed_episodes"], 1)
        self.assertEqual(result["false_episodes"], 1)

    def test_episode_metrics_count_decisions(self):
        result = summarize([self.row()], [[(2, .8), (2.48, .8), (2.96, .2)]], .5)
        self.assertEqual(result["missed_episodes"], 0)
        self.assertAlmostEqual(result["onset_to_decision_p50_s"], .48)
        self.assertAlmostEqual(result["reviewed_negative_hours"], 6/3600)

    def test_no_negatives_is_not_zero_false_alert_rate(self):
        result = summarize([self.row(cry_intervals=[[0, 10]])], [[]], .5)
        self.assertIsNone(result["false_alerts_per_8h"])

    def test_grouped_split_leakage_and_invalid_annotations(self):
        validate_manifest([self.row()])
        for field in ("recording_group", "subject_group", "session_group"):
            other = self.row(split="train", recording_group="r2", subject_group="s2", session_group="d2")
            other[field] = self.row()[field]
            with self.assertRaises(ValueError): validate_manifest([self.row(), other])
        for changes in (dict(reviewed=False), dict(cry_intervals=[[5, 4]]), dict(cry_intervals=[[0, 11]]), dict(cry_intervals=[[1, 3], [2, 4]]), dict(duration=float("nan"))):
            with self.assertRaises(ValueError): validate_manifest([self.row(**changes)])

    def test_invalid_output_fails_closed(self):
        for values in ([(2, float("nan"))], [(2, 1.1)], [(2, .8), (1, .8)]):
            with self.assertRaises(ValueError): episodes(values, .5)

    def test_personalized_evaluation_still_requires_independent_sessions(self):
        other = self.row(split="validation", recording_group="r2", session_group="d2")
        validate_manifest([self.row(), other], require_subject_holdout=False)
        with self.assertRaises(ValueError): validate_manifest([self.row(), other])
        other["session_group"] = "d1"
        with self.assertRaises(ValueError): validate_manifest([self.row(), other], require_subject_holdout=False)

    def test_smoke_clip_crossing_is_separate_from_episode_confirmation(self):
        rows = [dict(cry=True), dict(cry=False)]
        scores = [[(5, .8)], [(5, .1)]]
        result = summarize_clips(rows, scores, .5)
        self.assertEqual(result["missed_cry_clips"], 0)
        self.assertEqual(result["false_positive_clips"], 0)
        self.assertEqual(episodes(scores[0], .5), [])
        with self.assertRaises(ValueError): summarize_clips(rows, [[(5, float("nan"))], []], .5)


if __name__ == "__main__": unittest.main()
