# Build stage
FROM golang:1.25 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /vpc-file-pool-csi ./cmd/

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

# Install nsenter for host-namespace mount execution.
RUN microdnf install -y util-linux-core && microdnf clean all

COPY --from=builder /vpc-file-pool-csi /usr/local/bin/vpc-file-pool-csi

# Node agent runs privileged and needs mount/umount for NFS.
# nfs-utils is not available in UBI repos, so we use nsenter to call
# the host's mount binary (which has access to mount.nfs4) for NFS mounts.
# Bind mounts and unmounts use the container's own mount/umount binary
# from util-linux-core, since they work without the NFS helper and
# need the container's filesystem view.
RUN printf '#!/bin/sh\ncase "$*" in\n  *"-t nfs"*) exec nsenter --mount=/proc/1/ns/mnt --root=/proc/1/root -- /bin/mount "$@" ;;\n  *) exec /usr/bin/mount "$@" ;;\nesac\n' > /usr/local/bin/mount && \
    chmod +x /usr/local/bin/mount

ENTRYPOINT ["/usr/local/bin/vpc-file-pool-csi"]
