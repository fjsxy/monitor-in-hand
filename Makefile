
# Image URL to use all building/pushing image targets
IMG ?= docker.io/fjsxy/monitor-in-hand:0.1.0
HELM ?= $(shell command -v helm 2>/dev/null || printf '%s/.tools/bin/helm' '$(CURDIR)')
RELEASE ?= monitor-in-hand
NAMESPACE ?= monitor-in-hand-system
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
	$(CONTROLLER_GEN) rbac:roleName=monitor-in-hand-manager-role paths="./..." output:rbac:artifacts:config=config/rbac
	cp config/rbac/role.yaml charts/monitor-in-hand/generated/manager-role.yaml

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test test-unit test-helm test-testenv typecheck lint-chart verify-generated smoke-install preflight test-env-up test-env-verify test-env-evidence test-env-down
test: manifests verify-generated generate fmt typecheck test-unit test-helm test-testenv ## Run all tests.

test-unit: ## Run control-plane unit tests.
	go test ./internal/... ./cmd/...

test-helm: ## Run Helm contract tests.
	HELM_BIN="$(HELM)" go test ./test/helm

test-testenv: ## Run the external test-environment CLI contract tests.
	go test ./test/testenv

typecheck: vet ## Compile all Go packages.
	go build ./...

lint-chart: ## Lint the supported Helm installation artifact.
	$(HELM) lint charts/monitor-in-hand

verify-generated: manifests ## Verify Helm RBAC is synchronized from Kubebuilder markers.
	cmp config/rbac/role.yaml charts/monitor-in-hand/generated/manager-role.yaml
	git diff --exit-code -- config/rbac/role.yaml charts/monitor-in-hand/generated/manager-role.yaml

smoke-install: ## Run the isolated kind installation smoke test.
	./hack/smoke-install.sh

preflight: ## Strictly qualify a cluster before creating a native PrometheusAgent.
	./hack/preflight.sh \
		--agent-namespace "$(AGENT_NAMESPACE)" \
		--agent-service-account "$(AGENT_SERVICE_ACCOUNT)" \
		--config-namespaces "$(CONFIG_NAMESPACES)" \
		--discovery-namespaces "$(DISCOVERY_NAMESPACES)" \
		$(if $(OPERATOR_NAMESPACE),--operator-namespace "$(OPERATOR_NAMESPACE)") \
		$(if $(OPERATOR_DEPLOYMENT),--operator-deployment "$(OPERATOR_DEPLOYMENT)") \
		$(if $(KUBE_CONTEXT),--context "$(KUBE_CONTEXT)") \
		$(if $(PREFLIGHT_TIMEOUT_SECONDS),--timeout-seconds "$(PREFLIGHT_TIMEOUT_SECONDS)")

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
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	test -s $(LOCALBIN)/setup-envtest || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
