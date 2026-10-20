#!/bin/sh
# Regenerates the qcow2 test images. Needs qemu-img and qemu-io.
# The tests hard-code the extents these commands write.
set -e
cd "$(dirname "$0")"

# v3, 4 KiB clusters, virtual size 1 MiB + 512 bytes (last cluster is partial).
# Writes are out of order, so host clusters are not in virtual order.
qemu-img create -q -f qcow2 -o compat=1.1,cluster_size=4096 v3.qcow2 1049088
qemu-io -f qcow2 \
  -c 'write -P 0xdd 262144 4096' \
  -c 'write -P 0xaa 0 4096' \
  -c 'write -P 0xbb 8192 8192' \
  -c 'write -z 65536 8192' \
  -c 'write -P 0xcc 1048576 512' \
  v3.qcow2 >/dev/null

# v2 (compat 0.10) has no zero flag.
qemu-img create -q -f qcow2 -o compat=0.10,cluster_size=4096 v2.qcow2 1048576
qemu-io -f qcow2 -c 'write -P 0xaa 0 4096' -c 'write -P 0xbb 12288 4096' v2.qcow2 >/dev/null

# Default 64 KiB clusters, as QEMU makes staging files.
qemu-img create -q -f qcow2 default.qcow2 4194304
qemu-io -f qcow2 -c 'write -P 0xee 131072 65536' default.qcow2 >/dev/null

# Compressed clusters cannot be read and must be refused.
qemu-img convert -q -c -f qcow2 -O qcow2 -o cluster_size=4096 v3.qcow2 compressed.qcow2

# An image with a backing file must be refused.
qemu-img create -q -f qcow2 -b v3.qcow2 -F qcow2 backing.qcow2 1049088
