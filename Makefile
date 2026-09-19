.PHONY: build build-push build-criteria-k8s build-criteria-k8s-push images images-push deploy validate test lint apply-routes

# Workflow (linear_intake_v1) image. Unique tags are mandatory for the k3s
# local registry: the kubelet never re-resolves a reused tag with
# IfNotPresent, so every build gets a timestamped tag.
WORKFLOW_IMAGE ?= localhost:5000/linear-intake-remote
CRITERIA_K8S_IMAGE ?= localhost:5000/criteria-k8s
# Minimal source-fetching base image (CRI-230): criteria binary + git +
# ca-certs, fetched-and-applied at run time. Source-mode runs (CRI-231)
# execute on it via the operator's CRITERIA_BASE_IMAGE default.
CRITERIA_BASE_IMAGE ?= localhost:5000/criteria-base
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

# Minimal criteria base image (CRI-230): builds criteria from the pinned
# criteria main commit and publishes to the local registry. Without a
# container tool this falls back to the structural regression test, which
# still guards the image contract in CI.
build-criteria-base:
ifeq ($(CONTAINER_TOOL),)
	@echo "No container tool found; running the criteria-base structural test instead of building."
	./k8s/tests/test_criteria_base.sh
else
	$(CONTAINER_TOOL) build --build-arg TARGETARCH=amd64 -f criteria-base/Dockerfile -t $(CRITERIA_BASE_IMAGE):$(BUILD_TAG) criteria-base/
	@echo "Built $(CRITERIA_BASE_IMAGE):$(BUILD_TAG)"
endif

build-criteria-base-push: build-criteria-base
ifneq ($(CONTAINER_TOOL),)
	$(CONTAINER_TOOL) push $(CRITERIA_BASE_IMAGE):$(BUILD_TAG)
endif

# Full docker-run smoke test (build + publish + fetch/apply e2e + pin
# enforcement); needs a container tool and the local registry at
# localhost:5000. See criteria-base/tests/smoke_test.sh.
smoke-criteria-base:
ifeq ($(CONTAINER_TOOL),)
	@echo "No container tool found; the criteria-base smoke test needs docker/podman."
	@exit 1
else
	CONTAINER_TOOL=$(CONTAINER_TOOL) CRITERIA_BASE_IMAGE=$(CRITERIA_BASE_IMAGE) ./criteria-base/tests/smoke_test.sh
endif

# Build all three images with one unique tag.
images: build build-criteria-k8s build-criteria-base

images-push: build-push build-criteria-k8s-push build-criteria-base-push
	@echo "Tag: $(BUILD_TAG)"
	@echo "Deploy the watcher with:"
	@echo "  kubectl -n criteria-jobs set env deploy/criteria-linear-watcher CRITERIA_IMAGE=$(WORKFLOW_IMAGE):$(BUILD_TAG)"
	@echo "  kubectl -n criteria-jobs set image deploy/criteria-k8s-operator operator=$(CRITERIA_K8S_IMAGE):$(BUILD_TAG)"
	@echo "  kubectl -n criteria-jobs set env deploy/criteria-k8s-operator CRITERIA_BASE_IMAGE=$(CRITERIA_BASE_IMAGE):$(BUILD_TAG)"
	@echo "  kubectl -n criteria-jobs set env deploy/criteria-k8s-operator DEFAULT_CRITERIA_IMAGE=$(WORKFLOW_IMAGE):$(BUILD_TAG)"
	@echo "Deploy pairing (CRI-234/CRI-237): the per-(scope,environment) co-location reads"
	@echo "  the environment_type / environment_name keys only from criteria >= fc95449,"
	@echo "  and the wire token delivery reads the accept_token key only from criteria"
	@echo "  >= eae0181, so the operator image and the runner image must roll out together:"
	@echo "  - source-mode runners (criteria-base) build the audited eae0181 commit;"
	@echo "    k8s/tests/test_criteria_base_pin.sh fails the gate on any other pin."
	@echo "  - image-mode runners keep the baked workflow image, built from a released"
	@echo "    criteria tarball; no release contains eae0181 yet, so image-mode must"
	@echo "    NOT roll until its CRITERIA_VERSION pin is re-audited (coordinator)."

deploy-images: images-push
ifneq ($(CONTAINER_TOOL),)
	kubectl -n criteria-jobs set env deploy/criteria-linear-watcher CRITERIA_IMAGE=$(WORKFLOW_IMAGE):$(BUILD_TAG)
	kubectl -n criteria-jobs set image deploy/criteria-k8s-operator operator=$(CRITERIA_K8S_IMAGE):$(BUILD_TAG)
	kubectl -n criteria-jobs set env deploy/criteria-k8s-operator CRITERIA_BASE_IMAGE=$(CRITERIA_BASE_IMAGE):$(BUILD_TAG)
	# Deploy pairing (CRI-234/CRI-237): the per-(scope,environment)
	# co-location needs provision events carrying environment identity, and
	# the wire token delivery needs provision events carrying accept_token;
	# these only exist from criteria runner fc95449 / eae0181 onward. The
	# operator image and the runner image (the operator's --default-image
	# default, used by type=image routes that get no spec.image stamp from
	# the watcher) must be pinned to the same freshly built tag and rolled
	# together. Source-mode runs satisfy the pairing through the
	# criteria-base pin enforced by k8s/tests/test_criteria_base_pin.sh;
	# image-mode runs keep the baked workflow image, whose release-based
	# CRITERIA_VERSION does not contain eae0181 and must not roll until
	# re-audited (coordinator decision).
	kubectl -n criteria-jobs set env deploy/criteria-k8s-operator DEFAULT_CRITERIA_IMAGE=$(WORKFLOW_IMAGE):$(BUILD_TAG)
	kubectl -n criteria-jobs rollout status deploy/criteria-linear-watcher --timeout=120s
	kubectl -n criteria-jobs rollout status deploy/criteria-k8s-operator --timeout=120s
endif

validate:
	/usr/local/bin/criteria validate linear_intake_v1
	/usr/local/bin/criteria validate linear_triage_v1
	/usr/local/bin/criteria validate linear_develop_v1

# CRI-241 + CRI-243: apply the criteria-routes ConfigMap live. Deploy
# ordering for the CRI-243 cutover (the criteria project's routes become
# the split pattern — plan CRI-214 exit condition 3):
#   1. Ensure the Linear state exists, so the triage workflow's re-arm step
#      (CRI-240) finds the state name when it fires:
#        LINEAR_API_KEY=... ./k8s/create-ready-for-development-state.sh
#   2. Rebuild + push all images with a NEW timestamped tag (BUILD_TAG is
#      date+sha, never reused) and roll the operator/watcher together:
#        make deploy-images
#      Builds happen on the HOST (no DinD in pods). The operator/watcher
#      tags must be built from current main: they carry the CRI-242
#      queue-class machinery (operator admission keyed on (repoURL, class),
#      watcher class stamping), and the class=triage object below only
#      behaves as designed on those tags. If a criteria-base build is
#      required, it is built and pushed on the host too and its
#      $(CRITERIA_BASE_IMAGE):$(BUILD_TAG) tag is set on the operator
#      deployment by deploy-images.
#   3. Apply this ConfigMap so the watcher resolves the split routes
#      (criteria-triage: [Triage] -> linear_triage_v1;
#      criteria-develop: [Ready for Development] -> linear_develop_v1):
#        make apply-routes
# Steps 2 and 3 can be run in either order for older payloads, but step 2
# MUST precede this apply for the CRI-243 payload: a pre-CRI-242 watcher
# parses with plain json.Unmarshal and silently drops the class field, so
# triage runs would fall back to the dev queue and serialize per repoURL
# instead of admitting concurrently -- degraded concurrency, not a failure.
# CAUTION: this applies k8s/examples/routes-configmap.yaml WHOLESALE; the
# config source only carries criteria-triage and criteria-develop, so diff
# the live criteria-routes ConfigMap first -- any live-only routes added
# out of band (e.g. the pre-CRI-243 criteria-intake triage wiring) would
# be dropped by this apply.
apply-routes:
	kubectl -n criteria-jobs apply -f k8s/examples/routes-configmap.yaml

test: validate test-criteria-k8s
	@echo "Rendering pod-adapter manifest..."
	./k8s/generate-pod-adapter-manifest.sh
	@echo "Running k8s pod-adapter regression test..."
	./k8s/tests/test_job_cri_27.sh
	@echo "Running runner server-mode TLS opt-out regression test..."
	./k8s/tests/test_runner_server_tls.sh
	@echo "Running k8s template regression test..."
	./k8s/tests/test_launch_template.sh
	@echo "Running per-scope digest discovery regression test (CRI-140)..."
	./k8s/tests/test_per_scope_digest_files.sh
	@echo "Running per-scope adapter wire token delivery regression test (CRI-237)..."
	./k8s/tests/test_per_scope_wire_token.sh
	@echo "Running engine pin regression test (CRI-140)..."
	./linear_intake_v1/tests/test_engine_pin.sh
	@echo "Running linear_triage_v1 standalone extraction regression test (CRI-238)..."
	./linear_triage_v1/tests/test_triage_standalone.sh
	@echo "Running linear_develop_v1 standalone extraction regression test (CRI-239)..."
	./linear_develop_v1/tests/test_develop_standalone.sh
	@echo "Running container-entrypoint token substitution regression test..."
	./k8s/tests/test_container_entrypoint_substitution.sh
	@echo "Running Secrets Store CSI driver / OpenBao provider regression test..."
	./k8s/tests/test_secrets_store_csi.sh
	@echo "Running example manifest regression test..."
	./k8s/tests/test_example_manifest.sh
	@echo "Running criteria-k8s Helm chart regression test..."
	./k8s/tests/test_criteria_k8s_chart.sh
	@echo "Running routes ConfigMap schema regression test (CRI-216)..."
	./k8s/tests/test_routes_config.sh
	@echo "Running Ready for Development Linear state script regression test (CRI-241)..."
	./k8s/tests/test_create_ready_state.sh
	@echo "Running criteria-base image structure regression test (CRI-230)..."
	./k8s/tests/test_criteria_base.sh
	@echo "Running criteria-base entrypoint behavior regression test (CRI-230)..."
	./k8s/tests/test_criteria_base_entrypoint.sh
	@echo "Running criteria-base pin deploy-pairing guard (CRI-234/CRI-237)..."
	./k8s/tests/test_criteria_base_pin.sh

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
		linear_intake_v1/tests/test_engine_pin.sh \
		linear_triage_v1/tests/test_triage_standalone.sh \
		linear_develop_v1/tests/test_develop_standalone.sh \
		k8s/tests/test_per_scope_digest_files.sh \
		k8s/tests/test_per_scope_wire_token.sh \
		k8s/tests/test_runner_server_tls.sh \
		k8s/tests/test_container_entrypoint_substitution.sh \
		k8s/tests/test_secrets_store_csi.sh \
		k8s/tests/test_example_manifest.sh \
		k8s/tests/test_criteria_k8s_chart.sh \
		k8s/tests/test_routes_config.sh \
		k8s/create-ready-for-development-state.sh \
		k8s/tests/test_create_ready_state.sh \
		criteria-base/entrypoint.sh \
		criteria-base/tests/smoke_test.sh \
		k8s/tests/test_criteria_base.sh \
		k8s/tests/test_criteria_base_entrypoint.sh \
		k8s/tests/test_criteria_base_pin.sh

lint-criteria-k8s:
	cd criteria-k8s && go vet ./...