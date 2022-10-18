
SHELL := /usr/bin/env bash

# NUM_CLUSTERS is a total number of kind clusters to be provisioned
NUM_CLUSTERS ?= 1
DO_BREW := true
USE_CRC := false
CREATE_KIND_ROUTE_SYNC_TARGET := false
RUN_GLBC := false
KCP_BRANCH := release-0.9

IMAGE_TAG_BASE ?= quay.io/kuadrant/kcp-glbc
IMAGE_TAG ?= latest
IMG ?= $(IMAGE_TAG_BASE):$(IMAGE_TAG)

KUBECONFIG ?= $(shell pwd)/.kcp/admin.kubeconfig
CLUSTERS_KUBECONFIG_DIR ?= $(shell pwd)/tmp

PROMTOOL_IMAGE := quay.io/prometheus/prometheus:v2.36.2

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: clean
clean: clean-ld-apiexports clean-ld-synctargets ## Clean up temporary files.
	-rm -rf ./.kcp
	-rm -f ./bin/*
	-rm -rf ./tmp

.PHONY: generate
generate: generate-deepcopy generate-crd generate-client ## Generate code containing DeepCopy method implementations, CustomResourceDefinition objects and Clients.

.PHONY: generate-deepcopy
generate-deepcopy: controller-gen
	cd pkg/apis/kuadrant && $(CONTROLLER_GEN) paths="./..." object

.PHONY: generate-deepcopy
generate-crd: controller-gen
	cd pkg/apis/kuadrant && $(CONTROLLER_GEN) crd paths=./... output:crd:artifacts:config=../../../config/crd output:crd:dir=../../../config/crd/bases crd:crdVersions=v1 && rm -rf ./config

.PHONY: generate-client
generate-client:
	./scripts/gen_client.sh

.PHONY: vendor
vendor: ## Vendor the dependencies.
	go mod tidy
	go mod vendor

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint against code.
	golangci-lint run ./...

.PHONY: test
test: generate ## Run tests.
	go test -v ./... -coverprofile=cover.out

##@ Test

.PHONY: e2e
e2e: build ## Run e2e tests.
	## Run the metrics test first, so it starts from a clean state
	KUBECONFIG="$(KUBECONFIG)" CLUSTERS_KUBECONFIG_DIR="$(CLUSTERS_KUBECONFIG_DIR)" \
	AWS_DNS_PUBLIC_ZONE_ID="${AWS_DNS_PUBLIC_ZONE_ID}" \
	go test -count=1 -timeout 60m -v ./test/e2e/metrics -tags=e2e
	## Run the other tests
	KUBECONFIG="$(KUBECONFIG)" CLUSTERS_KUBECONFIG_DIR="$(CLUSTERS_KUBECONFIG_DIR)" \
	go test -count=1 -timeout 60m -v ./test/e2e -tags=e2e

TEST_DNSRECORD_COUNT ?= 2
TEST_INGRESS_COUNT ?= 2
TEST_WORKSPACE_COUNT ?= 2
.PHONY: performance
performance: TEST_TAGS ?=performance,ingress,dnsrecord
performance: build ## Run performance tests.
	@date +"Performance Test Start: %s%3N"
	KUBECONFIG="$(KUBECONFIG)" \
	AWS_DNS_PUBLIC_ZONE_ID="$(AWS_DNS_PUBLIC_ZONE_ID)" \
	TEST_DNSRECORD_COUNT="$(TEST_DNSRECORD_COUNT)" \
	TEST_INGRESS_COUNT="$(TEST_INGRESS_COUNT)" \
	TEST_WORKSPACE_COUNT="$(TEST_WORKSPACE_COUNT)" \
	go test -count=1 -timeout 60m -v ./test/performance -tags=$(TEST_TAGS)
	@date +"Performance Test End: %s%3N"

.PHONY: smoke
smoke: TEST_TAGS ?=smoke,ingress
smoke: build ## Run smoke tests.
	KUBECONFIG="$(KUBECONFIG)" \
	AWS_DNS_PUBLIC_ZONE_ID="$(AWS_DNS_PUBLIC_ZONE_ID)" \
	go test -count=1 -timeout 60m -v ./test/smoke -tags=$(TEST_TAGS)

TEST_OIDC_KUBECONFIG ?= test.oidc.kubeconfig
.PHONY: generate-test-oidc-kubeconfig
generate-test-oidc-kubeconfig: TEST_KUBE_HOST ?=""
generate-test-oidc-kubeconfig: TEST_KUBE_OIDC_ISSUER_URL ?=""
generate-test-oidc-kubeconfig: TEST_KUBE_OIDC_CLIENT_ID ?=""
generate-test-oidc-kubeconfig: TEST_KUBE_OIDC_CLIENT_SECRET ?=""
generate-test-oidc-kubeconfig: $(TEST_OIDC_KUBECONFIG) ## Generate test OIDC kubeconfig.
$(TEST_OIDC_KUBECONFIG):
	kubectl config --kubeconfig=$(TEST_OIDC_KUBECONFIG) set-cluster kcp --server=$(TEST_KUBE_HOST)
	kubectl config --kubeconfig=$(TEST_OIDC_KUBECONFIG) set-credentials oidc \
		--auth-provider=oidc \
		--auth-provider-arg=idp-issuer-url=$(TEST_KUBE_OIDC_ISSUER_URL) \
		--auth-provider-arg=client-id=$(TEST_KUBE_OIDC_CLIENT_ID)\
		--auth-provider-arg=refresh-token=$(TEST_KUBE_OIDC_CLIENT_SECRET)
	kubectl config --kubeconfig=$(TEST_OIDC_KUBECONFIG) set-context system:admin --cluster=kcp --user=oidc
	kubectl config --kubeconfig=$(TEST_OIDC_KUBECONFIG) use-context system:admin

##@ CI

#Note, these targets are expected to run in a clean CI environment.

.PHONY: verify-generate
verify-generate: generate ## Verify generate update.
	git diff --exit-code

##@ Build

.PHONY: build
build: ## Build the project.
	go build -o bin ./cmd/...

.PHONY: docker-build
docker-build: ## Build docker image.
	docker build -t ${IMG} .

##@ Deployment

.PHONY: install
install: generate-crd kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl apply -f -

.PHONY: uninstall
uninstall: generate-crd kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | kubectl delete -f -

.PHONY: deploy
deploy: generate-crd kustomize helm generate-ld-config ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/deploy/local/kcp-glbc --enable-helm --helm-command $(HELM) | kubectl apply -f -

.PHONY: undeploy
undeploy: kustomize helm ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/deploy/local/kcp-glbc --enable-helm --helm-command $(HELM) | kubectl delete -f -

## Local Deployment
LD_DIR=config/deploy/local/kcp-glbc
LD_APIEXPORTS_DIR=$(LD_DIR)/apiexports/glbc
LD_SYNC_TARGETS=$(LD_DIR)/../../sync-targets
LD_AWS_CREDS_ENV=$(LD_DIR)/aws-credentials.env
LD_CONTROLLER_CONFIG_ENV=$(LD_DIR)/controller-config.env

$(LD_AWS_CREDS_ENV):
	envsubst \
        < $(LD_AWS_CREDS_ENV).template \
        > $(LD_AWS_CREDS_ENV)

$(LD_CONTROLLER_CONFIG_ENV):
	envsubst \
		< $(LD_CONTROLLER_CONFIG_ENV).template \
		> $(LD_CONTROLLER_CONFIG_ENV)

.PHONY: generate-ld-config
generate-ld-config: $(LD_AWS_CREDS_ENV) $(LD_CONTROLLER_CONFIG_ENV) ## Generate local deployment files.

.PHONY: clean-ld-env
clean-ld-env:
	-rm -f $(LD_AWS_CREDS_ENV)
	-rm -f $(LD_CONTROLLER_CONFIG_ENV)

.PHONY: clean-ld-apiexports
clean-ld-apiexports:
	-rm -f $(LD_APIEXPORTS_DIR)/glbc-apiexport-config.env
	-rm -f $(LD_APIEXPORTS_DIR)/glbc-apibinding.yaml

.PHONY: clean-ld-synctargets
clean-ld-synctargets:
	-rm -f $(LD_SYNC_TARGETS)/*.yaml

.PHONY: clean-ld-config
clean-ld-config: clean-ld-env clean-ld-apiexports clean-ld-synctargets ## Remove local deployment files.

LOCAL_SETUP_FLAGS=""
ifeq ($(DO_BREW),true)
	LOCAL_SETUP_FLAGS:=-b
endif

.PHONY: local-setup
local-setup: export KCP_VERSION=${KCP_BRANCH}
local-setup: export USE_CRC_CLUSTER=${USE_CRC}
local-setup: export CREATE_KIND_ROUTE_SYNC_TARGET=${$CREATE_KIND_ROUTE_SYNC_TARGET}
local-setup: export DEPLOY_GLBC=${RUN_GLBC} # Optionally run GLBC after local-setup is done.
local-setup: clean kind kcp kustomize helm build ## Setup kcp locally using kind.
	./utils/local-setup.sh -c ${NUM_CLUSTERS} ${LOCAL_SETUP_FLAGS}

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN): ## Ensure that the directory exists
	mkdir -p $(LOCALBIN)

## Tool Binaries
KCP ?= $(LOCALBIN)/kcp
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
KIND ?= $(LOCALBIN)/kind
HELM ?= $(LOCALBIN)/helm

## Tool Versions
KUSTOMIZE_VERSION ?= v4.5.4
CONTROLLER_TOOLS_VERSION ?= v0.8.0
KIND_VERSION ?= v0.14.0
HELM_VERSION ?= v3.10.0

.PHONY: kcp
kcp: $(KCP) ## Download kcp locally if necessary.
$(KCP):
	rm -rf ./tmp/kcp
	git clone --depth=1 --branch ${KCP_BRANCH} https://github.com/kcp-dev/kcp ./tmp/kcp
	cd ./tmp/kcp && make
	cp ./tmp/kcp/bin/* $(LOCALBIN)
	rm -rf ./tmp/kcp

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: kind
kind: $(KIND) ## Download kind locally if necessary.
$(KIND):
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/kind@$(KIND_VERSION)

KUSTOMIZE_INSTALL_SCRIPT ?= "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh"
.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE):
	curl -s $(KUSTOMIZE_INSTALL_SCRIPT) | bash -s -- $(subst v,,$(KUSTOMIZE_VERSION)) $(LOCALBIN)

HELM_INSTALL_SCRIPT ?= "https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3"
.PHONY: helm
helm: $(HELM)
$(HELM):
	curl -s $(HELM_INSTALL_SCRIPT) | HELM_INSTALL_DIR=$(LOCALBIN) PATH=$$PATH:$$HELM_INSTALL_DIR bash -s -- --no-sudo --version $(HELM_VERSION)

# Generate metrics adoc content based on /metrics response from a running server
.PHONY: gen-metrics-docs
gen-metrics-docs:
	curl http://localhost:8080/metrics > tmp/metrics.pef
	go run ./utils/prometheus_format.go -f tmp/metrics.pef -c utils/prometheus_format_tables.csv > docs/observability/generated_metrics.adoc

# Ensure the generated metrics content is latest
.PHONY: verify-gen-metrics-docs
verify-gen-metrics-docs: gen-metrics-docs
	git diff --exit-code

include ./config/observability/observability.mk
