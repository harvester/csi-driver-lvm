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
  csi.storage.k8s.io/provisioner-secret-name: lvm-luks
  csi.storage.k8s.io/provisioner-secret-namespace: default
  csi.storage.k8s.io/node-stage-secret-name: lvm-luks
  csi.storage.k8s.io/node-stage-secret-namespace: default
  csi.storage.k8s.io/node-publish-secret-name: lvm-luks
  csi.storage.k8s.io/node-publish-secret-namespace: default
  csi.storage.k8s.io/node-expand-secret-name: lvm-luks
  csi.storage.k8s.io/node-expand-secret-namespace: default
```

`node-expand-secret-*` is required: expansion resizes the dm-crypt mapper before
the filesystem, so `NodeExpandVolume` needs the passphrase too.
`node-stage-secret-*` is not used by this driver — it does not advertise
`STAGE_UNSTAGE_VOLUME` — but Harvester's StorageClass webhook requires it on an
encrypted class, so set it to the same secret.

On Harvester the secret reference must also be **static**. `${pvc.name}` /
`${pvc.namespace}` templating works on upstream Kubernetes and gives every PVC
its own key, but the Harvester webhook resolves the reference literally when the
StorageClass is admitted and rejects a class whose secret does not already exist
(and one whose `CRYPTO_KEY_*` fields are missing or empty).

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

#### Snapshots, clones and restores

A snapshot or clone is a block-level copy of the source logical volume, so the
copy inherits the source's LUKS header — or its absence. Two consequences:

* **Encryption state cannot be converted by a restore.** Restoring an
  unencrypted source into an `encrypted: "true"` StorageClass, or an encrypted
  source into a plain one, is rejected at `CreateVolume` with
  `InvalidArgument`. The first would LUKS-format restored data and destroy it;
  the second would expose the raw LUKS container as if it were a filesystem.
  The node plugin enforces both rules again at publish time: it never
  LUKS-formats a volume that was restored from a content source, and it refuses
  to publish a filesystem volume whose blocks carry a LUKS header through a
  plain StorageClass (for a raw block volume, where a workload may legitimately
  keep its own LUKS header inside the volume, that probe is limited to
  restores).
* **The restored volume needs the *source's* passphrase.** When the restored
  PVC resolves to a different secret than the source did — a per-PVC templated
  name, or simply a different StorageClass — that secret must hold the source's
  passphrase. A missing credential fails at `CreateVolume`; a wrong one fails at
  `NodePublishVolume` with `FailedPrecondition`. Neither error echoes any secret
  value.

`CreateSnapshot` records the source's encryption state — non-secret metadata
saying only whether the blocks carry a LUKS header — in the snapshot's location
`ConfigMap`, so a restore can still be validated after the source volume is
gone. For a pre-provisioned `VolumeSnapshotContent` created by hand (no such
record exists), declare the state with the
`lvm.driver.harvesterhci.io/encrypted: "true"|"false"` annotation on the
content. Without either, the source state is unknown: restoring into a plain
StorageClass is allowed (the node still refuses to publish a stray LUKS
container), and restoring into an encrypted one is rejected.

Cloning an *unencrypted* source (a VM image, for example) into an encrypted
class is a different operation from a restore: the data has to be written
through the target's dm-crypt mapper. Set
`cdi.harvesterhci.io/storageProfileCloneStrategy: copy` on the StorageClass so
CDI performs a host-assisted copy rather than a block-level clone.

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
