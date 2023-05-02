V := @

PLUGIN_NAME ?= sklyarx/docker-log-driver-telegram
PLUGIN_TAG ?= $(shell git describe --match "v[0-9]*" --abbrev=0 --tags 2>/dev/null || echo "dev")

define build-rootfs
	$(V)rm -rf ./rootfs || true
	$(V)mkdir rootfs || true
	$(V)docker build -t rootfsimage -f Dockerfile.build .

	ID=$$(docker create rootfsimage true) && \
	(docker export $$ID | tar -x -C rootfs) && \
	docker rm -vf $$ID

	$(V)docker rmi rootfsimage -f

	$(V)cp plugin.config.json config.json
endef

build:
	$(V)CGO_ENABLED=0 go build $(GO_FLAGS) -o $@ ./$(@D)

.PHONY: docker-driver
docker-driver: docker-driver-clean
	$(call print-target)

	$(build-rootfs)
	$(V)docker plugin create $(PLUGIN_NAME):$(PLUGIN_TAG) .
	$(V)rm -rf ./rootfs

	$(build-rootfs)
	$(V)docker plugin create $(PLUGIN_NAME):latest .
	$(V)rm -rf ./rootfs

	$(V)rm config.json #

.PHONY: docker-driver-push
docker-driver-push: docker-driver
	$(call print-target)

	$(V)docker plugin push $(PLUGIN_NAME):$(PLUGIN_TAG)
	$(V)docker plugin push $(PLUGIN_NAME):latest

.PHONY: docker-driver-enable
docker-driver-enable:
	$(call print-target)
	$(V)docker plugin enable $(PLUGIN_NAME):$(PLUGIN_TAG)

.PHONY: docker-driver-clean
docker-driver-clean:
	$(call print-target)

	$(V)docker plugin disable $(PLUGIN_NAME):$(PLUGIN_TAG) || true
	$(V)docker plugin rm $(PLUGIN_NAME):$(PLUGIN_TAG) || true

	$(V)docker plugin disable $(PLUGIN_NAME):latest || true
	$(V)docker plugin rm $(PLUGIN_NAME):latest || true

	$(V)rm -rf rootfs
	$(V)rm -f config.json

.PHONY: lint
lint:
	$(call print-target)
	$(V)golangci-lint run

.PHONY: test
test: GO_TEST_FLAGS += -race
test:
	$(call print-target)
	$(V)go test $(GO_TEST_FLAGS) --tags=$(GO_TEST_TAGS) ./...

define print-target
    @printf "Executing target: \033[36m$@\033[0m\n"
endef