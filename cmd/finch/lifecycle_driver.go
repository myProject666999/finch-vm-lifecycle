// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/runfinch/finch/pkg/lifecycle"
)

// LimaDriver adapts limactl to the lifecycle.Driver interface.
//
// All mutating commands are run through exec.CommandContext so that cancelling
// the context (SIGINT/SIGTERM) terminates the limactl process together with the
// CLI: no underlying invocation is allowed to keep running after the lifecycle
// command returns, which is what keeps the lock holder record aligned with real
// driver progress.
type LimaDriver struct {
	limactlPath  string
	limaHomePath string
	qemuBinDir   string
	instanceName string
	// baseYamlPath and initExtraArgs reproduce the argument set of the legacy
	// `finch vm init` implementation.
	baseYamlPath  string
	initExtraArgs []string
	// runEnv is the environment every limactl invocation receives (LIMA_HOME and
	// an augmented PATH).
	runEnv []string
}

// LimaDriverConfig carries the installation paths needed to build a LimaDriver.
type LimaDriverConfig struct {
	LimactlPath  string
	LimaHomePath string
	QemuBinDir   string
	InstanceName string
	BaseYamlPath string
	// VZDriver disables Lima's ssh-over-vsock default, mirroring the legacy init.
	VZDriver bool
}

// NewLimaDriver constructs a driver with the limactl environment derived from
// the Finch installation layout.
func NewLimaDriver(cfg LimaDriverConfig) *LimaDriver {
	d := &LimaDriver{
		limactlPath:  cfg.LimactlPath,
		limaHomePath: cfg.LimaHomePath,
		qemuBinDir:   cfg.QemuBinDir,
		instanceName: cfg.InstanceName,
		baseYamlPath: cfg.BaseYamlPath,
		runEnv:       limaRunEnv(cfg.LimaHomePath, cfg.QemuBinDir),
	}
	if cfg.VZDriver {
		d.initExtraArgs = []string{"--set", ".ssh.overVsock=false"}
	}
	return d
}

// limaRunEnv builds LIMA_HOME plus a PATH that contains the vended QEMU/bin
// directory, matching the historical NerdctlCmdCreator environment.
func limaRunEnv(limaHome, qemuBinDir string) []string {
	env := environ()
	env = setEnv(env, "LIMA_HOME", limaHome)
	env = setEnv(env, "PATH", qemuBinDir+pathListSep+getEnv("PATH"))
	return env
}

// run executes limactl with the driver's standard environment and returns its
// combined output. Context cancellation kills the child (and, on Unix, its
// process group) so no underlying invocation outlives the command.
func (d *LimaDriver) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, d.limactlPath, args...)
	configureCancelableCommand(ctx, cmd)
	cmd.Env = d.runEnv
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return out.Bytes(), context.Canceled
		}
		return out.Bytes(), fmt.Errorf("%s %s: %w (output: %s)", d.limactlPath, strings.Join(args, " "), err,
			strings.TrimSpace(out.String()))
	}
	return out.Bytes(), nil
}

// Status implements lifecycle.Driver by parsing `limactl ls -f {{.Status}}`.
func (d *LimaDriver) Status(ctx context.Context) (lifecycle.DriverState, error) {
	out, err := d.run(ctx, "ls", "-f", "{{.Status}}", d.instanceName)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return 0, context.Canceled
		}
		// A missing instance is a normal, expected answer rather than a failure.
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" || strings.Contains(trimmed, fmt.Sprintf("No instance matching %s found", d.instanceName)) {
			return lifecycle.DriverAbsent, nil
		}
		return 0, err
	}
	switch parseLimaStatus(out) {
	case limaStatusAbsent:
		return lifecycle.DriverAbsent, nil
	case limaStatusRunning:
		return lifecycle.DriverRunning, nil
	case limaStatusStopped:
		return lifecycle.DriverStopped, nil
	default:
		// Transitional Lima statuses (Starting/Stopping) and unknown values: tell
		// the manager to retry rather than misclassifying the VM.
		return 0, lifecycle.ErrDriverNotReady
	}
}

// limaStatusKind is the parsed classification of a `limactl ls` status line.
type limaStatusKind int

const (
	limaStatusUnknown limaStatusKind = iota
	limaStatusAbsent
	limaStatusRunning
	limaStatusStopped
)

// parseLimaStatus maps the raw limactl status output onto a coarse kind. It is
// a pure function so the mapping is fully testable without invoking limactl.
func parseLimaStatus(raw []byte) limaStatusKind {
	switch strings.TrimSpace(string(raw)) {
	case "":
		return limaStatusAbsent
	case "Running":
		return limaStatusRunning
	case "Stopped":
		return limaStatusStopped
	default:
		return limaStatusUnknown
	}
}

// initArgs builds the limactl arguments for the legacy `finch vm init`
// invocation, which creates and first-boots the instance.
func (d *LimaDriver) initArgs() []string {
	args := []string{
		"start",
		"--name=" + d.instanceName,
		d.baseYamlPath,
		"--tty=false",
	}
	return append(args, d.initExtraArgs...)
}

// Init implements lifecycle.Driver with the legacy `limactl start --name=...`
// invocation, which creates and first-boots the instance.
func (d *LimaDriver) Init(ctx context.Context) error {
	_, err := d.run(ctx, d.initArgs()...)
	return err
}

// Start implements lifecycle.Driver.
func (d *LimaDriver) Start(ctx context.Context) error {
	_, err := d.run(ctx, "start", d.instanceName)
	return err
}

// Stop implements lifecycle.Driver with a graceful stop.
func (d *LimaDriver) Stop(ctx context.Context) error {
	_, err := d.run(ctx, "stop", d.instanceName)
	return err
}

// ForceStop implements lifecycle.Driver.
func (d *LimaDriver) ForceStop(ctx context.Context) error {
	_, err := d.run(ctx, "stop", "--force", d.instanceName)
	return err
}

// removeArgs builds the limactl arguments for removing the instance.
func (d *LimaDriver) removeArgs(force bool) []string {
	args := []string{"remove"}
	if force {
		args = append(args, "--force")
	}
	return append(args, d.instanceName)
}

// Remove implements lifecycle.Driver.
func (d *LimaDriver) Remove(ctx context.Context, force bool) error {
	_, err := d.run(ctx, d.removeArgs(force)...)
	return err
}

// Compile-time interface check.
var _ lifecycle.Driver = (*LimaDriver)(nil)

// setEnv replaces or appends name=value in an environment slice.
func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
