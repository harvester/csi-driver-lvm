/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lvm

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"k8s.io/klog/v2"
)

const (
	// encryptedParam is the StorageClass parameter (propagated into the volume
	// context via buildVolumeContext) that opts a volume into LUKS2 encryption
	// at rest.
	encryptedParam = "encrypted"

	// restoredFromSourceKey is set by the controller in the volume context (and
	// therefore in the PersistentVolume's volumeAttributes) when a volume was
	// populated from a snapshot or volume content source instead of being
	// created empty. The node plugin refuses to LUKS-format such a volume: the
	// restored blocks are real data, and formatting would destroy them.
	restoredFromSourceKey = "restoredFromSource"

	// Longhorn / Kubernetes CSI encryption-secret convention. Harvester's
	// admission webhook (harvester-webhook) validates that any StorageClass
	// referencing a CSI encryption secret carries these fields, exactly as it
	// does for Longhorn encrypted volumes. The LVM driver therefore reads the
	// same schema so an encrypted LVM StorageClass is a drop-in with the
	// platform's existing encrypted-volume workflow and UI. The passphrase
	// lives in CRYPTO_KEY_VALUE; the remaining fields are LUKS2 tuning knobs.
	cryptoKeyValue    = "CRYPTO_KEY_VALUE"    // the passphrase (required)
	cryptoKeyProvider = "CRYPTO_KEY_PROVIDER" // key provider, only "secret" is supported
	cryptoKeyCipher   = "CRYPTO_KEY_CIPHER"   // luksFormat --cipher
	cryptoKeyHash     = "CRYPTO_KEY_HASH"     // luksFormat --hash
	cryptoKeySize     = "CRYPTO_KEY_SIZE"     // luksFormat --key-size
	cryptoPBKDF       = "CRYPTO_PBKDF"        // luksFormat --pbkdf

	// LUKS2 format defaults, matching Longhorn's defaults, applied when the
	// secret omits the optional tuning fields.
	defaultCryptoCipher  = "aes-xts-plain64"
	defaultCryptoHash    = "sha256"
	defaultCryptoKeySize = "256"
	defaultCryptoPBKDF   = "argon2i"

	// cryptMapperPrefix namespaces the dm-crypt mapper devices this driver
	// creates under /dev/mapper so they are easy to identify and never collide
	// with other consumers.
	cryptMapperPrefix = "csi-lvm-"

	// cryptsetup exit codes we care about. See cryptsetup(8) EXIT STATUS.
	cryptExitNotLuks      = 1 // isLuks: device does not carry a LUKS header
	cryptExitNoPermission = 2 // luksOpen: no key available with this passphrase
	cryptExitInactive     = 4 // status: no such active mapping
)

// Restore-safety errors. They are values rather than formatted strings so the
// node server can map them onto FailedPrecondition (an operator has to fix the
// StorageClass or the secret) instead of a generic, retried Internal error.
var (
	// errRestoredVolumeNotLuks fires when an encrypted StorageClass receives a
	// volume restored from an unencrypted source. Formatting here would write a
	// LUKS header over the restored filesystem, so the restore is refused.
	errRestoredVolumeNotLuks = errors.New(
		"volume was restored from an unencrypted source but its StorageClass requests encryption; " +
			"refusing to LUKS-format restored data")

	// errBadPassphrase fires when the supplied secret does not unlock an
	// existing LUKS header. A restored volume keeps the header of its source, so
	// this is the expected failure when the restored PVC references a secret
	// holding a different passphrase.
	errBadPassphrase = errors.New(
		"the encryption secret does not unlock this volume; a volume restored from an encrypted " +
			"snapshot keeps the LUKS header of its source and needs that source's passphrase")
)

// cryptExecutor runs cryptsetup with the passphrase supplied on stdin so it
// never appears in the host process list (argv). It is a package variable so
// unit tests can substitute a fake without shelling out. This mirrors the
// newCommandExecutor pattern used for the LVM commands, but adds stdin support
// which the shared go-common executor does not provide.
type cryptExecutor interface {
	Execute(command string, args []string, stdin string) (string, error)
}

var newCryptExecutor = func() cryptExecutor {
	return &execCryptExecutor{}
}

type execCryptExecutor struct{}

func (e *execCryptExecutor) Execute(command string, args []string, stdin string) (string, error) {
	c := exec.Command(command, args...)
	if stdin != "" {
		// Feed the passphrase over stdin; keeping it out of argv avoids
		// leaking it via /proc/<pid>/cmdline and the host process list.
		c.Stdin = strings.NewReader(stdin)
	}
	var buf bytes.Buffer
	c.Stdout = &buf
	c.Stderr = &buf
	err := c.Run()
	out := buf.String()
	if err != nil {
		// Wrap with %w so commandExitCode can recover the exec.ExitError and
		// its ExitCode(). args never contain the passphrase, so logging them is
		// safe.
		return out, fmt.Errorf("command %s %v failed: %w", command, args, err)
	}
	return out, nil
}

// isEncrypted reports whether the volume context opts into encryption at rest.
func isEncrypted(volumeContext map[string]string) bool {
	value, ok := volumeContext[encryptedParam]
	if !ok {
		return false
	}
	enabled, err := strconv.ParseBool(value)
	return err == nil && enabled
}

// isRestoredFromSource reports whether the volume was populated from a snapshot
// or volume content source. Only the controller sets the flag, so a missing or
// malformed value is treated as "created empty".
func isRestoredFromSource(volumeContext map[string]string) bool {
	restored, err := strconv.ParseBool(volumeContext[restoredFromSourceKey])
	return err == nil && restored
}

// luks2HeaderBytes is the space cryptsetup's default LUKS2 format reserves ahead
// of the data payload for the header and keyslot area (the default data offset
// is 16 MiB). The dm-crypt mapper therefore exposes 16 MiB less than its backing
// block device. We never pass --offset to luksFormat, so this default always
// applies.
const luks2HeaderBytes int64 = 16 * 1024 * 1024

// backingLVBytes returns the backing LV size needed to expose usableBytes of
// (decrypted) capacity. For encrypted volumes that is usableBytes plus the LUKS2
// header overhead, so the requested capacity is honored end-to-end (e.g. a 10Gi
// encrypted PVC yields a full 10Gi usable device, which exact-fit consumers such
// as CDI/KubeVirt image imports require). For plain volumes it is unchanged.
func backingLVBytes(usableBytes int64, encrypted bool) int64 {
	if encrypted {
		return usableBytes + luks2HeaderBytes
	}
	return usableBytes
}

// cryptMapperName is the dm-crypt mapping name for a volume.
func cryptMapperName(volID string) string {
	return cryptMapperPrefix + volID
}

// cryptMapperPath is the /dev/mapper path of the opened dm-crypt device.
func cryptMapperPath(volID string) string {
	return "/dev/mapper/" + cryptMapperName(volID)
}

// mapperExists reports whether an open dm-crypt mapping node exists for the
// volume. It is a cheap filesystem check that lets the unpublish/expand paths
// (which have no volume context) skip cryptsetup entirely for plain,
// non-encrypted volumes — the mapper only exists while a LUKS device is open.
func mapperExists(volID string) bool {
	_, err := os.Stat(cryptMapperPath(volID))
	return err == nil
}

// cryptoParams captures the LUKS2 tuning read from a CRYPTO_KEY_* encryption
// secret. Only the passphrase is required; the rest fall back to Longhorn's
// defaults so a minimal secret still produces a Longhorn-compatible LUKS device.
type cryptoParams struct {
	passphrase string
	cipher     string
	hash       string
	keySize    string
	pbkdf      string
}

// extractCryptoParams pulls the passphrase and LUKS tuning out of the CSI
// encryption secret, following the Longhorn CRYPTO_KEY_* convention that the
// Harvester webhook enforces.
func extractCryptoParams(secrets map[string]string) (*cryptoParams, error) {
	passphrase := secrets[cryptoKeyValue]
	if passphrase == "" {
		return nil, fmt.Errorf(
			"encrypted volume requires a non-empty %q entry in the encryption secret",
			cryptoKeyValue,
		)
	}
	return &cryptoParams{
		passphrase: passphrase,
		cipher:     valueOrDefault(secrets[cryptoKeyCipher], defaultCryptoCipher),
		hash:       valueOrDefault(secrets[cryptoKeyHash], defaultCryptoHash),
		keySize:    valueOrDefault(secrets[cryptoKeySize], defaultCryptoKeySize),
		pbkdf:      valueOrDefault(secrets[cryptoPBKDF], defaultCryptoPBKDF),
	}, nil
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// openEncryptedDevice ensures the block device at devicePath carries a LUKS2
// header and opens it, returning the resulting /dev/mapper path to be mounted
// or bind-mounted. It is idempotent: a device that is already open is reused,
// so repeated NodePublishVolume calls are safe.
//
// allowFormat must be false for volumes restored from a snapshot or volume
// content source. Those already hold data, so a missing LUKS header means the
// source was unencrypted and formatting would destroy the restored content;
// openEncryptedDevice fails with errRestoredVolumeNotLuks instead.
func openEncryptedDevice(devicePath, volID string, params *cryptoParams, allowFormat bool) (string, error) {
	executor := newCryptExecutor()
	mapperName := cryptMapperName(volID)
	mapperPath := cryptMapperPath(volID)

	// Idempotent re-publish: if the mapping is already open, reuse it.
	if mapperExists(volID) {
		klog.Infof("dm-crypt device %s already open, reusing", mapperName)
		return mapperPath, nil
	}

	formatted, err := isLuks(executor, devicePath)
	if err != nil {
		return "", err
	}
	if !formatted {
		if !allowFormat {
			return "", fmt.Errorf("%w (device %s)", errRestoredVolumeNotLuks, devicePath)
		}
		klog.Infof("formatting %s as LUKS2 for encrypted volume %s", devicePath, volID)
		if out, err := luksFormat(executor, devicePath, params); err != nil {
			return "", fmt.Errorf("unable to LUKS-format %s: %w output:%s", devicePath, err, out)
		}
	}

	if out, err := luksOpen(executor, devicePath, mapperName, params.passphrase); err != nil {
		if code, ok := commandExitCode(err); ok && code == cryptExitNoPermission {
			// Deliberately drops cryptsetup's output: it adds nothing beyond
			// "No key available with this passphrase" and keeps the error clean.
			return "", fmt.Errorf("%w (device %s)", errBadPassphrase, devicePath)
		}
		return "", fmt.Errorf("unable to open LUKS device %s: %w output:%s", devicePath, err, out)
	}
	klog.Infof("opened dm-crypt device %s for volume %s", mapperName, volID)
	return mapperPath, nil
}

// luksHeaderPresent reports whether a block device already carries a LUKS
// header. Used to keep a volume restored from an encrypted source from being
// handed to a workload as a raw, still-locked LUKS container.
func luksHeaderPresent(devicePath string) (bool, error) {
	return isLuks(newCryptExecutor(), devicePath)
}

// closeEncryptedDevice tears down the dm-crypt mapping for a volume. It is
// idempotent and needs no passphrase: an already-closed (or never-encrypted)
// volume is a no-op. The caller must unmount the mapper first.
func closeEncryptedDevice(volID string) error {
	if !mapperExists(volID) {
		return nil
	}
	mapperName := cryptMapperName(volID)
	if out, err := luksClose(newCryptExecutor(), mapperName); err != nil {
		return fmt.Errorf("unable to close LUKS device %s: %w output:%s", mapperName, err, out)
	}
	klog.Infof("closed dm-crypt device %s for volume %s", mapperName, volID)
	return nil
}

// resizeEncryptedDevice grows the dm-crypt mapping to match the (already
// extended) backing LV. LUKS2 re-derives the volume key from a keyslot on
// resize unless the key is available in an accessible kernel keyring; in the
// CSI node plugin's mount namespace it is not, so cryptsetup would otherwise
// block on an interactive passphrase prompt. The passphrase is therefore fed
// on stdin (via --key-file -). Returns whether the mapping was active.
func resizeEncryptedDevice(volID, passphrase string) (bool, string, error) {
	if !mapperExists(volID) {
		return false, "", nil
	}
	mapperName := cryptMapperName(volID)
	out, err := luksResize(newCryptExecutor(), mapperName, passphrase)
	if err != nil {
		return true, out, fmt.Errorf("unable to resize LUKS device %s: %w output:%s", mapperName, err, out)
	}
	return true, out, nil
}

// encryptedVolumeActive reports whether an open dm-crypt mapping exists for the
// volume. Used by paths (expand, unpublish) that receive no volume context.
func encryptedVolumeActive(volID string) (bool, error) {
	if !mapperExists(volID) {
		return false, nil
	}
	return luksStatus(newCryptExecutor(), cryptMapperName(volID))
}

func isLuks(executor cryptExecutor, devicePath string) (bool, error) {
	_, err := executor.Execute("cryptsetup", []string{"isLuks", devicePath}, "")
	if err == nil {
		return true, nil
	}
	if code, ok := commandExitCode(err); ok && code == cryptExitNotLuks {
		return false, nil
	}
	return false, fmt.Errorf("unable to probe LUKS header on %s: %w", devicePath, err)
}

func luksStatus(executor cryptExecutor, mapperName string) (bool, error) {
	_, err := executor.Execute("cryptsetup", []string{"status", mapperName}, "")
	if err == nil {
		return true, nil
	}
	if code, ok := commandExitCode(err); ok && code == cryptExitInactive {
		return false, nil
	}
	return false, fmt.Errorf("unable to query status of dm-crypt device %s: %w", mapperName, err)
}

func luksFormat(executor cryptExecutor, devicePath string, params *cryptoParams) (string, error) {
	return executor.Execute(
		"cryptsetup",
		[]string{
			"luksFormat", "--type", "luks2",
			"--cipher", params.cipher,
			"--hash", params.hash,
			"--key-size", params.keySize,
			"--pbkdf", params.pbkdf,
			"--batch-mode", devicePath,
		},
		params.passphrase,
	)
}

func luksOpen(executor cryptExecutor, devicePath, mapperName, passphrase string) (string, error) {
	return executor.Execute("cryptsetup", []string{"luksOpen", devicePath, mapperName}, passphrase)
}

func luksClose(executor cryptExecutor, mapperName string) (string, error) {
	return executor.Execute("cryptsetup", []string{"luksClose", mapperName}, "")
}

func luksResize(executor cryptExecutor, mapperName, passphrase string) (string, error) {
	// Read the passphrase from stdin (--key-file -) so cryptsetup can unlock the
	// keyslot non-interactively; keeping it off argv avoids leaking it via ps.
	return executor.Execute("cryptsetup", []string{"resize", "--key-file", "-", mapperName}, passphrase)
}
