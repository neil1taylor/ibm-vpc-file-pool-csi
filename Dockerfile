# Build stage
FROM docker.io/library/golang:1.26 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is stamped into the binary and logged at startup; `make docker-build` sets it.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /vpc-file-pool-csi ./cmd/

# The Kopia command-line program, used only by the COS export Jobs (never linked
# into the driver). The SHA-256 is from checksums.txt of the official release.
ARG KOPIA_VERSION=0.23.1
ARG KOPIA_SHA256=416d0f84a3dbb321a8b2d8f0997b1a0a6e915babe79ee76fa6e4d2bd1e1c5178
RUN curl -fsSL -o /tmp/kopia.tar.gz "https://github.com/kopia/kopia/releases/download/v${KOPIA_VERSION}/kopia-${KOPIA_VERSION}-linux-x64.tar.gz" && \
    echo "${KOPIA_SHA256}  /tmp/kopia.tar.gz" | sha256sum -c - && \
    tar -xzf /tmp/kopia.tar.gz -C /tmp "kopia-${KOPIA_VERSION}-linux-x64/kopia" && \
    install -m 0755 "/tmp/kopia-${KOPIA_VERSION}-linux-x64/kopia" /kopia

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

# Install nsenter for host-namespace mount execution and rsync for incremental replication.
RUN microdnf install -y util-linux-core rsync && microdnf clean all

COPY --from=builder /vpc-file-pool-csi /usr/local/bin/vpc-file-pool-csi
COPY --from=builder /kopia /usr/local/bin/kopia

# Node agent runs privileged and needs mount/umount for NFS.
# nfs-utils is not available in UBI repos, so we use nsenter to call
# the host's mount binary (which has access to mount.nfs4) for NFS mounts.
# Bind mounts and unmounts use the container's own mount/umount binary
# from util-linux-core, since they work without the NFS helper and
# need the container's filesystem view.
RUN printf '#!/bin/sh\ncase "$*" in\n  *"-t nfs"*) exec nsenter --mount=/proc/1/ns/mnt --root=/proc/1/root -- /bin/mount "$@" ;;\n  *) exec /usr/bin/mount "$@" ;;\nesac\n' > /usr/local/bin/mount && \
    chmod +x /usr/local/bin/mount

ENTRYPOINT ["/usr/local/bin/vpc-file-pool-csi"]
