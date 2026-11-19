// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLimaStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want limaStatusKind
	}{
		{"empty means absent", "", limaStatusAbsent},
		{"whitespace only is absent", "  \n", limaStatusAbsent},
		{"running", "Running\n", limaStatusRunning},
		{"stopped", "Stopped", limaStatusStopped},
		{"transitional starting is unknown (retry)", "Starting", limaStatusUnknown},
		{"transitional stopping is unknown (retry)", "Stopping", limaStatusUnknown},
		{"garbage is unknown (retry)", "Broken", limaStatusUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, parseLimaStatus([]byte(tc.raw)))
		})
	}
}

func TestLimaDriverInitArgs(t *testing.T) {
	t.Parallel()

	t.Run("qemu driver keeps the legacy argument set", func(t *testing.T) {
		t.Parallel()
		d := NewLimaDriver(LimaDriverConfig{
			InstanceName: "finch", BaseYamlPath: "/os/finch.yaml", VZDriver: false,
		})
		assert.Equal(t, []string{"start", "--name=finch", "/os/finch.yaml", "--tty=false"}, d.initArgs())
	})

	t.Run("vz driver disables ssh over vsock", func(t *testing.T) {
		t.Parallel()
		d := NewLimaDriver(LimaDriverConfig{
			InstanceName: "finch", BaseYamlPath: "/os/finch.yaml", VZDriver: true,
		})
		assert.Equal(t, []string{
			"start", "--name=finch", "/os/finch.yaml", "--tty=false", "--set", ".ssh.overVsock=false",
		}, d.initArgs())
	})
}

func TestLimaDriverRemoveArgs(t *testing.T) {
	t.Parallel()

	d := NewLimaDriver(LimaDriverConfig{InstanceName: "finch"})
	assert.Equal(t, []string{"remove", "finch"}, d.removeArgs(false))
	assert.Equal(t, []string{"remove", "--force", "finch"}, d.removeArgs(true))
}

func TestSetEnv(t *testing.T) {
	t.Parallel()

	t.Run("appends when absent", func(t *testing.T) {
		t.Parallel()
		got := setEnv([]string{"A=1"}, "B", "2")
		assert.Equal(t, []string{"A=1", "B=2"}, got)
	})

	t.Run("replaces in place when present", func(t *testing.T) {
		t.Parallel()
		got := setEnv([]string{"A=1", "LIMA_HOME=/old", "C=3"}, "LIMA_HOME", "/new")
		assert.Equal(t, []string{"A=1", "LIMA_HOME=/new", "C=3"}, got)
	})
}

func TestNewLimaDriverEnvironment(t *testing.T) {
	t.Parallel()

	d := NewLimaDriver(LimaDriverConfig{
		LimactlPath:  "/bin/limactl",
		LimaHomePath: "/home/lima",
		QemuBinDir:   "/qemu/bin",
		InstanceName: "finch",
	})
	require.NotEmpty(t, d.runEnv)

	var sawLimaHome, sawPath bool
	for _, kv := range d.runEnv {
		if kv == "LIMA_HOME=/home/lima" {
			sawLimaHome = true
		}
		if len(kv) >= 5 && kv[:5] == "PATH=" && strings.Contains(kv, "/qemu/bin") {
			sawPath = true
		}
	}
	assert.True(t, sawLimaHome, "LIMA_HOME must be exported for limactl")
	assert.True(t, sawPath, "the vended QEMU bin dir must be on PATH")
}
