package lvm

import (
	"reflect"
	"testing"
)

func TestBuildThinPoolCreateArgs(t *testing.T) {
	const vg = "vmvg"
	const pool = "vmvg-thinpool"

	tests := []struct {
		name             string
		chunkSize        string
		poolMetadataSize string
		zeroBlocks       string
		want             []string
	}{
		{
			name: "all empty preserves current behavior (LVM auto-select)",
			want: []string{"-l90%FREE", "--thinpool", pool, vg},
		},
		{
			name:      "chunk size only",
			chunkSize: "512K",
			want:      []string{"-l90%FREE", "--thinpool", pool, "--chunksize", "512K", vg},
		},
		{
			name:             "chart defaults - 512K chunks, 16G metadata, no zeroing",
			chunkSize:        "512K",
			poolMetadataSize: "16G",
			zeroBlocks:       "false",
			want: []string{
				"-l90%FREE", "--thinpool", pool,
				"--chunksize", "512K",
				"--poolmetadatasize", "16G",
				"--zero", "n",
				vg,
			},
		},
		{
			name:       "zeroBlocks=true maps to --zero y",
			zeroBlocks: "true",
			want:       []string{"-l90%FREE", "--thinpool", pool, "--zero", "y", vg},
		},
		{
			name:       "zeroBlocks is case-insensitive",
			zeroBlocks: "FALSE",
			want:       []string{"-l90%FREE", "--thinpool", pool, "--zero", "n", vg},
		},
		{
			name:       "unknown zeroBlocks value defaults to --zero y (safe: LVM's own default)",
			zeroBlocks: "maybe",
			want:       []string{"-l90%FREE", "--thinpool", pool, "--zero", "y", vg},
		},
		{
			name:             "snapshot-heavy override: 128K chunks",
			chunkSize:        "128K",
			poolMetadataSize: "4G",
			zeroBlocks:       "false",
			want: []string{
				"-l90%FREE", "--thinpool", pool,
				"--chunksize", "128K",
				"--poolmetadatasize", "4G",
				"--zero", "n",
				vg,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildThinPoolCreateArgs(vg, pool, tt.chunkSize, tt.poolMetadataSize, tt.zeroBlocks)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildThinPoolCreateArgs()\n got:  %v\n want: %v", got, tt.want)
			}
		})
	}
}
