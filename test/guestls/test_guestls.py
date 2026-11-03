"""Tests the argument handling of guestls (pkg/guestfs/guestls.py) without libguestfs."""
import importlib.machinery
import importlib.util
import os
import sys
import types
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
PATH = os.path.join(HERE, "..", "..", "pkg", "guestfs", "guestls.py")

sys.modules["guestfs"] = types.ModuleType("guestfs")
loader = importlib.machinery.SourceFileLoader("guestls", PATH)
spec = importlib.util.spec_from_loader("guestls", loader)
guestls = importlib.util.module_from_spec(spec)
loader.exec_module(guestls)


class FakeGuest:
    def __init__(self):
        self.calls = []

    def add_drive_opts(self, *args, **kwargs):
        self.calls.append((args, kwargs))

    def set_program(self, name):
        self.program = name


class AddDriveTest(unittest.TestCase):
    def test_a_file_is_added_by_name(self):
        g = FakeGuest()
        guestls.add_drive(g, "/tmp/disk.img")
        self.assertEqual(g.calls, [(("/tmp/disk.img",), {"format": "raw", "readonly": 1})])

    def test_an_nbd_address_is_added_as_a_server(self):
        g = FakeGuest()
        guestls.add_drive(g, "nbd+unix:///?socket=/tmp/disk.sock")
        self.assertEqual(g.calls, [(("",), {"format": "raw", "readonly": 1, "protocol": "nbd", "server": ["unix:/tmp/disk.sock"]})])

    def test_an_nbd_address_without_a_socket_is_refused(self):
        with self.assertRaises(RuntimeError):
            guestls.add_drive(FakeGuest(), "nbd+unix:///")

    def test_a_relative_socket_is_refused(self):
        with self.assertRaises(RuntimeError):
            guestls.add_drive(FakeGuest(), "nbd+unix:///?socket=disk.sock")


class StartTest(unittest.TestCase):
    def test_the_program_is_named_like_a_libguestfs_tool(self):
        g = FakeGuest()
        guestls.start(g, "/tmp/disk.img")
        self.assertTrue(g.program.startswith("virt-"), "NTFS is readable only for a program named virt-*")
        self.assertEqual(len(g.calls), 1)


class ParseTest(unittest.TestCase):
    def test_the_usual_command_line(self):
        self.assertEqual(
            guestls.parse(["--format=raw", "-a", "/d.img", "-m", "/dev/sda1", "--", "/etc"]),
            ("/d.img", "/dev/sda1", "/etc"),
        )

    def test_a_relative_folder_is_refused(self):
        with self.assertRaises(SystemExit):
            guestls.parse(["-a", "/d.img", "-m", "/dev/sda1", "--", "etc"])

    def test_a_missing_disk_is_refused(self):
        with self.assertRaises(SystemExit):
            guestls.parse(["-m", "/dev/sda1", "--", "/"])


if __name__ == "__main__":
    unittest.main()
