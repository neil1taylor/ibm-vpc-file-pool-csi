# Build stage
FROM golang:1.25 AS builder

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /vpc-file-pool-csi ./cmd/

# Runtime stage
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

# Install NFS client utilities (needed by node agent for mounting)
RUN microdnf install -y nfs-utils && microdnf clean all

COPY --from=builder /vpc-file-pool-csi /usr/local/bin/vpc-file-pool-csi

ENTRYPOINT ["/usr/local/bin/vpc-file-pool-csi"]
