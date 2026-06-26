ROOT := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))

DOCKER_BUILDKIT := 1
export DOCKER_BUILDKIT

MK_HOST_ARCH ?= $(shell uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
export MK_HOST_ARCH

MK_REPO_ID := $(shell echo -n "$(ROOT)$$(cat /etc/machine-id 2>/dev/null)" | sha256sum | cut -c1-8)
export MK_REPO_ID

MK_DOCKER_PROGRESS ?= plain
export MK_DOCKER_PROGRESS

MK_DOCKER_PULL ?= --pull
export MK_DOCKER_PULL

ifdef CI
  BOLD  :=
  CYAN  :=
  RESET :=
else
  BOLD  := \033[1m
  CYAN  := \033[36m
  RESET := \033[0m
endif

BANNER = @printf "$(BOLD)$(CYAN)[target: $@]$(RESET)\n"

DOCKER_BUILD = docker build $(MK_DOCKER_PULL) \
    --progress=$(MK_DOCKER_PROGRESS) \
    --build-arg MK_REPO_ID \
    --build-arg MK_HOST_ARCH \
    -f $(ROOT)/Dockerfile $(ROOT)

.DEFAULT_GOAL := ci

.PHONY: ci build test validate validate-ci package package-lvmplugin package-lvm-provisioner package-lvm-webhook gen-version-env clean clean-all

# ---- gen-version-env ----
# Pre-generate version env for container builds (no .git needed inside Docker).
# Also handles git worktree checkouts where .git is a pointer file to an external directory.
gen-version-env:
	@bash $(ROOT)/scripts/version > /dev/null

# ---- ci ----
ci: build test validate validate-ci package

# ---- build ----
build: gen-version-env | $(ROOT)/bin
	$(BANNER)
	$(DOCKER_BUILD) --target build-output \
	    --output type=local,dest=$(ROOT)

# ---- test ----
test: gen-version-env
	$(BANNER)
	$(DOCKER_BUILD) --target test

# ---- validate ----
validate: gen-version-env
	$(BANNER)
	$(DOCKER_BUILD) --target validate

# ---- validate-ci ----
validate-ci: gen-version-env
	$(BANNER)
	$(DOCKER_BUILD) --target validate-ci

# ---- package ----
package-lvmplugin:
	$(BANNER)
	ARCH=$(MK_HOST_ARCH) $(ROOT)/scripts/package_lvmplugin

package-lvm-provisioner:
	$(BANNER)
	ARCH=$(MK_HOST_ARCH) $(ROOT)/scripts/package_lvm_provisioner

package-lvm-webhook:
	$(BANNER)
	ARCH=$(MK_HOST_ARCH) $(ROOT)/scripts/package_lvm_webhook

package: package-lvmplugin package-lvm-provisioner package-lvm-webhook

$(ROOT)/bin:
	mkdir -p $@

# ---- clean ----
clean:
	@rm -rf $(ROOT)/bin
	@rm -f $(ROOT)/scripts/.version_env

clean-all: clean
