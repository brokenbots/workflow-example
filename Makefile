.PHONY: build validate test lint

IMAGE_NAME ?= linear-intake
IMAGE_TAG ?= latest

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

test: validate
	@echo "Rendering pod-adapter manifest..."
	./k8s/generate-pod-adapter-manifest.sh
	@echo "Running k8s pod-adapter regression test..."
	./k8s/tests/test_job_cri_27.sh
	@echo "Running k8s template regression test..."
	./k8s/tests/test_launch_template.sh

lint:
	shellcheck linear_intake_v1/container-entrypoint.sh \
		k8s/launch-ticket-job.sh \
		k8s/generate-pod-adapter-manifest.sh \
		k8s/launch-pod-adapter-job.sh \
		k8s/pod-adapter-runner.sh \
		k8s/pod-adapter-sidecar.sh \
		k8s/tests/test_launch_template.sh \
		k8s/tests/test_job_cri_27.sh
