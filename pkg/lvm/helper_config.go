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
	"fmt"
	"time"
)

const (
	DefaultHelperCommandTimeout  = 3 * time.Minute
	DefaultHelperPodTimeout      = 3*time.Minute + 30*time.Second
	DefaultThinPoolCreateTimeout = 15 * time.Minute
	DefaultThinPoolPodTimeout    = 18 * time.Minute
	DefaultMaxActiveHelpers      = 3
)

// HelperConfig controls the pods used for controller-side LVM operations.
type HelperConfig struct {
	CommandTimeout        time.Duration
	PodTimeout            time.Duration
	ThinPoolCreateTimeout time.Duration
	ThinPoolPodTimeout    time.Duration
	MaxActive             int
}

// DefaultHelperConfig returns the helper settings used when no options are set.
func DefaultHelperConfig() HelperConfig {
	return HelperConfig{
		CommandTimeout:        DefaultHelperCommandTimeout,
		PodTimeout:            DefaultHelperPodTimeout,
		ThinPoolCreateTimeout: DefaultThinPoolCreateTimeout,
		ThinPoolPodTimeout:    DefaultThinPoolPodTimeout,
		MaxActive:             DefaultMaxActiveHelpers,
	}
}

func (c HelperConfig) validate() error {
	if c.CommandTimeout <= 0 {
		return fmt.Errorf("helper command timeout must be greater than zero")
	}
	if c.PodTimeout <= c.CommandTimeout {
		return fmt.Errorf(
			"helper pod timeout %s must be greater than command timeout %s",
			c.PodTimeout,
			c.CommandTimeout,
		)
	}
	if c.ThinPoolCreateTimeout <= 0 {
		return fmt.Errorf("thin-pool creation timeout must be greater than zero")
	}
	if c.ThinPoolPodTimeout <= c.ThinPoolCreateTimeout {
		return fmt.Errorf(
			"thin-pool helper pod timeout %s must be greater than creation timeout %s",
			c.ThinPoolPodTimeout,
			c.ThinPoolCreateTimeout,
		)
	}
	if c.MaxActive <= 0 {
		return fmt.Errorf("maximum active helpers must be greater than zero")
	}
	return nil
}

// DriverOption configures optional driver behavior.
type DriverOption func(*Lvm) error

// WithHelperConfig configures controller-side LVM helper pods.
func WithHelperConfig(config HelperConfig) DriverOption {
	return func(driver *Lvm) error {
		if err := config.validate(); err != nil {
			return err
		}
		driver.helperConfig = config
		return nil
	}
}
