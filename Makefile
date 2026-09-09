.PHONY: build validate test lint

IMAGE_NAME ?= linear-intake
IMAGE_TAG ?= latest

build:
	docker build -f linear_intake_v1/Dockerfile -t $(IMAGE_NAME):$(IMAGE_TAG) .

validate:
	/usr/local/bin/criteria validate linear_intake_v1

test: validate
	@echo "No additional test suite defined; running validation."

lint:
	shellcheck linear_intake_v1/container-entrypoint.sh
