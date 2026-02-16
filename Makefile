BINARY_NAME := vpc-file-pool-csi
IMAGE_NAME := icr.io/ibm-vpc-file-pool-csi/driver
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GOLANGCI_LINT_VERSION := v2.1.6
CONTROLLER_GEN_VERSION := v0.20.1
GOBIN := $(shell go env GOPATH)/bin
GOLANGCI_LINT := $(GOBIN)/golangci-lint
CONTROLLER_GEN := $(GOBIN)/controller-gen

.PHONY: build build-migrate test test-integration test-e2e test-coverage vet lint generate docker-build \
        install-crds deploy helm-install helm-lint helm-template run-local tools clean

build:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(BINARY_NAME) ./cmd/

build-migrate:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=$(VERSION)" -o bin/kubectl-migrate ./cmd/migrate/

test:
	go test ./... -v -race -count=1

test-integration:
	go test ./test/integration/ -v -race -count=1 -tags=integration

test-e2e:
	go test ./test/e2e/ -v -tags e2e -timeout 10m -count=1

test-coverage:
	go test ./... -v -race -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

vet:
	go vet ./...

lint: tools
	$(GOLANGCI_LINT) run ./...

generate: tools
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:dir=config/crd

docker-build:
	docker build -t $(IMAGE_NAME):$(VERSION) .

install-crds:
	kubectl apply -f config/crd/

deploy: install-crds
	kubectl apply -f config/rbac/
	kubectl apply -f config/deploy/

helm-lint:
	helm lint charts/ibm-vpc-file-pool-csi/

helm-template:
	helm template test-release charts/ibm-vpc-file-pool-csi/ > /dev/null
	@echo "Default values: OK"
	helm template test-release charts/ibm-vpc-file-pool-csi/ --set controller.replicas=3 > /dev/null
	@echo "Custom replicas: OK"
	helm template test-release charts/ibm-vpc-file-pool-csi/ --set metrics.serviceMonitor.enabled=true --set metrics.alerts.enabled=true > /dev/null
	@echo "Monitoring enabled: OK"
	helm template test-release charts/ibm-vpc-file-pool-csi/ --set secretProvider.managed=false > /dev/null
	@echo "Unmanaged secret provider: OK"
	helm template test-release charts/ibm-vpc-file-pool-csi/ --set volumeSnapshotClass.create=false --set storageClass.create=false > /dev/null
	@echo "Disabled optional resources: OK"
	@echo "All Helm template checks passed."

helm-install:
	helm upgrade --install ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi/ \
		--namespace kube-system

run-local:
	go run ./cmd/ --mode=controller --region=us-south --endpoint=unix:///tmp/csi.sock --v=4

tools:
	@test -x $(GOLANGCI_LINT) || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@test -x $(CONTROLLER_GEN) || go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

clean:
	rm -rf bin/ coverage.out coverage.html
