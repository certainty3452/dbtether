VERSION ?= $(shell sed -n 's/^appVersion: *"\(.*\)"/\1/p' charts/dbtether/Chart.yaml)
IMG ?= ghcr.io/certainty3452/dbtether:$(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64

CONTROLLER_GEN_VERSION ?= v0.20.1
GOIMPORTS_VERSION ?= v0.51.0
GOSEC_VERSION ?= v2.29.0
GOVULNCHECK_VERSION ?= v1.8.0
GOLANGCI_LINT_VERSION ?= v2.14.0
SETUP_ENVTEST_VERSION ?= release-0.22
ENVTEST_K8S_VERSION ?= 1.31.0

print-%:
	@echo $($*)

.PHONY: build
build:
	go build -o bin/manager main.go

.PHONY: test
test: test-envtest

.PHONY: fmt
fmt:
	gofmt -s -w .
	go run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -w .

.PHONY: vet
vet:
	go vet ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: security
security:
	go run github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION) ./...
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: check
check: fmt vet lint security

.PHONY: docker-build
docker-build:
	docker build -t ${IMG} .

.PHONY: docker-buildx
docker-buildx:
	docker buildx build --platform $(PLATFORMS) -t ${IMG} --push .

.PHONY: docker-push
docker-push:
	docker push ${IMG}


.PHONY: manifests
manifests:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) crd paths="./..." output:crd:artifacts:config=charts/dbtether/crds

.PHONY: generate
generate:
	go run sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: test-unit
test-unit:
	go test ./pkg/... -v -coverprofile=cover.out

.PHONY: test-integration
test-integration:
	go test ./pkg/postgres/ ./pkg/backup/ -tags=integration -count=1 -v

.PHONY: test-envtest
test-envtest:
	KUBEBUILDER_ASSETS=$$($(MAKE) -s envtest-path) go test ./... -tags=envtest -v -coverprofile=cover.out

.PHONY: envtest-path
envtest-path:
	@go run sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin -p path
