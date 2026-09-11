.PHONY: build build-push build-criteria-k8s build-criteria-k8s-push images images-push deploy validate test lint

# Workflow (linear_intake_v1) image. Unique tags are mandatory for the k3s
# local registry: the kubelet never re-resolves a reused tag with
# IfNotPresent, so every build gets a timestamped tag.
WORKFLOW_IMAGE ?= localhost:5000/linear-intake-remote
CRITERIA_K8S_IMAGE ?= localhost:5000/criteria-k8s
REGISTRY ?= localhost:5000

# Unique tag: date + short git sha (falls back to timestamp outside a repo).
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo nosha)
BUILD_TAG := $(shell date +%Y%m%d-%H%M%S)-$(GIT_SHA)

# Prefer podman/buildah when available so the same Dockerfile builds in
# minimal CI runners that do not ship Docker.
CONTAINER_TOOL := $(shell command -v docker || command -v podman || command -v buildah || echo "")

build:
ifeq ($(CONTAINER_TOOL),)
	@echo "No container tool found; running validation/lint instead of building the image."
	$(MAKE) validate lint
else
	$(CONTAINER_TOOL) build --build-arg TARGETARCH=amd64 -f linear_intake_v1/Dockerfile -t $(WORKFLOW_IMAGE):$(BUILD_TAG) .
	@echo "Built $(WORKFLOW_IMAGE):$(BUILD_TAG)"
endif

build-push: build
ifneq ($(CONTAINER_TOOL),)
	$(CONTAINER_TOOL) push $(WORKFLOW_IMAGE):$(BUILD_TAG)
endif

build-criteria-k8s:
ifeq ($(CONTAINER_TOOL),)
	cd criteria-k8s && go build ./...
else
	$(CONTAINER_TOOL) build -f criteria-k8s/Dockerfile -t $(CRITERIA_K8S_IMAGE):$(BUILD_TAG) criteria-k8s/
endif

build-criteria-k8s-push: build-criteria-k8s
ifneq ($(CONTAINER_TOOL),)
	$(CONTAINER_TOOL) push $(CRITERIA_K8S_IMAGE):$(BUILD_TAG)
endif

# Build both images with one unique tag.
images: build build-criteria-k8s

images-push: build-push build-criteria-k8s-push
	@echo "Tag: $(BUILD_TAG)"
	@echo "Deploy the watcher with:"
	@echo "  kubectl -n criteria-jobs set env deploy/criteria-linear-watcher CRITERIA_IMAGE=$(WORKFLOW_IMAGE):$(BUILD_TAG)"
	@echo "  kubectl -n criteria-jobs set image deploy/criteria-k8s-operator operator=$(CRITERIA_K8S_IMAGE):$(BUILD_TAG)"

deploy-images: images-push
ifneq ($(CONTAINER_TOOL),)
	kubectl -n criteria-jobs set env deploy/criteria-linear-watcher CRITERIA_IMAGE=$(WORKFLOW_IMAGE):$(BUILD_TAG)
	kubectl -n criteria-jobs set image deploy/criteria-k8s-operator operator=$(CRITERIA_K8S_IMAGE):$(BUILD_TAG)
	kubectl -n criteria-jobs rollout status deploy/criteria-linear-watcher --timeout=120s
	kubectl -n criteria-jobs rollout status deploy/criteria-k8s-operator --timeout=120s
endif

validate:
	/usr/local/bin/criteria validate linear_intake_v1

test: validate test-criteria-k8s
	@echo "Rendering pod-adapter manifest..."
	./k8s/generate-pod-adapter-manifest.sh
	@echo "Running k8s pod-adapter regression test..."
	./k8s/tests/test_job_cri_27.sh
	@echo "Running k8s template regression test..."
	./k8s/tests/test_launch_template.sh
	@echo "Running container-entrypoint token substitution regression test..."
	./k8s/tests/test_container_entrypoint_substitution.sh
	@echo "Running Secrets Store CSI driver / OpenBao provider regression test..."
	./k8s/tests/test_secrets_store_csi.sh
	@echo "Running example manifest regression test..."
	./k8s/tests/test_example_manifest.sh

test-criteria-k8s:
	cd criteria-k8s && go test ./...

lint: lint-criteria-k8s
	shellcheck linear_intake_v1/container-entrypoint.sh \
		k8s/launch-ticket-job.sh \
		k8s/generate-pod-adapter-manifest.sh \
		k8s/launch-pod-adapter-job.sh \
		k8s/pod-adapter-runner.sh \
		k8s/pod-adapter-adapter.sh \
		k8s/install-secrets-store-csi.sh \
		k8s/verify-secrets-store-csi.sh \
		k8s/tests/test_launch_template.sh \
		k8s/tests/test_job_cri_27.sh \
		k8s/tests/test_container_entrypoint_substitution.sh \
		k8s/tests/test_secrets_store_csi.sh \
		k8s/tests/test_example_manifest.sh

lint-criteria-k8s:
	cd criteria-k8s && go vet ./...