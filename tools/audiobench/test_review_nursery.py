import tempfile
from pathlib import Path
import unittest
import wave

from metrics import validate_manifest
from review_nursery import review_template


class NurseryReviewTest(unittest.TestCase):
    def test_template_never_claims_reviewed_accuracy(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'recording.wav'
            with wave.open(str(path), 'wb') as output:
                output.setnchannels(1)
                output.setsampwidth(2)
                output.setframerate(8000)
                output.writeframes(bytes(32000))
            rows = review_template([path], directory)
            self.assertEqual(rows[0]['duration'], 2)
            self.assertFalse(rows[0]['reviewed'])
            self.assertEqual(rows[0]['cry_intervals'], [])
            with self.assertRaisesRegex(ValueError, 'fully reviewed'):
                validate_manifest(rows)
            rows[0]['reviewed'] = True
            validate_manifest(rows)

    def test_empty_review_is_rejected(self):
        with self.assertRaises(ValueError):
            review_template([], '.')
