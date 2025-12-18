#  Copyright 2021 Synology Inc.

REGISTRY_NAME=synology
IMAGE_NAME=synology-csi
IMAGE_VERSION=v1.2.1
IMAGE_TAG=$(REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_VERSION)

# For now, only build linux/amd64 platform
ifeq ($(GOARCH),)
GOARCH:=amd64
endif
GOARM?=""
BUILD_ENV=CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) GOARM=$(GOARM)
BUILD_FLAGS="-s -w -extldflags \"-static\""

.PHONY: all
all: build

.PHONY: FORCE
FORCE: ;

.PHONY: build
build: bin/synology-csi-driver bin/synocli

bin:
	@mkdir -p $@

bin/synology-csi-driver: bin FORCE
	@echo "Compiling $@…"
	@$(BUILD_ENV) go build -v -ldflags $(BUILD_FLAGS) -o $@ ./

.PHONY: docker-build
docker-build:
	docker build -f Dockerfile -t $(IMAGE_TAG) .

.PHONY: docker-build-multiarch
docker-build-multiarch:
	docker buildx build -t $(IMAGE_TAG) --platform linux/amd64,linux/arm/v7,linux/arm64 . --push

bin/synocli: bin FORCE
	@echo "Compiling $@…"
	@$(BUILD_ENV) go build -v -ldflags $(BUILD_FLAGS) -o $@ ./synocli

.PHONY: test
test:
	go clean -testcache
	go test -v ./test/...

.PHONY: clean
clean:
	-rm -rf ./bin

