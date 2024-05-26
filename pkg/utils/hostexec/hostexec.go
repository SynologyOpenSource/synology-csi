// Package hostexec automatically wraps commands executed with kubernetes hostexec into
// chrooted commands
package hostexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"k8s.io/utils/exec"
)

// defaultSearchPath for running commands without absolute paths
var defaultSearchPath = []string{
	"/usr/local/sbin",
	"/usr/local/bin",
	"/usr/sbin",
	"/usr/bin",
	"/sbin",
	"/bin",
}

// Executor is mostly k8s.io/utils/exec compatible interface for the portions
// that synology-csi uses.
type Executor interface {
	Command(string, ...string) exec.Cmd
	CommandContext(context.Context, string, ...string) exec.Cmd
}

type hostexec struct {
	Executor
	commandMap map[string]string
	chrootDir  string
}

// New creates an instance of hostexec to execute commands in the given environment
func New(cmdMap map[string]string, chrootDir string) (Executor, error) {
	// If chroot directory is defined, check that directory exists or return an error
	if chrootDir != "" {
		fileinfo, err := os.Stat(chrootDir)
		if err != nil || !fileinfo.IsDir() {
			return nil, errors.New("chroot directory does not exist or is not a directory")
		}
	}

	return &hostexec{exec.New(), cmdMap, chrootDir}, nil
}

func (h *hostexec) resolveCmd(cmd string, args ...string) (string, []string) {
	c, ok := h.commandMap[cmd]
	if !ok || c == "" {
		return cmd, args
	}

	return c, args
}

func (h *hostexec) wrapEnv(cmd string, args ...string) (string, []string) {
	if strings.ContainsAny(cmd, "/") {
		return cmd, args
	}

	sp := fmt.Sprintf("PATH=%s", strings.Join(defaultSearchPath, ":"))
	args = append([]string{"-i", sp, cmd}, args...)
	cmd = "/usr/bin/env"

	return cmd, args
}

func (h *hostexec) wrapChroot(cmd string, args ...string) (string, []string) {
	if h.chrootDir == "" {
		return cmd, args
	}

	args = append([]string{h.chrootDir, cmd}, args...)
	cmd = "/usr/sbin/chroot"

	return cmd, args
}

func (h *hostexec) wrap(cmd string, args ...string) (string, []string) {
	cmd, args = h.resolveCmd(cmd, args...)
	cmd, args = h.wrapEnv(cmd, args...)
	cmd, args = h.wrapChroot(cmd, args...)

	return cmd, args
}

func (h *hostexec) Command(cmd string, args ...string) exec.Cmd {
	cmd, args = h.wrap(cmd, args...)
	return h.Executor.Command(cmd, args...)
}

func (h *hostexec) CommandContext(ctx context.Context, cmd string, args ...string) exec.Cmd {
	cmd, args = h.wrap(cmd, args...)
	return h.Executor.CommandContext(ctx, cmd, args...)
}
