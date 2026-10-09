
# Image URL to use all building/pushing image targets
IMG ?= docker.io/fjsxy/monitor-in-hand:0.1.0
HELM ?= $(shell command -v helm 2>/dev/null || printf '%s/.tools/bin/helm' '$(CURDIR)')
GINKGO ?= go run github.com/onsi/ginkgo/v2/ginkgo
KIND ?= kind
RELEASE ?= monitor-in-hand
NAMESPACE ?= monitor-in-hand-system
E2E_REPORT_DIR ?= artifacts/e2e/$(TEST_RUN_ID)-$(KUBERNETES_VERSION)
E2E_CLUSTER_NAME = mih-e2e-$(subst .,,$(KUBERNETES_VERSION))-$(TEST_RUN_ID)
E2E_KUBE_CONTEXT = kind-$(E2E_CLUSTER_NAME)
TEST_RESULTS_DIR ?= artifacts/test-results
UNIT_REPORT_DIR ?= $(TEST_RESULTS_DIR)/unit
INTEGRATION_REPORT_DIR ?= $(TEST_RESULTS_DIR)/integration
HELM_REPORT_DIR ?= $(TEST_RESULTS_DIR)/helm
TESTENV_REPORT_DIR ?= $(TEST_RESULTS_DIR)/testenv
UNIT_COVERAGE_PROFILE ?= $(UNIT_REPORT_DIR)/unit.out
# Kubebuilder v3.13.0 generated this project and matches the controller-runtime v0.16 line.
KUBEBUILDER_VERSION ?= v3.13.0
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.28.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=monitor-in-hand-manager-role crd paths="./..." \
		output:rbac:artifacts:config=config/rbac \
		output:crd:artifacts:config=config/crd/bases
	mkdir -p charts/monitor-in-hand/crds
	cp config/rbac/role.yaml charts/monitor-in-hand/generated/manager-role.yaml
	cp config/crd/bases/monitoring.fjsxy.github.io_kubestatemetrics.yaml charts/monitor-in-hand/crds/

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test test-unit test-integration test-helm test-testenv typecheck lint-chart verify-generated smoke-install preflight e2e-preflight e2e-agent e2e-image e2e-operator-install e2e-collection e2e-kubestatemetrics e2e-default-deployment test-env-up test-env-verify test-env-evidence test-env-down
test: manifests verify-generated generate fmt typecheck test-unit test-integration test-helm test-testenv ## Run all tests.

test-unit: ## Run control-plane unit tests and enforce phase-one statement coverage.
	mkdir -p "$(UNIT_REPORT_DIR)"
	$(GINKGO) -r --fail-on-pending --fail-on-empty --keep-separate-reports \
		--junit-report=junit.xml --output-dir="$(UNIT_REPORT_DIR)" \
		--cover --covermode=atomic --coverprofile=unit.out ./internal/...
	$(GINKGO) --fail-on-pending --fail-on-empty \
		--junit-report=junit.xml --output-dir="$(UNIT_REPORT_DIR)" ./cmd
	mkdir -p "$(UNIT_REPORT_DIR)/collection-support" "$(UNIT_REPORT_DIR)/kubestatemetrics-support"
	$(GINKGO) --fail-on-pending --fail-on-empty \
		--junit-report=junit.xml --output-dir="$(UNIT_REPORT_DIR)/collection-support" ./test/e2e/collection/support
	$(GINKGO) --fail-on-pending --fail-on-empty \
		--junit-report=junit.xml --output-dir="$(UNIT_REPORT_DIR)/kubestatemetrics-support" ./test/e2e/kubestatemetrics/support
	go run ./cmd/phase1-gate --coverage-profile "$(UNIT_COVERAGE_PROFILE)" \
		--aggregate-coverage 80 --package-coverage 70

test-integration: envtest manifests ## Run controller integration tests against a real API server.
	mkdir -p "$(INTEGRATION_REPORT_DIR)"
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		$(GINKGO) -r --fail-on-pending --fail-on-empty --keep-separate-reports \
		--junit-report=junit.xml --output-dir="$(INTEGRATION_REPORT_DIR)" ./test/integration/...

test-helm: ## Run Helm contract tests.
	mkdir -p "$(HELM_REPORT_DIR)"
	HELM_BIN="$(HELM)" $(GINKGO) --fail-on-pending --fail-on-empty \
		--junit-report=junit.xml --output-dir="$(HELM_REPORT_DIR)" ./test/helm

test-testenv: ## Run the external test-environment CLI contract tests.
	mkdir -p "$(TESTENV_REPORT_DIR)"
	$(GINKGO) --fail-on-pending --fail-on-empty \
		--junit-report=junit.xml --output-dir="$(TESTENV_REPORT_DIR)" ./test/testenv

typecheck: vet ## Compile all Go packages.
	go build ./...

lint-chart: ## Lint the supported Helm installation artifact.
	$(HELM) lint charts/monitor-in-hand

verify-generated: manifests ## Verify Helm RBAC is synchronized from Kubebuilder markers.
	cmp config/rbac/role.yaml charts/monitor-in-hand/generated/manager-role.yaml
	cmp config/crd/bases/monitoring.fjsxy.github.io_kubestatemetrics.yaml charts/monitor-in-hand/crds/monitoring.fjsxy.github.io_kubestatemetrics.yaml
	sha256sum --check config/crd/baselines/v0.1.0.sha256
	git diff --exit-code -- config/rbac/role.yaml config/crd/bases/monitoring.fjsxy.github.io_kubestatemetrics.yaml charts/monitor-in-hand/generated/manager-role.yaml charts/monitor-in-hand/crds/monitoring.fjsxy.github.io_kubestatemetrics.yaml

smoke-install: ## Run the isolated kind installation smoke test.
	./hack/smoke-install.sh

preflight: ## Strictly qualify a cluster before creating a native PrometheusAgent.
	./hack/preflight.sh \
		--agent-namespace "$(AGENT_NAMESPACE)" \
		--agent-service-account "$(AGENT_SERVICE_ACCOUNT)" \
		--config-namespaces "$(CONFIG_NAMESPACES)" \
		--discovery-namespaces "$(DISCOVERY_NAMESPACES)" \
		$(if $(AGENT_MANIFEST),--agent-manifest "$(AGENT_MANIFEST)") \
		$(if $(COLLECTION_CONFIG_MANIFEST),--collection-config-manifest "$(COLLECTION_CONFIG_MANIFEST)") \
		$(if $(OPERATOR_NAMESPACE),--operator-namespace "$(OPERATOR_NAMESPACE)") \
		$(if $(OPERATOR_DEPLOYMENT),--operator-deployment "$(OPERATOR_DEPLOYMENT)") \
		$(if $(KUBE_CONTEXT),--context "$(KUBE_CONTEXT)") \
		$(if $(PREFLIGHT_TIMEOUT_SECONDS),--timeout-seconds "$(PREFLIGHT_TIMEOUT_SECONDS)")

e2e-preflight: ## Verify strict deployment preflight against the current pinned test environment.
	mkdir -p "$(E2E_REPORT_DIR)/preflight"
	KUBERNETES_VERSION="$(KUBERNETES_VERSION)" TEST_RUN_ID="$(TEST_RUN_ID)" \
		$(GINKGO) -v --fail-on-pending --fail-on-empty --timeout=20m \
		--junit-report=junit.xml --json-report=report.json --output-dir="$(E2E_REPORT_DIR)/preflight" \
		./test/e2e/preflight

e2e-agent: e2e-preflight e2e-collection ## Verify preflight and native PrometheusAgent boundaries in an independent lane.

e2e-image: ## Build and load the current worktree Operator image into the active E2E cluster.
	mkdir -p bin/e2e-kubestatemetrics
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o bin/e2e-kubestatemetrics/manager ./cmd
	BUILDKIT_PROGRESS=plain $(CONTAINER_TOOL) build --file test/e2e/kubestatemetrics/Dockerfile --tag "$(IMG)" bin/e2e-kubestatemetrics
	$(KIND) load docker-image --name "$(E2E_CLUSTER_NAME)" "$(IMG)"

e2e-operator-install: e2e-image ## Install the current worktree Operator for standalone Agent boundary E2E.
	$(KUBECTL) --context "$(E2E_KUBE_CONTEXT)" create namespace "$(NAMESPACE)" --dry-run=client --output yaml | \
		$(KUBECTL) --context "$(E2E_KUBE_CONTEXT)" apply --filename -
	$(HELM) upgrade --install "$(RELEASE)" charts/monitor-in-hand \
		--kube-context "$(E2E_KUBE_CONTEXT)" --namespace "$(NAMESPACE)" --wait --timeout 180s

e2e-collection: e2e-operator-install ## Verify native PrometheusAgent collection and boundaries independently.
	mkdir -p "$(E2E_REPORT_DIR)/collection"
	KUBERNETES_VERSION="$(KUBERNETES_VERSION)" TEST_RUN_ID="$(TEST_RUN_ID)" \
		$(GINKGO) -v --fail-on-pending --fail-on-empty --timeout=35m \
		--junit-report=junit.xml --json-report=report.json --output-dir="$(E2E_REPORT_DIR)/collection" \
		./test/e2e/collection

e2e-kubestatemetrics: e2e-image ## Verify KubeStateMetrics deployment modes against real managed workloads.
	mkdir -p "$(E2E_REPORT_DIR)/kubestatemetrics"
	KUBERNETES_VERSION="$(KUBERNETES_VERSION)" TEST_RUN_ID="$(TEST_RUN_ID)" HELM_BIN="$(HELM)" \
		$(GINKGO) -v --fail-on-pending --fail-on-empty --timeout=35m \
		--junit-report=junit.xml --json-report=report.json --output-dir="$(E2E_REPORT_DIR)/kubestatemetrics" \
		./test/e2e/kubestatemetrics

e2e-default-deployment: e2e-kubestatemetrics ## Compatibility alias for the original E2E entry point.

test-env-up: ## Create the pinned external test environment for one supported Kubernetes minor.
	KUBERNETES_VERSION="$(KUBERNETES_VERSION)" TEST_RUN_ID="$(TEST_RUN_ID)" ./hack/test-env.sh up

test-env-verify: ## Verify the external test environment through its Kubernetes interfaces.
	KUBERNETES_VERSION="$(KUBERNETES_VERSION)" TEST_RUN_ID="$(TEST_RUN_ID)" ./hack/test-env.sh verify

test-env-evidence: ## Save diagnostics for a failed external test-environment run.
	KUBERNETES_VERSION="$(KUBERNETES_VERSION)" TEST_RUN_ID="$(TEST_RUN_ID)" ./hack/test-env.sh evidence

test-env-down: ## Delete the test environment only when its run identity matches.
	KUBERNETES_VERSION="$(KUBERNETES_VERSION)" TEST_RUN_ID="$(TEST_RUN_ID)" ./hack/test-env.sh down

GOLANGCI_LINT = $(shell pwd)/bin/golangci-lint
GOLANGCI_LINT_VERSION ?= v1.54.2
golangci-lint:
	@[ -f $(GOLANGCI_LINT) ] || { \
	set -e ;\
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(shell dirname $(GOLANGCI_LINT)) $(GOLANGCI_LINT_VERSION) ;\
	}

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter & yamllint
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name project-v3-builder
	$(CONTAINER_TOOL) buildx use project-v3-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm project-v3-builder
	rm Dockerfile.cross

##@ Deployment

.PHONY: install
install: manifests verify-generated lint-chart ## Install through the only supported entry point: Helm.
	$(HELM) upgrade --install $(RELEASE) charts/monitor-in-hand --namespace $(NAMESPACE)

.PHONY: uninstall
uninstall: ## Uninstall the Helm release.
	$(HELM) uninstall $(RELEASE) --namespace $(NAMESPACE)

.PHONY: deploy
deploy: install ## Alias for the Helm-only installation entry point.

.PHONY: undeploy
undeploy: uninstall ## Alias for Helm uninstall.

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/.tools/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest

## Tool Versions
KUSTOMIZE_VERSION ?= v5.2.1
CONTROLLER_TOOLS_VERSION ?= v0.16.5
ENVTEST_VERSION ?= v0.0.0-20250308055145-5fe7bb3edc86

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary. If wrong version is installed, it will be removed before downloading.
$(KUSTOMIZE): $(LOCALBIN)
	@if test -x $(LOCALBIN)/kustomize && ! $(LOCALBIN)/kustomize version | grep -q $(KUSTOMIZE_VERSION); then \
		echo "$(LOCALBIN)/kustomize version is not expected $(KUSTOMIZE_VERSION). Removing it before installing."; \
		rm -rf $(LOCALBIN)/kustomize; \
	fi
	test -s $(LOCALBIN)/kustomize || GOBIN=$(LOCALBIN) GO111MODULE=on go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary. If wrong version is installed, it will be overwritten.
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(LOCALBIN)/controller-gen && $(LOCALBIN)/controller-gen --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(LOCALBIN) ## Download envtest-setup locally if necessary.
	test -s $(LOCALBIN)/setup-envtest && \
		go version -m $(LOCALBIN)/setup-envtest | grep -Fq "$(ENVTEST_VERSION)" || \
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)
