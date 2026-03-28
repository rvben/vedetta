.PHONY: build build-capi build-deploy run test bench lint clean fmt check docker-build docker-push deploy release-patch release-minor release-major

BINARY := vedetta
BUILD_DIR := ./build
DOCKER_IMAGE := ghcr.io/rvben/vedetta
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
CODESIGN_IDENTITY := Apple Development: ruben@am8.nl (D7C7CMD397)
CODESIGN_IDENTIFIER := nl.am8.vedetta
DEPLOY_HOST := mac-mini

build:
	go build -o $(BUILD_DIR)/$(BINARY) ./cmd/vedetta

build-deploy:
	GOOS=darwin GOARCH=arm64 go build -o $(BUILD_DIR)/$(BINARY)-arm64 -ldflags="-s -w" ./cmd/vedetta
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
	go build -tags cgo_onnxruntime -o $(BUILD_DIR)/$(BINARY) ./cmd/vedetta

run: build
	$(BUILD_DIR)/$(BINARY) -config config.example.yml

test:
	go test ./...

bench:
	go test ./internal/detect/ -bench=. -benchmem -count=1

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR)
	rm -f vedetta.db

fmt:
	gofmt -w .

check: lint test

docker-build:
	docker build -t $(DOCKER_IMAGE):$(VERSION) -t $(DOCKER_IMAGE):latest .

docker-push:
	docker push $(DOCKER_IMAGE):$(VERSION)
	docker push $(DOCKER_IMAGE):latest

release-patch:
	vership bump patch

release-minor:
	vership bump minor

release-major:
	vership bump major
