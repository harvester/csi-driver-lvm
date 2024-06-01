FROM registry.suse.com/bci/bci-base:15.5

RUN zypper -n rm container-suseconnect && \
    zypper -n install util-linux-systemd lvm2 e2fsprogs nvme-cli device-mapper xfsprogs && \
    zypper -n clean -a && rm -rf /tmp/* /var/tmp/* /usr/share/doc/packages/*

COPY bin/lvmplugin /usr/bin/
ENTRYPOINT ["lvmplugin"]
