.PHONY: build build-capi build-hwaccel build-deploy run test test-js test-browser test-race bench lint clean fmt check generate docker-build docker-push docker-build-hwaccel docker-push-hwaccel deploy release-patch release-minor release-major

BINARY := vedetta
BUILD_DIR := ./build
DOCKER_IMAGE := ghcr.io/rvben/vedetta
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags="-X main.Version=$(VERSION)"
CODESIGN_IDENTITY := Apple Development: ruben@am8.nl (D7C7CMD397)
CODESIGN_IDENTIFIER := nl.am8.vedetta
DEPLOY_HOST := mac-mini

build:
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) ./cmd/vedetta

build-deploy:
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

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR)
	rm -f vedetta.db

fmt:
	gofmt -w .

generate:
	cd internal/api && oapi-codegen --config oapi-codegen.yaml openapi.yaml

check: lint test-js test-race

docker-build:
	docker build -t $(DOCKER_IMAGE):$(VERSION) -t $(DOCKER_IMAGE):latest .

docker-push:
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest

# Hardware-accelerated image variant (VA-API + NVDEC, linux/amd64). Includes
# libavcodec and libva, so it is larger than the default static image; pull it
# only on hosts that pass through a GPU (--device /dev/dri or --gpus all).
# Doubles as the CI compile-check for the -tags hwaccel build path.
docker-build-hwaccel:
	docker build -f Dockerfile.hwaccel -t $(DOCKER_IMAGE):$(VERSION)-hwaccel -t $(DOCKER_IMAGE):hwaccel .

docker-push-hwaccel:
	docker push $(DOCKER_IMAGE):$(VERSION)-hwaccel
	docker push $(DOCKER_IMAGE):hwaccel

release-patch:
	vership bump patch

release-minor:
	vership bump minor

release-major:
	vership bump major
