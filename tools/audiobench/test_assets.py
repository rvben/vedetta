import csv
from pathlib import Path
import tempfile
import unittest

from prepare import download
from replay import class_index


class AssetTest(unittest.TestCase):
    def test_lookup_uses_mid_not_model_specific_index(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"classes.csv"
            for index in (20, 23):
                with path.open("w", newline="") as f:
                    writer = csv.writer(f)
                    writer.writerow(["index", "mid", "display_name"])
                    writer.writerow([19, "/m/other", "Crying"])
                    writer.writerow([index, "/t/dd00002", "Baby cry, infant cry"])
                self.assertEqual(class_index(path), index)
            path.write_text("index,mid,display_name\n19,/m/other,Crying\n")
            with self.assertRaises(ValueError): class_index(path)

    def test_corrupt_cached_model_is_rejected_without_network(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"model.pt"
            path.write_bytes(b"corrupt")
            with self.assertRaisesRegex(ValueError, "SHA-256 mismatch"):
                download("https://example.invalid/model", path, "0"*64)


if __name__ == "__main__": unittest.main()
