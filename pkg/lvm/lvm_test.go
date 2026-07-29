package lvm

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type commandCall struct {
	command string
	args    []string
}

type commandResult struct {
	command string
	output  string
	err     error
}

type commandExitError struct {
	code int
}

func (e commandExitError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.code)
}

func (e commandExitError) ExitCode() int {
	return e.code
}

type fakeCommandExecutor struct {
	t       *testing.T
	results []commandResult
	calls   []commandCall
}

func (f *fakeCommandExecutor) Execute(command string, args []string) (string, error) {
	f.t.Helper()
	f.calls = append(f.calls, commandCall{command: command, args: append([]string(nil), args...)})
	if len(f.results) == 0 {
		f.t.Fatalf("unexpected command: %s %v", command, args)
	}
	result := f.results[0]
	f.results = f.results[1:]
	if result.command != command {
		f.t.Fatalf("expected command %q, got %q with args %v", result.command, command, args)
	}
	return result.output, result.err
}

func useFakeCommandExecutor(t *testing.T, fake *fakeCommandExecutor) {
	t.Helper()
	original := newCommandExecutor
	newCommandExecutor = func() commandExecutor {
		return fake
	}
	t.Cleanup(func() {
		newCommandExecutor = original
	})
}

func TestMountLVDoesNotFormatWhenFilesystemProbeFails(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{{
			command: "blkid",
			err:     errors.New("transient probe failure"),
		}},
	}
	useFakeCommandExecutor(t, fake)

	_, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "ext4", nil, false)
	if err == nil || !strings.Contains(err.Error(), "unable to determine filesystem type") {
		t.Fatalf("expected filesystem probe error, got %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].command != "blkid" {
		t.Fatalf("expected only blkid to run, got %#v", fake.calls)
	}
}

func TestMountLVDoesNotFormatWhenBlkidIsAmbiguous(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{{
			command: "blkid",
			err:     commandExitError{code: 8},
		}},
	}
	useFakeCommandExecutor(t, fake)

	_, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "ext4", nil, false)
	if err == nil || !strings.Contains(err.Error(), "ambiguous filesystem signatures") {
		t.Fatalf("expected ambiguous signature error, got %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].command != "blkid" {
		t.Fatalf("expected only blkid to run, got %#v", fake.calls)
	}
}

func TestMountLVDoesNotFormatWhenBlkidHasOperationalError(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{{
			command: "blkid",
			err:     commandExitError{code: 4},
		}},
	}
	useFakeCommandExecutor(t, fake)

	_, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "ext4", nil, false)
	if err == nil || !strings.Contains(err.Error(), "blkid failed to inspect") {
		t.Fatalf("expected blkid operational error, got %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].command != "blkid" {
		t.Fatalf("expected only blkid to run, got %#v", fake.calls)
	}
}

func TestMountLVUsesWipefsToConfirmExistingFilesystem(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "blkid", err: commandExitError{code: 2}},
			{command: "wipefs", output: `{"signatures":[{"type":"ext4"}]}`},
			{command: "mount"},
		},
	}
	useFakeCommandExecutor(t, fake)

	if _, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "ext4", nil, false); err != nil {
		t.Fatalf("mountLV failed: %v", err)
	}
	if want := []string{"-p", "-s", "TYPE", "-o", "value", "/dev/vg/volume"}; !reflect.DeepEqual(fake.calls[0].args, want) {
		t.Fatalf("unexpected blkid arguments: want %#v, got %#v", want, fake.calls[0].args)
	}
	if want := []string{"--no-act", "--json", "--output", "TYPE", "/dev/vg/volume"}; !reflect.DeepEqual(fake.calls[1].args, want) {
		t.Fatalf("unexpected wipefs arguments: want %#v, got %#v", want, fake.calls[1].args)
	}
	if got := []string{fake.calls[0].command, fake.calls[1].command, fake.calls[2].command}; !reflect.DeepEqual(got, []string{"blkid", "wipefs", "mount"}) {
		t.Fatalf("unexpected commands: %#v", fake.calls)
	}
}

func TestMountLVDoesNotFormatWhenWipefsFails(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "blkid", err: commandExitError{code: 2}},
			{command: "wipefs", err: errors.New("device read failed")},
		},
	}
	useFakeCommandExecutor(t, fake)

	_, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "ext4", nil, false)
	if err == nil || !strings.Contains(err.Error(), "unable to confirm filesystem signatures") {
		t.Fatalf("expected wipefs error, got %v", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("expected only blkid and wipefs to run, got %#v", fake.calls)
	}
}

func TestMountLVDoesNotFormatXFSExternalLogSignature(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{{
			command: "blkid",
			output:  "xfs_external_log\n",
		}},
	}
	useFakeCommandExecutor(t, fake)

	_, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "xfs", nil, false)
	if err == nil || !strings.Contains(err.Error(), "xfs_external_log found") {
		t.Fatalf("expected existing signature error, got %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].command != "blkid" {
		t.Fatalf("expected only blkid to run, got %#v", fake.calls)
	}
}

func TestMountLVFormatsOnlyAfterWipefsConfirmsNoSignatures(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "blkid", err: commandExitError{code: 2}},
			{command: "wipefs", output: `{"signatures":[]}`},
			{command: "mkfs.ext4"},
			{command: "mount"},
		},
	}
	useFakeCommandExecutor(t, fake)

	if _, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "ext4", nil, false); err != nil {
		t.Fatalf("mountLV failed: %v", err)
	}
	commands := make([]string, 0, len(fake.calls))
	for _, call := range fake.calls {
		commands = append(commands, call.command)
	}
	if want := []string{"blkid", "wipefs", "mkfs.ext4", "mount"}; !reflect.DeepEqual(commands, want) {
		t.Fatalf("unexpected commands: want %#v, got %#v", want, commands)
	}
}

func TestMountLVPassesMountFlagsAndReadOnly(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "blkid", output: "ext4\n"},
			{command: "mount"},
		},
	}
	useFakeCommandExecutor(t, fake)

	mountPath := filepath.Join(t.TempDir(), "mount")
	if _, err := mountLV("volume", mountPath, "vg", "ext4", []string{"noatime"}, true); err != nil {
		t.Fatalf("mountLV failed: %v", err)
	}

	want := []string{"--make-shared", "-t", "ext4", "-o", "noatime,ro", "/dev/vg/volume", mountPath}
	if got := fake.calls[1].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected mount arguments:\nwant: %#v\n got: %#v", want, got)
	}
}

func TestNormalizeMountOptionsEnforcesReadOnly(t *testing.T) {
	got := normalizeMountOptions([]string{"rw", "noatime,nosuid"}, true)
	want := []string{"noatime", "nosuid", "ro"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected normalized mount options: want %#v, got %#v", want, got)
	}
}

func TestMountLVAcceptsAlreadyMountedError(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "blkid", output: "ext4\n"},
			{command: "mount", err: errors.New("mount: /target: already mounted")},
			{command: "findmnt", output: "rw,relatime"},
		},
	}
	useFakeCommandExecutor(t, fake)

	if _, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "ext4", nil, false); err != nil {
		t.Fatalf("idempotent mount retry failed: %v", err)
	}
}

func TestMountLVRejectsWritableExistingMountForReadOnlyRequest(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "blkid", output: "ext4\n"},
			{command: "mount", err: errors.New("mount: /target: already mounted")},
			{command: "findmnt", output: "rw,relatime"},
		},
	}
	useFakeCommandExecutor(t, fake)

	_, err := mountLV("volume", filepath.Join(t.TempDir(), "mount"), "vg", "ext4", nil, true)
	if err == nil || !strings.Contains(err.Error(), "readonly was requested") {
		t.Fatalf("expected incompatible existing mount error, got %v", err)
	}
}

func TestCreateLVSExistingVolumeCompatibility(t *testing.T) {
	const report = `{
		"report": [{
			"lv": [{
				"lv_name": "volume",
				"vg_name": "vg",
				"lv_size": "2097152",
				"segtype": "thin",
				"origin": ""
			}]
		}]
	}`

	t.Run("compatible retry succeeds without lvcreate", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t:       t,
			results: []commandResult{{command: "lvs", output: report}},
		}
		useFakeCommandExecutor(t, fake)

		if _, err := CreateLVS("vg", "volume", 1048576, DmThinType); err != nil {
			t.Fatalf("compatible retry failed: %v", err)
		}
		if len(fake.calls) != 1 {
			t.Fatalf("expected only compatibility lookup, got %#v", fake.calls)
		}
	})

	t.Run("incompatible retry fails", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t:       t,
			results: []commandResult{{command: "lvs", output: report}},
		}
		useFakeCommandExecutor(t, fake)

		if _, err := CreateLVS("vg", "volume", 1048576, StripedType); err == nil {
			t.Fatal("expected incompatible type error")
		}
	})
}

func TestCreateLVSValidatesExistingThinPool(t *testing.T) {
	const noVolumes = `{"report":[{"lv":[]}]}`
	const thinPool = "vg thin-pool vg-thinpool 0\n"

	t.Run("inactive pool fails before lvcreate", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "lvs", output: noVolumes},
				{command: "lvs", output: thinPool},
				{command: "lvs", output: "twi---tz--"},
			},
		}
		useFakeCommandExecutor(t, fake)

		_, err := CreateLVS("vg", "volume", 1048576, DmThinType)
		if err == nil || !strings.Contains(err.Error(), "thin pool vg/vg-thinpool is inactive") {
			t.Fatalf("expected inactive thin-pool error, got %v", err)
		}
		if len(fake.calls) != 3 {
			t.Fatalf("inactive pool should fail before lvcreate, got %#v", fake.calls)
		}
	})

	t.Run("unhealthy pool fails before lvcreate", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "lvs", output: noVolumes},
				{command: "lvs", output: thinPool},
				{command: "lvs", output: "twi-a-tz-- partial"},
			},
		}
		useFakeCommandExecutor(t, fake)

		_, err := CreateLVS("vg", "volume", 1048576, DmThinType)
		if err == nil || !strings.Contains(err.Error(), "thin pool vg/vg-thinpool is unhealthy: partial") {
			t.Fatalf("expected unhealthy thin-pool error, got %v", err)
		}
		if len(fake.calls) != 3 {
			t.Fatalf("unhealthy pool should fail before lvcreate, got %#v", fake.calls)
		}
	})

	t.Run("active healthy pool permits lvcreate", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "lvs", output: noVolumes},
				{command: "lvs", output: thinPool},
				{command: "lvs", output: "twi-a-tz--"},
				{command: "vgs", output: "1"},
				{command: "lvcreate", output: "created"},
			},
		}
		useFakeCommandExecutor(t, fake)

		output, err := CreateLVS("vg", "volume", 1048576, DmThinType)
		if err != nil || output != "created" {
			t.Fatalf("expected active healthy pool to permit creation, output=%q err=%v", output, err)
		}
	})

	t.Run("missing pool is created before the volume", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "lvs", output: noVolumes},
				{command: "lvs"},
				{command: "lvcreate", output: "pool created"},
				{command: "vgs", output: "1"},
				{command: "lvcreate", output: "volume created"},
			},
		}
		useFakeCommandExecutor(t, fake)

		output, err := CreateLVS("vg", "volume", 1048576, DmThinType)
		if err != nil || output != "volume created" {
			t.Fatalf("expected thin pool and volume creation, output=%q err=%v", output, err)
		}
		wantPoolArgs := []string{"-l90%FREE", "--thinpool", "vg-thinpool", "vg"}
		if !reflect.DeepEqual(fake.calls[2].args, wantPoolArgs) {
			t.Fatalf("unexpected thin-pool creation arguments: want %#v, got %#v", wantPoolArgs, fake.calls[2].args)
		}
	})
}

func TestVgActivateReturnsCommandErrors(t *testing.T) {
	t.Run("vgscan failure", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{{
				command: "vgscan",
				output:  "scan output",
				err:     errors.New("scan failed"),
			}},
		}
		useFakeCommandExecutor(t, fake)

		err := VgActivate()
		if err == nil || !strings.Contains(err.Error(), "scan output") || !strings.Contains(err.Error(), "scan failed") {
			t.Fatalf("expected vgscan output and error, got %v", err)
		}
		if len(fake.calls) != 1 {
			t.Fatalf("vgchange should not run after vgscan failure, got %#v", fake.calls)
		}
	})

	t.Run("vgchange failure reports vgchange output", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "vgscan", output: "scan output"},
				{command: "vgchange", output: "activation output", err: errors.New("activation failed")},
			},
		}
		useFakeCommandExecutor(t, fake)

		err := VgActivate()
		if err == nil || !strings.Contains(err.Error(), "activation output") || !strings.Contains(err.Error(), "activation failed") {
			t.Fatalf("expected vgchange output and error, got %v", err)
		}
		if strings.Contains(err.Error(), "scan output") {
			t.Fatalf("vgchange failure reported stale vgscan output: %v", err)
		}
	})
}

func TestEnsureVG(t *testing.T) {
	t.Run("existing VG is activated", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "vgs", output: " vg\n"},
				{command: "vgchange"},
			},
		}
		useFakeCommandExecutor(t, fake)

		if err := EnsureVG("vg"); err != nil {
			t.Fatalf("EnsureVG failed: %v", err)
		}
		if len(fake.calls) != 2 || fake.calls[1].command != "vgchange" {
			t.Fatalf("existing VG should be activated, got %#v", fake.calls)
		}
		wantArgs := []string{"-ay", "vg"}
		if !reflect.DeepEqual(fake.calls[1].args, wantArgs) {
			t.Fatalf("unexpected targeted activation arguments: want %#v, got %#v", wantArgs, fake.calls[1].args)
		}
	})

	t.Run("existing but unactivatable VG fails", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "vgs", output: "vg"},
				{
					command: "vgchange",
					output:  "thin-pool metadata LV is active",
					err:     errors.New("activation prohibited"),
				},
			},
		}
		useFakeCommandExecutor(t, fake)

		err := EnsureVG("vg")
		if err == nil || !strings.Contains(err.Error(), "unable to activate volume group vg") ||
			!strings.Contains(err.Error(), "thin-pool metadata LV is active") {
			t.Fatalf("expected activation failure with LVM output, got %v", err)
		}
	})

	t.Run("missing VG is scanned then activated", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "vgs"},
				{command: "vgscan"},
				{command: "vgs", output: "vg"},
				{command: "vgchange"},
			},
		}
		useFakeCommandExecutor(t, fake)

		if err := EnsureVG("vg"); err != nil {
			t.Fatalf("EnsureVG failed: %v", err)
		}
		want := []string{"vgs", "vgscan", "vgs", "vgchange"}
		for i, command := range want {
			if fake.calls[i].command != command {
				t.Fatalf("unexpected command sequence: want %#v, got %#v", want, fake.calls)
			}
		}
	})

	t.Run("missing VG returns actionable error", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "vgs"},
				{command: "vgscan"},
				{command: "vgs"},
			},
		}
		useFakeCommandExecutor(t, fake)

		err := EnsureVG("vg")
		if err == nil || !strings.Contains(err.Error(), "volume group vg does not exist") {
			t.Fatalf("expected missing VG error, got %v", err)
		}
	})
}

func TestBuildLVExtendArgs(t *testing.T) {
	want := []string{"-L", "1048576b", "-n", "vg/volume"}
	got := buildLVExtendArgs("vg", "volume", 1048576)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected lvextend arguments: want %#v, got %#v", want, got)
	}
}

func TestGetLogicalVolumeByName(t *testing.T) {
	t.Run("returns matching volume", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{{
				command: "lvs",
				output:  `{"report":[{"lv":[{"lv_name":" volume ","vg_name":" vg ","lv_size":" 1048576 ","segtype":"linear","origin":""}]}]}`,
			}},
		}
		useFakeCommandExecutor(t, fake)

		volume, err := getLogicalVolumeByName("volume")
		if err != nil {
			t.Fatalf("getLogicalVolumeByName failed: %v", err)
		}
		if volume.VGName != "vg" || volume.Size != "1048576" {
			t.Fatalf("unexpected logical volume: %#v", volume)
		}
	})

	t.Run("rejects ambiguous name", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{{
				command: "lvs",
				output:  `{"report":[{"lv":[{"lv_name":"volume","vg_name":"vg-a","lv_size":"1"},{"lv_name":"volume","vg_name":"vg-b","lv_size":"1"}]}]}`,
			}},
		}
		useFakeCommandExecutor(t, fake)

		if _, err := getLogicalVolumeByName("volume"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("expected ambiguous name error, got %v", err)
		}
	})

	t.Run("rejects missing volume", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t:       t,
			results: []commandResult{{command: "lvs", output: `{"report":[{"lv":[]}]}`}},
		}
		useFakeCommandExecutor(t, fake)

		if _, err := getLogicalVolumeByName("missing"); err == nil {
			t.Fatal("expected missing volume error")
		}
	})
}

func TestExtendLVSExpandsLogicalVolumeAndExt4Filesystem(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{
				command: "lvs",
				output:  `{"report":[{"lv":[{"lv_name":"volume","vg_name":"vg","lv_size":"1048576","segtype":"linear","origin":""}]}]}`,
			},
			{command: "lvextend", output: "extended"},
			{command: "blkid", output: "ext4\n"},
			{command: "resize2fs", output: "resized"},
		},
	}
	useFakeCommandExecutor(t, fake)

	out, err := extendLVS("volume", 2097152, false, "/mnt/volume")
	if err != nil || out != "extended\nresized" {
		t.Fatalf("extendLVS failed: output=%q err=%v", out, err)
	}
	wantCommands := []string{"lvs", "lvextend", "blkid", "resize2fs"}
	if len(fake.calls) != len(wantCommands) {
		t.Fatalf("unexpected command sequence: %#v", fake.calls)
	}
	for i, want := range wantCommands {
		if fake.calls[i].command != want {
			t.Fatalf("unexpected command sequence: %#v", fake.calls)
		}
	}
	if want := []string{"-L", "2097152b", "-n", "vg/volume"}; !reflect.DeepEqual(fake.calls[1].args, want) {
		t.Fatalf("unexpected lvextend arguments: want %#v, got %#v", want, fake.calls[1].args)
	}
}

func TestExtendLVSRetriesExt4ResizeWhenLogicalVolumeAlreadyExpanded(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{
				command: "lvs",
				output:  `{"report":[{"lv":[{"lv_name":"volume","vg_name":"vg","lv_size":"2097152","segtype":"linear","origin":""}]}]}`,
			},
			{command: "blkid", output: "ext4"},
			{command: "resize2fs", output: "filesystem expanded"},
		},
	}
	useFakeCommandExecutor(t, fake)

	out, err := extendLVS("volume", 2097152, false, "/mnt/volume")
	if err != nil || out != "filesystem expanded" {
		t.Fatalf("extendLVS retry failed: output=%q err=%v", out, err)
	}
	wantCommands := []string{"lvs", "blkid", "resize2fs"}
	if len(fake.calls) != len(wantCommands) {
		t.Fatalf("unexpected command sequence: %#v", fake.calls)
	}
	for i, want := range wantCommands {
		if fake.calls[i].command != want {
			t.Fatalf("unexpected command sequence: %#v", fake.calls)
		}
	}
}

func TestExtendLVSExpandsBlockVolumeWithoutFilesystemCommands(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{
				command: "lvs",
				output:  `{"report":[{"lv":[{"lv_name":"volume","vg_name":"vg","lv_size":"1048576","segtype":"thin","origin":""}]}]}`,
			},
			{command: "lvextend", output: "extended"},
		},
	}
	useFakeCommandExecutor(t, fake)

	out, err := extendLVS("volume", 2097152, true, "/unused")
	if err != nil || out != "extended" {
		t.Fatalf("block extendLVS failed: output=%q err=%v", out, err)
	}
	if len(fake.calls) != 2 || fake.calls[1].command != "lvextend" {
		t.Fatalf("unexpected block-volume command sequence: %#v", fake.calls)
	}
}

func TestExtendLVSUsesMountPathForXFS(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{
				command: "lvs",
				output:  `{"report":[{"lv":[{"lv_name":"volume","vg_name":"vg","lv_size":"2097152","segtype":"linear","origin":""}]}]}`,
			},
			{command: "blkid", output: "xfs"},
			{command: "xfs_growfs", output: "grown"},
		},
	}
	useFakeCommandExecutor(t, fake)

	const mountPath = "/mnt/xfs-volume"
	if _, err := extendLVS("volume", 2097152, false, mountPath); err != nil {
		t.Fatalf("XFS extendLVS failed: %v", err)
	}
	want := []string{"-d", mountPath}
	if got := fake.calls[2].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected xfs_growfs arguments: want %#v, got %#v", want, got)
	}
}

func TestExtendLVSReturnsFilesystemResizeFailureAfterLVExpansion(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{
				command: "lvs",
				output:  `{"report":[{"lv":[{"lv_name":"volume","vg_name":"vg","lv_size":"1048576","segtype":"linear","origin":""}]}]}`,
			},
			{command: "lvextend", output: "LV expanded"},
			{command: "blkid", output: "ext4"},
			{command: "resize2fs", output: "resize output", err: errors.New("resize failed")},
		},
	}
	useFakeCommandExecutor(t, fake)

	out, err := extendLVS("volume", 2097152, false, "/mnt/volume")
	if err == nil || !strings.Contains(err.Error(), "unable to resize ext4 filesystem") {
		t.Fatalf("expected filesystem resize failure, got output=%q err=%v", out, err)
	}
	if out != "LV expanded\nresize output" {
		t.Fatalf("expected both command outputs, got %q", out)
	}
}

func TestExtendLVSRejectsUnsupportedFilesystem(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{
				command: "lvs",
				output:  `{"report":[{"lv":[{"lv_name":"volume","vg_name":"vg","lv_size":"2097152","segtype":"linear","origin":""}]}]}`,
			},
			{command: "blkid", output: "btrfs"},
		},
	}
	useFakeCommandExecutor(t, fake)

	if _, err := extendLVS("volume", 2097152, false, "/mnt/volume"); err == nil || !strings.Contains(err.Error(), "does not support expansion") {
		t.Fatalf("expected unsupported filesystem error, got %v", err)
	}
}

func TestSnapshotBackendOperationsAreIdempotent(t *testing.T) {
	const report = `{
		"report": [{
			"lv": [
				{
					"lv_name": "source",
					"vg_name": "vg",
					"lv_size": "1048576",
					"segtype": "thin",
					"origin": ""
				},
				{
					"lv_name": "lvm-snapshot-id",
					"vg_name": "vg",
					"lv_size": "1048576",
					"segtype": "thin",
					"origin": "source"
				}
			]
		}]
	}`

	t.Run("create retry returns existing compatible snapshot", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t: t,
			results: []commandResult{
				{command: "lvs", output: report},
				{command: "lvs", output: report},
			},
		}
		useFakeCommandExecutor(t, fake)

		if _, err := CreateSnapshot("snapshot-id", "source", "vg", 1048576, DmThinType, false); err != nil {
			t.Fatalf("snapshot retry failed: %v", err)
		}
		if len(fake.calls) != 2 {
			t.Fatalf("expected only source and snapshot lookups, got %#v", fake.calls)
		}
	})

	t.Run("delete retry accepts missing snapshot", func(t *testing.T) {
		fake := &fakeCommandExecutor{
			t:       t,
			results: []commandResult{{command: "lvs", output: `{"report":[{"lv":[]}]}`}},
		}
		useFakeCommandExecutor(t, fake)

		output, err := DeleteSnapshot("snapshot-id", "vg")
		if err != nil {
			t.Fatalf("snapshot delete retry failed: %v", err)
		}
		if !strings.Contains(output, "already been deleted") {
			t.Fatalf("unexpected idempotent delete output: %q", output)
		}
	})
}

func TestRemoveThinPoolIsIdempotentWhenVGIsAbsent(t *testing.T) {
	fake := &fakeCommandExecutor{
		t:       t,
		results: []commandResult{{command: "lvs", output: ""}},
	}
	useFakeCommandExecutor(t, fake)

	if err := RemoveThinPool("missing-vg"); err != nil {
		t.Fatalf("missing VG should be treated as an already absent thin pool: %v", err)
	}
}

func TestGetThinPoolAndCountsFiltersVolumeGroup(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{{
			command: "lvs",
			output: "vg-a thin volume-a\n" +
				"vg-a thin-pool vg-a-thinpool 2\n" +
				"vg-b thin-pool vg-b-thinpool 1\n",
		}},
	}
	useFakeCommandExecutor(t, fake)

	got, err := getThinPoolAndCounts("vg-a")
	if err != nil {
		t.Fatalf("getThinPoolAndCounts failed: %v", err)
	}
	want := map[string]int{"vg-a-thinpool": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected thin pool counts: want %#v, got %#v", want, got)
	}
}

func TestGetFilesystemTypeRejectsAmbiguousWipefsOutput(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "blkid", err: commandExitError{code: 2}},
			{command: "wipefs", output: `{"signatures":[{"type":"ext4"},{"type":"xfs"}]}`},
		},
	}

	if _, err := getFilesystemType(fake, "/dev/vg/volume"); err == nil || !strings.Contains(err.Error(), "ambiguous filesystem signatures") {
		t.Fatalf("expected ambiguous wipefs output to fail, got %v", err)
	}
}

func TestGetFilesystemTypeRejectsMissingWipefsSignatures(t *testing.T) {
	fake := &fakeCommandExecutor{
		t: t,
		results: []commandResult{
			{command: "blkid", err: commandExitError{code: 2}},
			{command: "wipefs", output: `{}`},
		},
	}

	if _, err := getFilesystemType(fake, "/dev/vg/volume"); err == nil || !strings.Contains(err.Error(), "does not contain a signatures array") {
		t.Fatalf("expected incomplete wipefs output to fail, got %v", err)
	}
}
