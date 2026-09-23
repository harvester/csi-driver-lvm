package lvm

import (
	"strings"
	"testing"
	"time"
)

func TestHelperConfigValidation(t *testing.T) {
	if err := DefaultHelperConfig().validate(); err != nil {
		t.Fatalf("default helper configuration is invalid: %v", err)
	}

	tests := []struct {
		name   string
		update func(*HelperConfig)
		want   string
	}{
		{
			name: "command timeout",
			update: func(config *HelperConfig) {
				config.CommandTimeout = 0
			},
			want: "command timeout",
		},
		{
			name: "pod deadline",
			update: func(config *HelperConfig) {
				config.PodTimeout = config.CommandTimeout
			},
			want: "must be greater",
		},
		{
			name: "thin-pool creation timeout",
			update: func(config *HelperConfig) {
				config.ThinPoolCreateTimeout = 0
			},
			want: "thin-pool creation timeout",
		},
		{
			name: "thin-pool pod deadline",
			update: func(config *HelperConfig) {
				config.ThinPoolPodTimeout = config.ThinPoolCreateTimeout
			},
			want: "must be greater",
		},
		{
			name: "active limit",
			update: func(config *HelperConfig) {
				config.MaxActive = 0
			},
			want: "maximum active helpers",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultHelperConfig()
			test.update(&config)
			err := config.validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected error containing %q, got %v", test.want, err)
			}
		})
	}
}

func TestSetThinPoolCreateTimeout(t *testing.T) {
	original := thinPoolCreateTimeout()
	t.Cleanup(func() {
		if err := SetThinPoolCreateTimeout(original); err != nil {
			t.Fatalf("failed to restore thin-pool creation timeout: %v", err)
		}
	})

	if err := SetThinPoolCreateTimeout(0); err == nil {
		t.Fatal("expected zero thin-pool creation timeout to fail")
	}
	if err := SetThinPoolCreateTimeout(42 * time.Second); err != nil {
		t.Fatalf("failed to set thin-pool creation timeout: %v", err)
	}
	if got := thinPoolCreateTimeout(); got != 42*time.Second {
		t.Fatalf("unexpected thin-pool creation timeout: %s", got)
	}
}

func TestSetCommandTimeout(t *testing.T) {
	original := commandTimeout()
	t.Cleanup(func() {
		if err := SetCommandTimeout(original); err != nil {
			t.Fatalf("failed to restore command timeout: %v", err)
		}
	})

	if err := SetCommandTimeout(0); err == nil {
		t.Fatal("expected zero command timeout to fail")
	}
	if err := SetCommandTimeout(42 * time.Second); err != nil {
		t.Fatalf("failed to set command timeout: %v", err)
	}
	if got := commandTimeout(); got != 42*time.Second {
		t.Fatalf("unexpected command timeout: %s", got)
	}
}
