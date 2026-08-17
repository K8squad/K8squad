# K8squad control-plane Makefile (kubebuilder go.kubebuilder.io/v4 layout).
# Story 1.1: `make generate manifests` builds the ksquad.io/v1alpha1 API group,
# emitting zz_generated.deepcopy.go and CRD manifests under config/crd/bases.

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
# Pin controller-tools to the release train that pairs with k8s.io/* v0.31.x
# (see go.mod). Bump alongside the K8s libs.
CONTROLLER_TOOLS_VERSION ?= v0.16.5

.PHONY: all
all: generate manifests

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate DeepCopy method implementations (zz_generated.deepcopy.go).
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

HELM ?= $(shell command -v helm 2>/dev/null)
CHART_DIR ?= config/helm

.PHONY: helm-sync-crds
helm-sync-crds: ## Sync generated CRDs (config/crd/bases) into the Helm chart's crds/ dir.
	mkdir -p $(CHART_DIR)/crds
	cp config/crd/bases/*.yaml $(CHART_DIR)/crds/

.PHONY: helm-lint
helm-lint: ## Lint the control-plane Helm chart.
	$(HELM) lint $(CHART_DIR)

.PHONY: helm-template
helm-template: ## Render the control-plane Helm chart locally (CRDs included, no cluster needed).
	$(HELM) template k8squad $(CHART_DIR) --include-crds

.PHONY: helm-package
helm-package: helm-sync-crds ## Package the chart into .cr-release-packages/ (what CI publishes to charts.k8squad.io).
	mkdir -p .cr-release-packages
	$(HELM) package $(CHART_DIR) -d .cr-release-packages

.PHONY: quickstart
quickstart: ## Assemble dist/quickstart.yaml (the quickstart squad published to charts.k8squad.io/quickstart.yaml).
	CHART_DIR=$(CHART_DIR) bash hack/gen-quickstart.sh

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: cover
cover: ## Story 14.5 (L5): run tests with -race + coverage and score per-package (>=80%, >=90% pkg/coord).
	go test -race -covermode=atomic -coverprofile=coverage.out ./...
	go run ./cmd/covgate coverage.out

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Download controller-gen locally if necessary.
	test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
