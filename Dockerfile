FROM registry.suse.com/bci/golang:1.26 AS builder
ARG TARGETARCH
ENV ARCH=$TARGETARCH
ENV GOTOOLCHAIN=auto

RUN zypper refresh && zypper install -y \
    git \
    curl \
    gzip \
    tar \
    gawk \
    docker \
    wget \
    make \
    binutils \
    gcc \
    glibc-devel-static \
    && zypper clean -a

COPY --from=golangci/golangci-lint:v2.11.4-alpine@sha256:72bcd68512b4e27540dd3a778a1b7afd45759d8145cfb3c089f1d7af53e718e9 /usr/bin/golangci-lint /usr/local/bin/golangci-lint

ENV HOME=/go/src/github.com/harvester/csi-driver-lvm

# ---- base ----
FROM builder AS base
WORKDIR /go/src/github.com/harvester/csi-driver-lvm
COPY . .

# ---- build ----
FROM base AS build
ARG MK_REPO_ID
ARG TARGETARCH
ENV TARGETARCH=$TARGETARCH

RUN --mount=type=cache,target=/go/pkg/mod,id=csi-driver-lvm-go-mod-${MK_REPO_ID} \
    --mount=type=cache,target=/go/src/github.com/harvester/csi-driver-lvm/.cache/go-build,id=csi-driver-lvm-go-build-${MK_REPO_ID} \
    ./scripts/build

FROM scratch AS build-output
COPY --from=build /go/src/github.com/harvester/csi-driver-lvm/bin/ /bin/

# ---- validate ----
FROM base AS validate
ARG MK_REPO_ID

RUN --mount=type=cache,target=/go/pkg/mod,id=csi-driver-lvm-go-mod-${MK_REPO_ID} \
    --mount=type=cache,target=/go/src/github.com/harvester/csi-driver-lvm/.cache/go-build,id=csi-driver-lvm-go-build-${MK_REPO_ID} \
    ./scripts/validate

# ---- validate-ci ----
FROM base AS validate-ci
ARG MK_REPO_ID

RUN git config --global user.email "ci@example.com" && \
    git config --global user.name "ci" && \
    git init 2>/dev/null && git add . && git commit -q -m "commit for validate-ci"

RUN --mount=type=cache,target=/go/pkg/mod,id=csi-driver-lvm-go-mod-${MK_REPO_ID} \
    --mount=type=cache,target=/go/src/github.com/harvester/csi-driver-lvm/.cache/go-build,id=csi-driver-lvm-go-build-${MK_REPO_ID} \
    ./scripts/validate-ci

# ---- test ----
FROM base AS test
ARG MK_REPO_ID

RUN --mount=type=cache,target=/go/pkg/mod,id=csi-driver-lvm-go-mod-${MK_REPO_ID} \
    --mount=type=cache,target=/go/src/github.com/harvester/csi-driver-lvm/.cache/go-build,id=csi-driver-lvm-go-build-${MK_REPO_ID} \
    ./scripts/test
