package lvm

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type cryptCall struct {
	command string
	args    []string
	stdin   string
}

type cryptResult struct {
	output string
	err    error
}

type fakeCryptExecutor struct {
	t       *testing.T
	results []cryptResult
	calls   []cryptCall
}

func (f *fakeCryptExecutor) Execute(command string, args []string, stdin string) (string, error) {
	f.t.Helper()
	f.calls = append(f.calls, cryptCall{command: command, args: append([]string(nil), args...), stdin: stdin})
	if len(f.results) == 0 {
		f.t.Fatalf("unexpected crypt command: %s %v", command, args)
	}
	result := f.results[0]
	f.results = f.results[1:]
	return result.output, result.err
}

func useFakeCryptExecutor(t *testing.T, fake *fakeCryptExecutor) {
	t.Helper()
	original := newCryptExecutor
	newCryptExecutor = func() cryptExecutor {
		return fake
	}
	t.Cleanup(func() {
		newCryptExecutor = original
	})
}

func TestIsEncrypted(t *testing.T) {
	cases := []struct {
		name    string
		context map[string]string
		want    bool
	}{
		{"absent", map[string]string{}, false},
		{"true", map[string]string{encryptedParam: "true"}, true},
		{"one", map[string]string{encryptedParam: "1"}, true},
		{"false", map[string]string{encryptedParam: "false"}, false},
		{"garbage", map[string]string{encryptedParam: "yesplease"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEncrypted(tc.context); got != tc.want {
				t.Fatalf("isEncrypted(%v)=%t, want %t", tc.context, got, tc.want)
			}
		})
	}
}

func TestExtractCryptoParams(t *testing.T) {
	if _, err := extractCryptoParams(map[string]string{}); err == nil {
		t.Fatal("expected error for missing passphrase")
	}
	if _, err := extractCryptoParams(map[string]string{cryptoKeyValue: ""}); err == nil {
		t.Fatal("expected error for empty passphrase")
	}

	// Minimal secret: passphrase only -> Longhorn defaults fill the rest.
	got, err := extractCryptoParams(map[string]string{cryptoKeyValue: "hunter2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.passphrase != "hunter2" {
		t.Fatalf("passphrase = %q, want %q", got.passphrase, "hunter2")
	}
	if got.cipher != defaultCryptoCipher || got.hash != defaultCryptoHash ||
		got.keySize != defaultCryptoKeySize || got.pbkdf != defaultCryptoPBKDF {
		t.Fatalf("defaults not applied: %+v", got)
	}

	// Full secret: explicit tuning is honored.
	full, err := extractCryptoParams(map[string]string{
		cryptoKeyValue:  "pw",
		cryptoKeyCipher: "aes-cbc-essiv:sha256",
		cryptoKeyHash:   "sha512",
		cryptoKeySize:   "512",
		cryptoPBKDF:     "pbkdf2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if full.cipher != "aes-cbc-essiv:sha256" || full.hash != "sha512" ||
		full.keySize != "512" || full.pbkdf != "pbkdf2" {
		t.Fatalf("explicit tuning not honored: %+v", full)
	}
}

func TestCryptMapperNaming(t *testing.T) {
	if got := cryptMapperName("abc"); got != "csi-lvm-abc" {
		t.Fatalf("cryptMapperName = %q", got)
	}
	if got := cryptMapperPath("abc"); got != "/dev/mapper/csi-lvm-abc" {
		t.Fatalf("cryptMapperPath = %q", got)
	}
}

func TestIsLuks(t *testing.T) {
	t.Run("is luks", func(t *testing.T) {
		fake := &fakeCryptExecutor{t: t, results: []cryptResult{{}}}
		ok, err := isLuks(fake, "/dev/vg/lv")
		if err != nil || !ok {
			t.Fatalf("expected (true,nil), got (%t,%v)", ok, err)
		}
	})
	t.Run("not luks", func(t *testing.T) {
		fake := &fakeCryptExecutor{t: t, results: []cryptResult{{err: commandExitError{code: cryptExitNotLuks}}}}
		ok, err := isLuks(fake, "/dev/vg/lv")
		if err != nil || ok {
			t.Fatalf("expected (false,nil), got (%t,%v)", ok, err)
		}
	})
	t.Run("device error", func(t *testing.T) {
		fake := &fakeCryptExecutor{t: t, results: []cryptResult{{err: commandExitError{code: 4}}}}
		if _, err := isLuks(fake, "/dev/vg/lv"); err == nil {
			t.Fatal("expected error for non-1 exit code")
		}
	})
}

func TestLuksStatus(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		fake := &fakeCryptExecutor{t: t, results: []cryptResult{{}}}
		ok, err := luksStatus(fake, "csi-lvm-x")
		if err != nil || !ok {
			t.Fatalf("expected (true,nil), got (%t,%v)", ok, err)
		}
	})
	t.Run("inactive", func(t *testing.T) {
		fake := &fakeCryptExecutor{t: t, results: []cryptResult{{err: commandExitError{code: cryptExitInactive}}}}
		ok, err := luksStatus(fake, "csi-lvm-x")
		if err != nil || ok {
			t.Fatalf("expected (false,nil), got (%t,%v)", ok, err)
		}
	})
	t.Run("other error", func(t *testing.T) {
		fake := &fakeCryptExecutor{t: t, results: []cryptResult{{err: errors.New("boom")}}}
		if _, err := luksStatus(fake, "csi-lvm-x"); err == nil {
			t.Fatal("expected error")
		}
	})
}

// openEncryptedDevice on a fresh (never-formatted) device should probe, format
// with LUKS2, then open — and the passphrase must travel over stdin, never argv.
func TestOpenEncryptedDeviceFormatsWhenNotLuks(t *testing.T) {
	const volID = "unit-open-fresh" // no /dev/mapper node exists for this in the test env
	const passphrase = "s3cr3t-pass"
	fake := &fakeCryptExecutor{
		t: t,
		results: []cryptResult{
			{err: commandExitError{code: cryptExitNotLuks}}, // isLuks -> not luks
			{}, // luksFormat
			{}, // luksOpen
		},
	}
	useFakeCryptExecutor(t, fake)

	params := &cryptoParams{
		passphrase: passphrase,
		cipher:     defaultCryptoCipher,
		hash:       defaultCryptoHash,
		keySize:    defaultCryptoKeySize,
		pbkdf:      defaultCryptoPBKDF,
	}
	mapperPath, err := openEncryptedDevice("/dev/vg/"+volID, volID, params, true)
	if err != nil {
		t.Fatalf("openEncryptedDevice failed: %v", err)
	}
	if want := cryptMapperPath(volID); mapperPath != want {
		t.Fatalf("mapperPath = %q, want %q", mapperPath, want)
	}
	if len(fake.calls) != 3 {
		t.Fatalf("expected 3 cryptsetup calls, got %d: %#v", len(fake.calls), fake.calls)
	}
	assertCryptSubcommand(t, fake.calls[0], "isLuks", "")
	assertCryptSubcommand(t, fake.calls[1], "luksFormat", passphrase)
	assertCryptSubcommand(t, fake.calls[2], "luksOpen", passphrase)

	// The passphrase must never appear in argv.
	for _, call := range fake.calls {
		for _, arg := range call.args {
			if strings.Contains(arg, passphrase) {
				t.Fatalf("passphrase leaked into argv: %v", call.args)
			}
		}
	}
	// luksFormat must request LUKS2 in batch mode with the Longhorn-style tuning.
	wantFormat := []string{
		"luksFormat", "--type", "luks2",
		"--cipher", defaultCryptoCipher,
		"--hash", defaultCryptoHash,
		"--key-size", defaultCryptoKeySize,
		"--pbkdf", defaultCryptoPBKDF,
		"--batch-mode", "/dev/vg/" + volID,
	}
	if !reflect.DeepEqual(fake.calls[1].args, wantFormat) {
		t.Fatalf("unexpected luksFormat args: %v", fake.calls[1].args)
	}
}

// A device that already carries a LUKS header must be opened, not reformatted.
func TestOpenEncryptedDeviceSkipsFormatWhenLuks(t *testing.T) {
	const volID = "unit-open-existing"
	fake := &fakeCryptExecutor{
		t: t,
		results: []cryptResult{
			{}, // isLuks -> is luks
			{}, // luksOpen
		},
	}
	useFakeCryptExecutor(t, fake)

	if _, err := openEncryptedDevice("/dev/vg/"+volID, volID, &cryptoParams{passphrase: "pw"}, true); err != nil {
		t.Fatalf("openEncryptedDevice failed: %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("expected 2 calls (isLuks, luksOpen), got %#v", fake.calls)
	}
	assertCryptSubcommand(t, fake.calls[0], "isLuks", "")
	assertCryptSubcommand(t, fake.calls[1], "luksOpen", "pw")
}

// A volume restored from an unencrypted source carries real data but no LUKS
// header. Formatting it would destroy that data, so the open must fail instead.
func TestOpenEncryptedDeviceRefusesToFormatRestoredVolume(t *testing.T) {
	const volID = "unit-open-restored-plain"
	fake := &fakeCryptExecutor{
		t:       t,
		results: []cryptResult{{err: commandExitError{code: cryptExitNotLuks}}}, // isLuks -> not luks
	}
	useFakeCryptExecutor(t, fake)

	_, err := openEncryptedDevice("/dev/vg/"+volID, volID, &cryptoParams{passphrase: "pw"}, false)
	if !errors.Is(err, errRestoredVolumeNotLuks) {
		t.Fatalf("expected errRestoredVolumeNotLuks, got %v", err)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected only the isLuks probe, got %#v", fake.calls)
	}
	assertCryptSubcommand(t, fake.calls[0], "isLuks", "")
}

// Restoring an encrypted snapshot with a secret holding a different passphrase
// must surface as errBadPassphrase, not as an opaque cryptsetup failure.
func TestOpenEncryptedDeviceReportsBadPassphrase(t *testing.T) {
	const volID = "unit-open-wrong-key"
	const passphrase = "wrong-passphrase"
	fake := &fakeCryptExecutor{
		t: t,
		results: []cryptResult{
			{}, // isLuks -> is luks
			{output: "No key available with this passphrase.", err: commandExitError{code: cryptExitNoPermission}},
		},
	}
	useFakeCryptExecutor(t, fake)

	_, err := openEncryptedDevice("/dev/vg/"+volID, volID, &cryptoParams{passphrase: passphrase}, false)
	if !errors.Is(err, errBadPassphrase) {
		t.Fatalf("expected errBadPassphrase, got %v", err)
	}
	if strings.Contains(err.Error(), passphrase) {
		t.Fatalf("passphrase leaked into the error: %v", err)
	}
}

// The mapped CSI codes matter: an operator-fixable credential or state problem
// must not be reported as a transient Internal error.
func TestEncryptedOpenErrorMapsRestoreFailuresToFailedPrecondition(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want codes.Code
	}{
		{name: "bad passphrase", err: errBadPassphrase, want: codes.FailedPrecondition},
		{name: "unencrypted restore source", err: errRestoredVolumeNotLuks, want: codes.FailedPrecondition},
		{name: "cryptsetup failure", err: errors.New("device busy"), want: codes.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := status.Code(encryptedOpenError("volume", tt.err)); got != tt.want {
				t.Fatalf("encryptedOpenError code = %v, want %v", got, tt.want)
			}
		})
	}
}

// A volume restored from an encrypted source into a plain StorageClass must not
// reach the workload as a raw LUKS container.
func TestRejectRestoredLuksContainer(t *testing.T) {
	t.Run("luks header present", func(t *testing.T) {
		useFakeCryptExecutor(t, &fakeCryptExecutor{t: t, results: []cryptResult{{}}})
		err := rejectRestoredLuksContainer("/dev/vg/volume", "volume")
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("expected FailedPrecondition, got %v", err)
		}
	})
	t.Run("plain device", func(t *testing.T) {
		useFakeCryptExecutor(t, &fakeCryptExecutor{
			t:       t,
			results: []cryptResult{{err: commandExitError{code: cryptExitNotLuks}}},
		})
		if err := rejectRestoredLuksContainer("/dev/vg/volume", "volume"); err != nil {
			t.Fatalf("a plain restored device must publish normally, got %v", err)
		}
	})
}

func TestIsRestoredFromSource(t *testing.T) {
	tests := []struct {
		name    string
		context map[string]string
		want    bool
	}{
		{name: "absent", context: map[string]string{}},
		{name: "false", context: map[string]string{restoredFromSourceKey: "false"}},
		{name: "malformed", context: map[string]string{restoredFromSourceKey: "yes-please"}},
		{name: "true", context: map[string]string{restoredFromSourceKey: "true"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRestoredFromSource(tt.context); got != tt.want {
				t.Fatalf("isRestoredFromSource = %t, want %t", got, tt.want)
			}
		})
	}
}

// For a plain (non-encrypted) volume no dm-crypt mapper exists, so the
// unpublish/expand helpers must be no-ops that never invoke cryptsetup.
func TestCloseAndResizeNoopWithoutMapper(t *testing.T) {
	const volID = "unit-plain-volume"
	fake := &fakeCryptExecutor{t: t} // no results -> any call fails the test
	useFakeCryptExecutor(t, fake)

	if err := closeEncryptedDevice(volID); err != nil {
		t.Fatalf("closeEncryptedDevice should be a no-op, got %v", err)
	}
	active, _, err := resizeEncryptedDevice(volID, "unit-test-passphrase")
	if err != nil || active {
		t.Fatalf("resizeEncryptedDevice should be a no-op, got (active=%t, err=%v)", active, err)
	}
	got, err := encryptedVolumeActive(volID)
	if err != nil || got {
		t.Fatalf("encryptedVolumeActive should be false, got (%t, %v)", got, err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("expected no cryptsetup calls, got %#v", fake.calls)
	}
}

func assertCryptSubcommand(t *testing.T, call cryptCall, subcommand, stdin string) {
	t.Helper()
	if call.command != "cryptsetup" {
		t.Fatalf("expected cryptsetup, got %q", call.command)
	}
	if len(call.args) == 0 || call.args[0] != subcommand {
		t.Fatalf("expected subcommand %q, got args %v", subcommand, call.args)
	}
	if call.stdin != stdin {
		t.Fatalf("expected stdin %q for %s, got %q", stdin, subcommand, call.stdin)
	}
}
