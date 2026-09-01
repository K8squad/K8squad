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
	$(MAKE) helm-sync-crds

.PHONY: generate
generate: controller-gen ## Generate DeepCopy method implementations (zz_generated.deepcopy.go).
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

# ISI-3297: codegen-drift gate. Regenerates everything codegen owns
# (zz_generated.deepcopy.go, config/crd/bases, config/helm-crds/templates) and fails if
# the result differs from what is committed — the markers in api/v1alpha1 are
# the source of truth, the YAML/deepcopy artifacts must never lag them.
# This is what .github/workflows/ci.yml (codegen-drift job) runs on every
# PR/push to main; run it locally after editing any +kubebuilder marker.
.PHONY: verify-codegen
verify-codegen: generate manifests ## Fail when generated code/manifests drift from the committed state (ISI-3297).
	@git diff --exit-code -- api/ config/crd/bases/ config/helm-crds/templates/ || { \
		echo ""; \
		echo "CODEGEN DRIFT (ISI-3297): generated artifacts do not match the committed state."; \
		echo "The api/v1alpha1 +kubebuilder markers are the source of truth. Run:"; \
		echo "  make generate manifests && git add api/ config/crd/bases/ config/helm-crds/templates/"; \
		echo "and commit the result alongside your marker changes."; \
		exit 1; }
	@echo "OK: no codegen drift (api/ config/crd/bases/ config/helm-crds/templates/ in sync)."

HELM ?= $(shell command -v helm 2>/dev/null)
CHART_DIR ?= config/helm
CRDS_CHART_DIR ?= config/helm-crds

# ISI-3518 / ADR-0002 (Option B): the 11 ksquad.io CRDs live as guarded Helm
# templates in the standalone k8squad-crds chart (config/helm-crds/templates/),
# NOT in the control-plane chart and NOT in Helm's install-only crds/ dir — so
# `helm upgrade k8squad-crds` propagates schema changes independently of the
# control plane. This target keeps the CRD chart in lockstep with the
# controller-gen source of truth (config/crd/bases): each base CRD is wrapped by
# hack/wrap-crd-template.sh (helm.sh/resource-policy: keep gated by .Values.keep,
# schema body verbatim). It removes stale CRD templates first (but preserves the
# chart's _helpers.tpl / NOTES.txt) so a removed CRD does not linger.
.PHONY: helm-sync-crds
helm-sync-crds: ## Sync generated CRDs (config/crd/bases) into config/helm-crds/templates/ as upgrade-safe templates.
	rm -f $(CRDS_CHART_DIR)/templates/ksquad.io_*.yaml
	mkdir -p $(CRDS_CHART_DIR)/templates
	@for f in config/crd/bases/*.yaml; do \
		bash hack/wrap-crd-template.sh "$$f" > "$(CRDS_CHART_DIR)/templates/$$(basename $$f)"; \
	done

.PHONY: helm-lint
helm-lint: ## Lint both Helm charts (control plane + CRDs).
	$(HELM) lint $(CRDS_CHART_DIR)
	$(HELM) lint $(CHART_DIR)

.PHONY: helm-template
helm-template: ## Render both Helm charts locally (no cluster needed).
	$(HELM) template k8squad-crds $(CRDS_CHART_DIR)
	$(HELM) template k8squad $(CHART_DIR)

.PHONY: helm-package
helm-package: helm-sync-crds ## Package both charts into .cr-release-packages/ (what CI publishes to charts.k8squad.io).
	mkdir -p .cr-release-packages
	$(HELM) package $(CRDS_CHART_DIR) -d .cr-release-packages
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

# ---------------------------------------------------------------------------
# Story 14.1 (ISI-2743) — L1 feature / functional test lane.
# The L1 layer (testing-strategy §3) is "each component's units + integration".
# These targets are the single named entrypoint the component-matrix pipeline
# (.github/workflows/l1.yml, ISI-2742 primitive) invokes per component, so the
# lane the humans read in CI and the command a developer runs locally are the
# same. Skeleton/unlanded components (operator, apiserver, shims, console) are
# skip-with-reason here and in the workflow — never silently omitted (§3.3, §10.4).
# ---------------------------------------------------------------------------

ENVTEST ?= $(LOCALBIN)/setup-envtest
# Pairs with controller-runtime v0.19.x (go.mod); bump alongside the K8s libs.
ENVTEST_VERSION ?= release-0.19
ENVTEST_K8S_VERSION ?= 1.31.0

.PHONY: l1
l1: l1-unit l1-integration l1-node ## Run the whole L1 feature/functional suite (unit + integration + console).

.PHONY: l1-unit
l1-unit: ## L1 unit half: race-enabled untagged feature/functional tests over every Go package.
	go test -race -covermode=atomic ./...

.PHONY: l1-integration
l1-integration: ## L1 integration half: service-backed feature tests. Each suite SKIPs when its DSN/URL is unset, so this is safe to run without services.
	go test -p 1 -tags=integration,discussion_integration ./...

.PHONY: setup-envtest
setup-envtest: $(LOCALBIN) ## Install setup-envtest (provisions the envtest kube-apiserver+etcd binaries for controller integration).
	test -s $(ENVTEST) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: l1-node
l1-node: ## L1 console half: Vitest units (skip-with-reason until console/package.json lands — §3.3 Epic 8).
	@if [ -f console/package.json ]; then \
	  cd console && npm ci && npm test -- --run ; \
	else \
	  echo ">> console/package.json absent — L1 node lane skipped (skip-with-reason, §3.3 Epic 8 / ISI-2743)"; \
	fi

.PHONY: conformance
conformance: ## Story 5.6 (GATE-BLOCKING): run the A2A shim conformance suite over the v1 shim set, both lanes.
	go test -race -count=1 ./conformance/...
	go run ./cmd/conformance -lane default
	go run ./cmd/conformance -lane ollama

.PHONY: conformance-ollama
conformance-ollama: ## Story 5.6 $0 lane: prove every byoModelEndpoint runtime is conformant on a BYO Ollama endpoint (ISI-2157).
	go run ./cmd/conformance -lane ollama
