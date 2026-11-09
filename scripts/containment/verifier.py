"""Task-cgroup evidence only. Host engine access is deliberately a separate guard."""
REQUIRED = ("admission_closed", "admissions_drained", "stop_succeeded", "container_exited",
            "kernel_populated", "kernel_empty", "observer_succeeded", "no_observation_error", "operations_succeeded")


def verdict(evidence):
    missing = [name for name in REQUIRED if evidence.get(name) is not True]
    return {"task_cgroup": "PROVEN" if not missing else "NOT PROVEN",
            "synthetic_cleanup_allowed": not missing,
            "missing": missing,
            "host_writer_exclusion": "OPEN", "production_cleanup_allowed": False}


class AdmissionGate:
    """The lock spans launch acknowledgement; close drains prior admissions."""
    def __init__(self):
        import threading
        self.lock = threading.Lock()
        self.closed = False

    def admit(self, launch):
        with self.lock:
            if self.closed:
                return False
            launch()
            return True

    def close(self):
        with self.lock:
            self.closed = True
