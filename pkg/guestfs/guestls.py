#!/usr/bin/python3
"""Lists one folder of a file system inside a disk image.

    guestls --format=raw -a DISK -m DEVICE[:MOUNTPOINT[:OPTIONS]] -- FOLDER

DISK is a file or an address like nbd+unix:///?socket=PATH.

It writes what `virt-ls -lR --csv --time-t --uids -- FOLDER` writes, but only
for the folder and the names directly in it: type letter, permissions in octal,
size, user number, group number, access time, content-change time, status time,
the full path, and for a link its text. virt-ls prints times only together with
-R, which reads the whole tree below the folder; this reads one level.

A name that is not valid UTF-8 cannot be given to the libguestfs Python
binding (it crashes), so such a name is listed with type "?" and no details.
"""
import csv
import os
import stat
import sys
import tempfile
import urllib.parse

import guestfs

BATCH = 1000
PROGRAM = "virt-guestls"


def usage():
    sys.stderr.write("usage: guestls --format=raw -a DISK -m MOUNT -- FOLDER\n")
    sys.exit(2)


def parse(argv):
    drive = mount = folder = None
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--":
            if i + 2 != len(argv):
                usage()
            folder = argv[i + 1]
            break
        if a == "--format=raw":
            i += 1
        elif a == "-a" and i + 1 < len(argv):
            drive, i = argv[i + 1], i + 2
        elif a == "-m" and i + 1 < len(argv):
            mount, i = argv[i + 1], i + 2
        else:
            usage()
    if not (drive and mount and folder and folder.startswith("/")):
        usage()
    return drive, mount, folder


def add_drive(g, drive):
    """Adds the disk. The tools take a URI such as nbd+unix:///?socket=PATH, which the
    API does not; the reader serves its disk over such a socket."""
    if drive.startswith("nbd+unix:"):
        socket = urllib.parse.parse_qs(urllib.parse.urlparse(drive).query).get("socket", [""])[0]
        if not socket.startswith("/"):
            raise RuntimeError("the nbd address has no socket path")
        g.add_drive_opts("", format="raw", readonly=1, protocol="nbd", server=["unix:" + socket])
    else:
        g.add_drive_opts(drive, format="raw", readonly=1)


def start(g, drive):
    """Names the program as the libguestfs tools do: the NTFS support of
    libguestfs-winsupport works only for a program whose name starts with virt-."""
    g.set_program(PROGRAM)
    add_drive(g, drive)


def row(typ, mode, size, uid, gid, atime, mtime, ctime, path, link=None):
    r = [typ, "%04o" % mode, str(size), "%4d" % uid, "%4d" % gid, str(atime), str(mtime), str(ctime), path]
    if link is not None:
        r.append(link)
    return r


def from_stat(path, st, g):
    mode = st["st_mode"]
    typ = "d" if stat.S_ISDIR(mode) else "-" if stat.S_ISREG(mode) else "l" if stat.S_ISLNK(mode) else "?"
    link = None
    if typ == "l":
        try:
            link = g.readlink(path)
        except Exception:
            link = None
    return row(typ, stat.S_IMODE(mode), st["st_size"], st["st_uid"], st["st_gid"],
               st["st_atime_sec"], st["st_mtime_sec"], st["st_ctime_sec"], path, link)


def main():
    drive, mount, folder = parse(sys.argv[1:])
    dev, _, rest = mount.partition(":")
    mountpoint, _, opts = rest.partition(":")
    g = guestfs.GuestFS(python_return_dict=True)
    start(g, drive)
    g.launch()
    g.mount_options(opts, dev, mountpoint or "/")

    out = csv.writer(open(sys.stdout.fileno(), "w", encoding="utf-8", errors="surrogateescape",
                          newline="", closefd=False), lineterminator="\n")
    base = folder.rstrip("/")
    top = g.statns(folder)  # follows a link, as ls does
    if not stat.S_ISDIR(top["st_mode"]):
        raise RuntimeError("%s: Not a directory" % folder)
    out.writerow(from_stat(folder, top, g))

    with tempfile.NamedTemporaryFile() as tmp:
        g.ls0(folder, tmp.name)
        raw = sorted(n for n in open(tmp.name, "rb").read().split(b"\0") if n)
    names = [os.fsdecode(n) for n in raw]
    good = []
    for n, r in zip(names, raw):
        try:
            r.decode("utf-8")
            good.append(n)
        except UnicodeDecodeError:
            out.writerow(row("?", 0, 0, 0, 0, 0, 0, 0, base + "/" + n))
    for i in range(0, len(good), BATCH):
        chunk = good[i:i + BATCH]
        for n, st in zip(chunk, g.lstatnslist(folder, chunk)):
            path = base + "/" + n
            if st["st_ino"] == -1:
                out.writerow(row("?", 0, 0, 0, 0, 0, 0, 0, path))
            else:
                out.writerow(from_stat(path, st, g))
    g.close()


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # libguestfs errors carry the reason in their text
        sys.stderr.write("guestls: %s\n" % e)
        sys.exit(1)
