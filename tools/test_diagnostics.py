"""Tests for read-only diagnostics helpers."""
import unittest

from station import parse_proc_net_tcp


class ProcNetTcpTests(unittest.TestCase):
    def test_listening_ports_only(self):
        sample = """  sl  local_address rem_address   st
   0: 0100007F:2692 00000000:0000 0A 00000000:00000000 00:00:00 00000000 0 0 1
   1: 0100007F:2693 00000000:0000 01 00000000:00000000 00:00:00 00000000 0 0 2
"""
        self.assertEqual(parse_proc_net_tcp(sample), {0x2692})

    def test_malformed_rows_are_ignored(self):
        self.assertEqual(parse_proc_net_tcp("header\ninvalid\n 0: bad"), set())


if __name__ == "__main__":
    unittest.main()
