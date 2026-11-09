import threading
import unittest
from verifier import REQUIRED, AdmissionGate, verdict


class VerifierTest(unittest.TestCase):
    def test_all_absent_signals_forbid_cleanup(self):
        good = dict.fromkeys(REQUIRED, True)
        self.assertTrue(verdict(good)["synthetic_cleanup_allowed"])
        self.assertFalse(verdict(good)["production_cleanup_allowed"])
        for field in REQUIRED:
            for value in [None, False, "true"]:
                with self.subTest(field=field, value=value):
                    result = verdict(good | {field: value})
                    self.assertFalse(result["synthetic_cleanup_allowed"])
                    self.assertEqual(result["task_cgroup"], "NOT PROVEN")

    def test_close_drains_inflight_and_forbids_later_launches(self):
        gate = AdmissionGate()
        entered, release, closed = threading.Event(), threading.Event(), threading.Event()
        def launch():
            entered.set()
            self.assertTrue(release.wait(2))
        writer = threading.Thread(target=lambda: gate.admit(launch))
        writer.start()
        self.assertTrue(entered.wait(2))
        closer = threading.Thread(target=lambda: (gate.close(), closed.set()))
        closer.start()
        self.assertFalse(closed.wait(.02))
        release.set()
        writer.join(2); closer.join(2)
        self.assertTrue(closed.is_set())
        self.assertFalse(gate.admit(lambda: self.fail("closed gate launched work")))


if __name__ == "__main__":
    unittest.main()
