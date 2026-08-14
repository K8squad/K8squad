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

# crd:allowDangerousTypes=true is required for OTelConfig SamplingConfig.Ratio
# (*float64, story 1.5). Without it controller-gen refuses floats and writes a
# partial CRD with an empty ratio schema. The generated schema is `type: number`
# with minimum/maximum bounds — acceptable for this Go-only control plane.
.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate DeepCopy method implementations (zz_generated.deepcopy.go).
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: controller-gen
controller-gen: $(LOCALBIN) ## Download controller-gen locally if necessary.
	test -s $(CONTROLLER_GEN) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)
