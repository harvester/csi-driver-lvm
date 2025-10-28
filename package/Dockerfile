# syntax=docker/dockerfile:1.7.0

FROM registry.suse.com/bci/bci-base:15.7

RUN zypper -n rm container-suseconnect && \
    zypper -n install util-linux-systemd lvm2 e2fsprogs nvme-cli device-mapper xfsprogs && \
    zypper -n clean -a && rm -rf /tmp/* /var/tmp/* /usr/share/doc/packages/*

ARG TARGETPLATFORM

RUN if [ "$TARGETPLATFORM" != "linux/amd64" ] && [ "$TARGETPLATFORM" != "linux/arm64" ]; then \
    echo "Error: Unsupported TARGETPLATFORM: $TARGETPLATFORM" && \
    exit 1; \
    fi

ENV ARCH=${TARGETPLATFORM#linux/}

COPY bin/lvmplugin-${ARCH} /usr/bin/lvmplugin
ENTRYPOINT ["lvmplugin"]
