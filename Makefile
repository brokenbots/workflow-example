.PHONY: build validate test lint build-criteria-k8s test-criteria-k8s lint-criteria-k8s

IMAGE_NAME ?= linear-intake
IMAGE_TAG ?= latest
CRITERIA_K8S_IMAGE ?= localhost:5000/criteria-k8s
CRITERIA_K8S_TAG ?= dev

# Prefer podman/buildah when available so the same Dockerfile builds in
# minimal CI runners that do not ship Docker.
CONTAINER_TOOL := $(shell command -v docker || command -v podman || command -v buildah || echo "")

build:
ifeq ($(CONTAINER_TOOL),)
	@echo "No container tool found; running validation/lint instead of building the image."
	$(MAKE) validate lint
else
	$(CONTAINER_TOOL) build -f linear_intake_v1/Dockerfile -t $(IMAGE_NAME):$(IMAGE_TAG) .
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
		k8s/pod-adapter-sidecar.sh \
		k8s/install-secrets-store-csi.sh \
		k8s/verify-secrets-store-csi.sh \
		k8s/tests/test_launch_template.sh \
		k8s/tests/test_job_cri_27.sh \
		k8s/tests/test_container_entrypoint_substitution.sh \
		k8s/tests/test_secrets_store_csi.sh \
		k8s/tests/test_example_manifest.sh

lint-criteria-k8s:
	cd criteria-k8s && go vet ./...

build-criteria-k8s:
ifeq ($(CONTAINER_TOOL),)
	cd criteria-k8s && go build ./...
else
	$(CONTAINER_TOOL) build -f criteria-k8s/Dockerfile -t $(CRITERIA_K8S_IMAGE):$(CRITERIA_K8S_TAG) .
endif
