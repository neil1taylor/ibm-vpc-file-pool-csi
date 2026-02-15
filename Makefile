BINARY_NAME := vpc-file-pool-csi
IMAGE_NAME := icr.io/ibm-vpc-file-pool-csi/driver
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test test-coverage lint generate docker-build install-crds deploy helm-install run-local

build:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY_NAME) ./cmd/

test:
	go test ./... -v -race -count=1

test-coverage:
	go test ./... -v -race -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

generate:
	controller-gen object paths="./api/..."
	controller-gen crd paths="./api/..." output:crd:dir=config/crd

docker-build:
	docker build -t $(IMAGE_NAME):$(VERSION) .

install-crds:
	kubectl apply -f config/crd/

deploy: install-crds
	kubectl apply -f config/rbac/
	kubectl apply -f config/deploy/

helm-install:
	helm upgrade --install ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi/ \
		--namespace kube-system

run-local:
	go run ./cmd/ --mode=controller --endpoint=unix:///tmp/csi.sock --v=4
