.PHONY: build build-capi build-hwaccel require-deploy-config build-deploy build-release verify-version run test test-js test-browser test-race bench lint lint-install vet vulncheck vulncheck-install clean fmt check generate docker-build docker-push docker-build-hwaccel docker-push-hwaccel deploy release-patch release-minor release-major

BINARY := vedetta
BUILD_DIR := ./build
DOCKER_IMAGE := ghcr.io/rvben/vedetta
# Overridable so the release workflow can inject the tag it is building. A
# runner checkout may not carry enough history for git describe to name the
# tag, and a build must never be published under a version it guessed.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags="-X main.Version=$(VERSION)"

# Deployment identity is personal and machine-specific: a signing certificate
# names a developer account, and the host names a private machine. Neither
# belongs in a public repository, so both are read from an untracked
# Makefile.local. Copy deploy/Makefile.local.example to Makefile.local and fill
# in your own values; only the deploy targets read them, so every other target
# works without the file.
-include Makefile.local
CODESIGN_IDENTITY ?= UNSET
CODESIGN_IDENTIFIER ?= UNSET
DEPLOY_HOST ?= UNSET
GOLANGCI_LINT_VERSION := v2.13.1
GOVULNCHECK_VERSION := v1.7.0

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/vedetta

# Fails before doing any work when Makefile.local is missing, so the error
# names the file to create instead of surfacing as a codesign or ssh failure
# further in.
require-deploy-config:
	@missing=""; \
	[ "$(CODESIGN_IDENTITY)" = "UNSET" ] && missing="$$missing CODESIGN_IDENTITY"; \
	[ "$(CODESIGN_IDENTIFIER)" = "UNSET" ] && missing="$$missing CODESIGN_IDENTIFIER"; \
	[ "$(DEPLOY_HOST)" = "UNSET" ] && missing="$$missing DEPLOY_HOST"; \
	if [ -n "$$missing" ]; then \
		echo "deploy needs:$$missing"; \
		echo "copy deploy/Makefile.local.example to Makefile.local and fill in your values"; \
		exit 1; \
	fi; \
	exit 0

build-deploy: require-deploy-config
	GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w -X main.Version=$(VERSION)" -o $(BUILD_DIR)/$(BINARY)-arm64 ./cmd/vedetta
	codesign --force --sign "$(CODESIGN_IDENTITY)" --identifier "$(CODESIGN_IDENTIFIER)" $(BUILD_DIR)/$(BINARY)-arm64

deploy: build-deploy
	scp $(BUILD_DIR)/$(BINARY)-arm64 $(DEPLOY_HOST):/tmp/vedetta-new
	ssh $(DEPLOY_HOST) 'launchctl unload ~/Library/LaunchAgents/com.vedetta.plist 2>/dev/null; \
		sleep 2; \
		rm ~/vedetta/vedetta; \
		mv /tmp/vedetta-new ~/vedetta/vedetta; \
		chmod +x ~/vedetta/vedetta; \
		launchctl load ~/Library/LaunchAgents/com.vedetta.plist'

# Binaries attached to the GitHub release. They carry the same -X main.Version
# as every other build path, so a downloaded binary can name itself in a bug
# report and the update checker can compare it against the latest release.
build-release:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-amd64 ./cmd/vedetta
	GOOS=linux GOARCH=arm64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-linux-arm64 ./cmd/vedetta

# Prove a build reports the version it was built with. A missing -X flag is
# invisible until a user reports "Version dev", and it disables the update
# checker for exactly the people it exists for, so the release gates on this.
# The check compares against the expected version rather than only rejecting
# "dev": matching a specific string cannot pass on a build that reports
# something else wrong, and the second clause keeps it from passing vacuously
# when VERSION itself degraded to "dev".
verify-version: build
	@reported=$$($(BUILD_DIR)/$(BINARY) --version); \
	if [ "$$reported" != "$(VERSION)" ]; then \
		echo "version check failed: binary reports '$$reported', expected '$(VERSION)'"; \
		exit 1; \
	fi; \
	if [ "$$reported" = "dev" ]; then \
		echo "version check failed: binary reports 'dev'"; \
		exit 1; \
	fi; \
	echo "version check passed: $$reported"

build-capi:
	go build -tags cgo_onnxruntime $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/vedetta

# Opt-in Linux hardware decode (VA-API for Intel/AMD, NVDEC for NVIDIA). Both
# backends build against libavcodec/libavutil/libva development libraries only
# (see contrib/setup-hwaccel-ubuntu.sh); NVDEC loads the NVIDIA driver at runtime
# and needs no CUDA toolkit to compile.
build-hwaccel:
	CGO_ENABLED=1 go build -tags hwaccel $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY)-hwaccel ./cmd/vedetta

run: build
	$(BUILD_DIR)/$(BINARY) -config config.example.yml

test: test-js
	go test ./...

# Browser-side pure logic (no DOM) is extracted into standalone modules and
# unit-tested with the Node built-in test runner so it runs locally and in CI
# with no extra toolchain.
test-js:
	node --test internal/api/static/*.test.js

# Real camera-page interaction coverage in mobile Chromium and WebKit. Install
# the pinned browser runtimes once with: npx playwright install chromium webkit
test-browser:
	npm run test:browser

# Race-enabled run of the full Go suite. The detector instruments every memory
# access, so this catches concurrency bugs the plain run cannot - the server
# lifecycle and fan-out paths are only exercised safely under -race. This is the
# CI gate's test step; `make test` stays race-free for a fast local loop.
test-race:
	go test -race ./...

bench:
	go test ./internal/detect/ -bench=. -benchmem -count=1

# Install the linter CI uses. The version is pinned because an unpinned
# "@latest" lets a golangci-lint release turn main red with no change to this
# repository, and makes a local `make lint` disagree with CI about the same
# commit. Bump it deliberately, after running the new version here.
lint-install:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

lint:
	@installed=$$(golangci-lint version --short 2>/dev/null || echo none); \
	if [ "v$$installed" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "warning: golangci-lint v$$installed differs from the pinned $(GOLANGCI_LINT_VERSION) that CI runs; 'make lint-install' to match"; \
	fi
	golangci-lint run ./...

# The standard analyzers, with unsafeptr off.
#
# Three call sites in internal/media hand OpenH264 addresses back to Go as
# uintptr on purpose: the memory belongs to the C library, so storing it in a
# Go pointer type would hand the garbage collector a foreign address to trace.
# vet's unsafeptr analyzer cannot see that the address is not a Go pointer and
# has no per-line suppression, so it reports all three on every run and `go vet`
# has been red on main with nobody able to act on it.
#
# golangci-lint is the source of truth for unsafeptr. It runs the same analyzer
# over the same packages, it honours the //nolint:govet marker beside each
# documented site, and it therefore still fails on a new misuse anywhere in the
# tree. This target covers everything else vet checks.
vet:
	go vet -unsafeptr=false ./...

# Install the vulnerability scanner CI uses. Pinned for the same reason the
# linter is: an unpinned "@latest" lets a scanner release change the verdict on
# an unchanged tree. The vulnerability database is still fetched fresh on every
# run, which is the part that must not be pinned.
vulncheck-install:
	go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)

# Report known vulnerabilities that are reachable from this module's code.
# govulncheck resolves the call graph, so a finding means the vulnerable
# function is actually callable here, not merely present in the dependency
# graph. Run on its own rather than from check: it needs network access to
# fetch the vulnerability database, and its verdict changes when an advisory is
# published rather than when the code changes. CI runs it as a separate job so
# a new advisory is reported without gating a release on it.
vulncheck:
	@if ! command -v govulncheck >/dev/null 2>&1; then \
		echo "govulncheck not found, installing $(GOVULNCHECK_VERSION)"; \
		$(MAKE) vulncheck-install; \
	fi
	govulncheck ./...

clean:
	rm -rf $(BUILD_DIR)
	rm -f vedetta.db

fmt:
	gofmt -w .

generate:
	cd internal/api && oapi-codegen --config oapi-codegen.yaml openapi.yaml

# The local pre-push gate. Deliberately excludes vulncheck: every target here
# runs offline against the code in the working tree, so the gate answers "is my
# change sound" and nothing else. vulncheck depends on a database fetched over
# the network and on advisories published after the code was written, so a
# newly disclosed CVE would redden an unchanged tree and, because the release
# job needs this target, silently block publication.
check: lint vet test-js test-race

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE):$(VERSION) -t $(DOCKER_IMAGE):latest .

docker-push:
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest

# Hardware-accelerated image variant (VA-API + NVDEC, linux/amd64). Includes
# libavcodec and libva, so it is larger than the default static image; pull it
# only on hosts that pass through a GPU (--device /dev/dri or --gpus all).
# Doubles as the CI compile-check for the -tags hwaccel build path.
docker-build-hwaccel:
	docker build --build-arg VERSION=$(VERSION) -f Dockerfile.hwaccel -t $(DOCKER_IMAGE):$(VERSION)-hwaccel -t $(DOCKER_IMAGE):hwaccel .

docker-push-hwaccel:
	docker push $(DOCKER_IMAGE):$(VERSION)-hwaccel
	docker push $(DOCKER_IMAGE):hwaccel

release-patch:
	vership bump patch

release-minor:
	vership bump minor

release-major:
	vership bump major
