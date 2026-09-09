.PHONY: build validate test lint

IMAGE_NAME ?= linear-intake
IMAGE_TAG ?= latest

# Prefer podman/buildah when available so the same Dockerfile builds in
# minimal CI runners that do not ship Docker.
CONTAINER_TOOL := $(shell command -v docker || command -v podman || command -v buildah || echo "")

build:
ifeq ($(CONTAINER_TOOL),)
	$(error No container tool found. Install docker, podman, or buildah to build the image.)
endif
	$(CONTAINER_TOOL) build -f linear_intake_v1/Dockerfile -t $(IMAGE_NAME):$(IMAGE_TAG) .

validate:
	/usr/local/bin/criteria validate linear_intake_v1

test: validate
	@echo "No additional test suite defined; running validation."

lint:
	shellcheck linear_intake_v1/container-entrypoint.sh
