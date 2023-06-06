#  Copyright 2021 Synology Inc.

REGISTRY_NAME=synology
IMAGE_NAME=synology-csi
IMAGE_VERSION=v1.1.2
IMAGE_TAG=$(REGISTRY_NAME)/$(IMAGE_NAME):$(IMAGE_VERSION)

# For now, only build linux/amd64 platform
ifeq ($(GOARCH),)
GOARCH:=amd64
endif
GOARM?=""
BUILD_ENV=CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) GOARM=$(GOARM)
BUILD_FLAGS="-extldflags \"-static\""

.PHONY: all clean synology-csi-driver synocli test docker-build

all: synology-csi-driver

synology-csi-driver:
	@mkdir -p bin
	$(BUILD_ENV) go build -v -ldflags $(BUILD_FLAGS) -o ./bin/synology-csi-driver ./

docker-build:
	docker build -f Dockerfile -t $(IMAGE_TAG) .

docker-build-multiarch:
	docker buildx build -t $(IMAGE_TAG) --platform linux/amd64,linux/arm/v7,linux/arm64 . --push

synocli:
	@mkdir -p bin
	$(BUILD_ENV) go build -v -ldflags $(BUILD_FLAGS) -o ./bin/synocli ./synocli

test:
	go clean -testcache
	go test -v ./test/...
clean:
	-rm -rf ./bin

