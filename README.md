# Harvester-csi-driver-lvm

Harvester-CSI-Driver-LVM is derived from [metal-stack/csi-driver-lvm](https://github.com/metal-stack/csi-driver-lvm).

## Introduction

Harvester-CSI-Driver-LVM utilizes local storage to provide persistent storage for workloads (Usually VM workloads). It will make the VM unable to be migrated to other nodes, but it can provide better performance.

Before you use it, you should have the pre-established Volume Group (VG) on that node. The VG name will be specified in the StorageClass.

The Harvester-CSI-Driver-LVM provides the following features:
- OnDemand Creation of Logical Volume (LV).
- Support LVM type Striped and DM-Thin.
- Support for Raw Block Volume.
- Support Volume Expansion.
- Support Volume Snapshot.
- Support Volume Clone.
- Support Encryption at Rest (LUKS2 / dm-crypt).

**NOTE**: The Snapshot/Clone feature only works on the same nodes. Clone works for different Volume Groups.

When the first `dm-thin` volume is provisioned in a volume group, the driver
creates a `<vg-name>-thinpool` thin pool using 90% of the free extents, a 512
KiB chunk size, 16 GiB of metadata, and an enabled volume-group metadata spare.
Existing thin pools are used as-is and are not modified by the driver.

### Encryption at Rest

Volumes can be transparently encrypted at rest with LUKS2 (dm-crypt). Set
`encrypted: "true"` on the StorageClass and reference a CSI secret that follows
the same `CRYPTO_KEY_*` convention as Longhorn encrypted volumes — the passphrase
lives in `CRYPTO_KEY_VALUE`. Using the platform's existing encryption-secret
schema means the Harvester admission webhook and UI accept the StorageClass
unchanged:

```yaml
parameters:
  type: dm-thin
  vgName: vg01
  encrypted: "true"
  csi.storage.k8s.io/provisioner-secret-name: ${pvc.name}-luks
  csi.storage.k8s.io/provisioner-secret-namespace: ${pvc.namespace}
  csi.storage.k8s.io/node-publish-secret-name: ${pvc.name}-luks
  csi.storage.k8s.io/node-publish-secret-namespace: ${pvc.namespace}
```

The secret must carry `CRYPTO_KEY_VALUE` (the passphrase); the optional
`CRYPTO_KEY_CIPHER`, `CRYPTO_KEY_HASH`, `CRYPTO_KEY_SIZE` and `CRYPTO_PBKDF`
fields tune `luksFormat` and default to `aes-xts-plain64` / `sha256` / `256` /
`argon2i` (Longhorn's defaults) when omitted.

On first `NodePublishVolume` the logical volume is LUKS2-formatted and opened as
`/dev/mapper/csi-lvm-<volID>`; the filesystem (or raw block bind-mount) is placed
on the mapper so all data on the backing LV is encrypted. The passphrase is fed
to `cryptsetup` over stdin and never appears in the host process list. The
mapping is torn down on `NodeUnpublishVolume` and grown on `NodeExpandVolume`.

See `examples/storageclass-dm-thin-encrypted.yaml`. **Losing the passphrase
makes the data unrecoverable** — manage it with a KMS-backed secret store.

## Installation ##

You can use Helm to install the Harvester-CSI-Driver-LVM by remote repo or local helm chart files.

1. Install the Harvester-CSI-Driver-LVM locally:

```
$ git clone https://github.com/harvester/csi-driver-lvm.git
$ cd csi-driver-lvm/deploy
$ helm install harvester-lvm-csi-driver charts/ -n harvester-system
```

2. Install the Harvester-CSI-Driver-LVM by remote repo:

```
$ helm repo add harvester https://charts.harvesterhci.io
$ helm install harvester/harvester-lvm-csi-driver -n harvester-system
```

After the installation, you can check the status of the following pods:
```
$ kubectl get pods -A |grep harvester-csi-driver-lvm
harvester-system                  harvester-csi-driver-lvm-controller-0                   4/4     Running     0               3h2m
harvester-system                  harvester-csi-driver-lvm-plugin-ctlgp                   3/3     Running     1 (14h ago)     14h
harvester-system                  harvester-csi-driver-lvm-plugin-qxxqs                   3/3     Running     1 (14h ago)     14h
harvester-system                  harvester-csi-driver-lvm-plugin-xktx2                   3/3     Running     0               14h
```

The CSI driver will be installed in the `harvester-system` namespace and provision to each node.

After installation, you can refer to the `examples` directory for some example CRDs for usage.

### Todo ###

* Implement the unittest
* Implement the webhook for the validation

### HowTo Build

```
$ make
```

The above command will execute the validation and build the target Image.
You can define your REPO and TAG with ENV `REPO` and `TAG`.
