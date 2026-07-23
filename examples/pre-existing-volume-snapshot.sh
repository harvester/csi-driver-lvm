#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat <<EOF
Usage:
  $0 <node-name> <vg-name>

Environment variables:
  NAMESPACE            Kubernetes namespace for demo objects. Default: default
  DRIVER_NAMESPACE     Namespace where the LVM CSI driver is installed. Default: harvester-system
  DRIVER_NAME          CSI provisioner/driver name. Default: lvm.driver.harvesterhci.io
  TIMEOUT_SECONDS      Wait timeout in seconds. Default: 180
  RUN_ID               Suffix for generated resource names. Default: current unix time
  KEEP_DEMO_RESOURCES  Set to 1 to keep source PVC/classes after success.
EOF
}

if [[ $# -ne 2 ]]; then
	usage
	exit 1
fi

NODE_NAME=$1
VG_NAME=$2

NAMESPACE=${NAMESPACE:-default}
DRIVER_NAMESPACE=${DRIVER_NAMESPACE:-harvester-system}
DRIVER_NAME=${DRIVER_NAME:-lvm.driver.harvesterhci.io}
TIMEOUT_SECONDS=${TIMEOUT_SECONDS:-180}
RUN_ID=${RUN_ID:-$(date +%s)}

APP_LABEL=lvm-pre-existing-snapshot-demo
SC_NAME="lvm-pre-existing-demo-${RUN_ID}"
RETAIN_CLASS="lvm-snap-retain-${RUN_ID}"
DELETE_CLASS="lvm-snap-delete-${RUN_ID}"
PVC_NAME="pre-existing-source-pvc-${RUN_ID}"
SOURCE_SNAPSHOT="pre-existing-source-snapshot-${RUN_ID}"
IMPORT_CONTENT="pre-existing-import-content-${RUN_ID}"
IMPORT_SNAPSHOT="pre-existing-import-${RUN_ID}"

log() {
	printf '\n==> %s\n' "$*"
}

if ! command -v kubectl >/dev/null 2>&1; then
	printf 'missing required command: kubectl\n' >&2
	exit 1
fi

wait_jsonpath() {
	local kind=$1
	local name=$2
	local path=$3
	local want=$4
	local value

	for _ in $(seq 1 "$TIMEOUT_SECONDS"); do
		value=$(kubectl -n "$NAMESPACE" get "$kind" "$name" -o "jsonpath=${path}" 2>/dev/null || true)
		if [[ "$value" == "$want" ]]; then
			return 0
		fi
		sleep 1
	done

	printf 'timed out waiting for %s/%s jsonpath %s to be %s\n' "$kind" "$name" "$path" "$want" >&2
	return 1
}

wait_jsonpath_nonempty() {
	local kind=$1
	local name=$2
	local path=$3
	local value

	for _ in $(seq 1 "$TIMEOUT_SECONDS"); do
		value=$(kubectl -n "$NAMESPACE" get "$kind" "$name" -o "jsonpath=${path}" 2>/dev/null || true)
		if [[ -n "$value" ]]; then
			printf '%s' "$value"
			return 0
		fi
		sleep 1
	done

	printf 'timed out waiting for %s/%s jsonpath %s\n' "$kind" "$name" "$path" >&2
	return 1
}

wait_cluster_jsonpath_nonempty() {
	local kind=$1
	local name=$2
	local path=$3
	local value

	for _ in $(seq 1 "$TIMEOUT_SECONDS"); do
		value=$(kubectl get "$kind" "$name" -o "jsonpath=${path}" 2>/dev/null || true)
		if [[ -n "$value" ]]; then
			printf '%s' "$value"
			return 0
		fi
		sleep 1
	done

	printf 'timed out waiting for %s/%s jsonpath %s\n' "$kind" "$name" "$path" >&2
	return 1
}

wait_namespaced_deleted() {
	local kind=$1
	local name=$2

	for _ in $(seq 1 "$TIMEOUT_SECONDS"); do
		if ! kubectl -n "$NAMESPACE" get "$kind" "$name" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done

	printf 'timed out waiting for %s/%s to be deleted\n' "$kind" "$name" >&2
	return 1
}

dump_snapshot_debug() {
	local content

	content=$(kubectl -n "$NAMESPACE" get volumesnapshot "$SOURCE_SNAPSHOT" -o jsonpath='{.status.boundVolumeSnapshotContentName}' 2>/dev/null || true)

	cat <<EOF

Snapshot did not become ready. Debug data:

kubectl -n ${NAMESPACE} describe volumesnapshot ${SOURCE_SNAPSHOT}
kubectl -n ${NAMESPACE} get volumesnapshot ${SOURCE_SNAPSHOT} -o yaml
EOF

	kubectl -n "$NAMESPACE" describe volumesnapshot "$SOURCE_SNAPSHOT" || true
	kubectl -n "$NAMESPACE" get volumesnapshot "$SOURCE_SNAPSHOT" -o yaml || true

	if [[ -n "$content" ]]; then
		cat <<EOF

kubectl describe volumesnapshotcontent ${content}
kubectl get volumesnapshotcontent ${content} -o yaml
EOF
		kubectl describe volumesnapshotcontent "$content" || true
		kubectl get volumesnapshotcontent "$content" -o yaml || true
	fi

	cat <<EOF

Recent CSI snapshot helper pods:
EOF
	kubectl -n "$DRIVER_NAMESPACE" get pods | grep -E 'snap-create|snap-delete|lvm-create|lvm-delete' || true

	cat <<EOF

Recent csi-snapshotter logs:
EOF
	kubectl -n "$DRIVER_NAMESPACE" logs statefulset/harvester-csi-driver-lvm-controller -c csi-snapshotter --tail=120 || true
}

print_cleanup_hint() {
	cat <<EOF

The demo did not finish. To clean Kubernetes demo objects for RUN_ID=${RUN_ID}:

kubectl -n ${NAMESPACE} delete pvc ${PVC_NAME} --ignore-not-found
kubectl -n ${NAMESPACE} delete volumesnapshot ${SOURCE_SNAPSHOT} ${IMPORT_SNAPSHOT} --ignore-not-found
kubectl delete volumesnapshotcontent ${SOURCE_CONTENT:-<source-content>} ${IMPORT_CONTENT} --ignore-not-found
kubectl delete volumesnapshotclass ${RETAIN_CLASS} ${DELETE_CLASS} --ignore-not-found
kubectl delete storageclass ${SC_NAME} --ignore-not-found
EOF

	if [[ -n "${LV_PATH:-}" ]]; then
		cat <<EOF

If the retained backend LV snapshot is still present and no longer has a VolumeSnapshotContent:

ssh ${NODE_NAME} sudo lvremove -y /dev/${LV_PATH}
EOF
	fi
}

on_exit() {
	local rc=$?

	if [[ "$rc" -ne 0 ]]; then
		print_cleanup_hint >&2
	fi
	exit "$rc"
}

cleanup_success() {
	if [[ "${KEEP_DEMO_RESOURCES:-0}" == "1" ]]; then
		return
	fi

	log "Cleaning up demo source resources"
	kubectl -n "$NAMESPACE" delete pvc "$PVC_NAME" --ignore-not-found >/dev/null
	kubectl delete volumesnapshotclass "$RETAIN_CLASS" --ignore-not-found >/dev/null
	kubectl delete volumesnapshotclass "$DELETE_CLASS" --ignore-not-found >/dev/null
	kubectl delete storageclass "$SC_NAME" --ignore-not-found >/dev/null
}

trap on_exit EXIT

log "Using namespace=${NAMESPACE}, node=${NODE_NAME}, vg=${VG_NAME}"

log "Creating source StorageClass, Retain snapshot class, and PVC"
kubectl apply -f - <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ${SC_NAME}
  labels:
    app.kubernetes.io/name: ${APP_LABEL}
parameters:
  type: dm-thin
  vgName: ${VG_NAME}
provisioner: ${DRIVER_NAME}
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowedTopologies:
- matchLabelExpressions:
  - key: topology.lvm.csi/node
    values:
    - ${NODE_NAME}
---
apiVersion: snapshot.storage.k8s.io/v1
deletionPolicy: Retain
driver: ${DRIVER_NAME}
kind: VolumeSnapshotClass
metadata:
  name: ${RETAIN_CLASS}
  labels:
    app.kubernetes.io/name: ${APP_LABEL}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: ${APP_LABEL}
spec:
  storageClassName: ${SC_NAME}
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  volumeMode: Block
EOF

log "Waiting for source PVC to bind"
wait_jsonpath persistentvolumeclaim "$PVC_NAME" '{.status.phase}' Bound

log "Creating CSI snapshot from PVC"
kubectl apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: ${SOURCE_SNAPSHOT}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: ${APP_LABEL}
spec:
  volumeSnapshotClassName: ${RETAIN_CLASS}
  source:
    persistentVolumeClaimName: ${PVC_NAME}
EOF

log "Waiting for source snapshot"
if ! wait_jsonpath volumesnapshot "$SOURCE_SNAPSHOT" '{.status.readyToUse}' true; then
	dump_snapshot_debug
	exit 1
fi

PV_NAME=$(kubectl -n "$NAMESPACE" get pvc "$PVC_NAME" -o jsonpath='{.spec.volumeName}')
SOURCE_CONTENT=$(wait_jsonpath_nonempty volumesnapshot "$SOURCE_SNAPSHOT" '{.status.boundVolumeSnapshotContentName}')
SNAPSHOT_HANDLE=$(wait_cluster_jsonpath_nonempty volumesnapshotcontent "$SOURCE_CONTENT" '{.status.snapshotHandle}')
LV_PATH="${VG_NAME}/lvm-${SNAPSHOT_HANDLE}"

log "Discovered values"
printf 'PV_NAME=%s\nSOURCE_CONTENT=%s\nSNAPSHOT_HANDLE=%s\nLV_PATH=%s\n' \
	"$PV_NAME" "$SOURCE_CONTENT" "$SNAPSHOT_HANDLE" "$LV_PATH"

log "Optional manual backend check"
printf 'ssh %s sudo lvs %s\n' "$NODE_NAME" "$LV_PATH"

log "Deleting source VolumeSnapshot with Retain policy"
kubectl -n "$NAMESPACE" delete volumesnapshot "$SOURCE_SNAPSHOT" --wait=false

log "Waiting for source VolumeSnapshot to be fully removed"
wait_namespaced_deleted volumesnapshot "$SOURCE_SNAPSHOT"

log "Source snapshot deleted with Retain policy"
printf 'Optional check that the backend snapshot is retained: ssh %s sudo lvs %s\n' "$NODE_NAME" "$LV_PATH"

log "Importing retained LV snapshot as a pre-existing VolumeSnapshotContent"
kubectl apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
deletionPolicy: Delete
driver: ${DRIVER_NAME}
kind: VolumeSnapshotClass
metadata:
  name: ${DELETE_CLASS}
  labels:
    app.kubernetes.io/name: ${APP_LABEL}
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotContent
metadata:
  name: ${IMPORT_CONTENT}
  labels:
    app.kubernetes.io/name: ${APP_LABEL}
  annotations:
    lvm.driver.harvesterhci.io/nodeName: ${NODE_NAME}
    lvm.driver.harvesterhci.io/vgName: ${VG_NAME}
spec:
  deletionPolicy: Delete
  driver: ${DRIVER_NAME}
  source:
    snapshotHandle: ${SNAPSHOT_HANDLE}
  volumeSnapshotClassName: ${DELETE_CLASS}
  volumeSnapshotRef:
    name: ${IMPORT_SNAPSHOT}
    namespace: ${NAMESPACE}
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: ${IMPORT_SNAPSHOT}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/name: ${APP_LABEL}
spec:
  source:
    volumeSnapshotContentName: ${IMPORT_CONTENT}
  volumeSnapshotClassName: ${DELETE_CLASS}
EOF

log "Deleting imported snapshot and waiting for imported content deletion"
kubectl -n "$NAMESPACE" delete volumesnapshot "$IMPORT_SNAPSHOT" --wait=true --timeout="${TIMEOUT_SECONDS}s"
kubectl wait --for=delete "volumesnapshotcontent/${IMPORT_CONTENT}" --timeout="${TIMEOUT_SECONDS}s"

log "Imported snapshot deleted"
printf 'Optional check that the backend snapshot was removed: ssh %s sudo lvs %s\n' "$NODE_NAME" "$LV_PATH"

log "Deleting retained source VolumeSnapshotContent"
kubectl delete volumesnapshotcontent "$SOURCE_CONTENT" --ignore-not-found --wait=true --timeout="${TIMEOUT_SECONDS}s"

cleanup_success

log "Done"
