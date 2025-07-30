package hostexec

import (
	"context"
	"io"
	"os"
	"reflect"
	"testing"

	"k8s.io/utils/exec"
)

func TestNew(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "chroot_test")
	if err != nil {
		t.Fatalf("Temporary directory creation failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tests := []struct {
		name      string
		cmdMap    map[string]string
		chrootDir string
		wantErr   bool
	}{
		{
			name:      "valid chroot directory",
			cmdMap:    nil,
			chrootDir: tmpDir,
			wantErr:   false,
		},
		{
			name:      "invalid chroot directory",
			cmdMap:    nil,
			chrootDir: "/invalid/path",
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cmdMap, tt.chrootDir)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHostexec_wrapEnv(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		args     []string
		wantCmd  string
		wantArgs []string
	}{
		{
			name:     "empty args",
			cmd:      "echo",
			args:     nil,
			wantCmd:  "/usr/bin/env",
			wantArgs: []string{"-i", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "echo"},
		},
		{
			name:     "non-empty args",
			cmd:      "echo",
			args:     []string{"hello", "world"},
			wantCmd:  "/usr/bin/env",
			wantArgs: []string{"-i", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "echo", "hello", "world"},
		},
		{
			name:     "fully qualified cmd without args",
			cmd:      "/bin/echo",
			args:     nil,
			wantCmd:  "/bin/echo",
			wantArgs: nil,
		},
		{
			name:     "fully qualified cmd with args",
			cmd:      "/bin/echo",
			args:     []string{"hello", "world"},
			wantCmd:  "/bin/echo",
			wantArgs: []string{"hello", "world"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &hostexec{}

			c, a := h.wrapEnv(tt.cmd, tt.args...)
			if c != tt.wantCmd {
				t.Errorf("wrapEnv() c = %v, want %v", c, tt.wantCmd)
			}
			if !reflect.DeepEqual(a, tt.wantArgs) {
				t.Errorf("wrapEnv() a = %v, want %v", a, tt.wantArgs)
			}
		})
	}
}

func TestHostexec_resolveCmd(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		cmdArgs     []string
		cmdMap      map[string]string
		wantCmd     string
		wantCmdArgs []string
	}{
		{
			name:        "empty cmd map",
			cmd:         "echo",
			cmdArgs:     []string{"hello", "world"},
			cmdMap:      nil,
			wantCmd:     "echo",
			wantCmdArgs: []string{"hello", "world"},
		},
		{
			name:    "non-empty cmd map",
			cmd:     "echo",
			cmdArgs: []string{"hello", "world"},
			cmdMap: map[string]string{
				"echo": "/bin/echo",
			},
			wantCmd:     "/bin/echo",
			wantCmdArgs: []string{"hello", "world"},
		},
		{
			name:    "non-empty cmd map without matching command",
			cmd:     "echo",
			cmdArgs: []string{"hello", "world"},
			cmdMap: map[string]string{
				"cat": "/dummy/cat",
			},
			wantCmd:     "echo",
			wantCmdArgs: []string{"hello", "world"},
		},
		{
			name:    "empty string mapping",
			cmd:     "echo",
			cmdArgs: []string{"hello", "world"},
			cmdMap: map[string]string{
				"echo": "",
			},
			wantCmd:     "echo",
			wantCmdArgs: []string{"hello", "world"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &hostexec{commandMap: tt.cmdMap}

			c, a := h.resolveCmd(tt.cmd, tt.cmdArgs...)
			if c != tt.wantCmd {
				t.Errorf("resolveCmd() c = %v, want %v", c, tt.wantCmd)
			}
			if !reflect.DeepEqual(a, tt.wantCmdArgs) {
				t.Errorf("resolveCmd() a = %v, want %v", a, tt.wantCmdArgs)
			}
		})
	}
}

func TestHostexec_wrapChroot(t *testing.T) {
	tests := []struct {
		name     string
		chroot   string
		cmd      string
		args     []string
		wantCmd  string
		wantArgs []string
	}{
		{
			name:     "empty chroot",
			chroot:   "",
			cmd:      "echo",
			args:     []string{"hello", "world"},
			wantCmd:  "echo",
			wantArgs: []string{"hello", "world"},
		},
		{
			name:     "non-empty chroot",
			chroot:   "/tmp",
			cmd:      "echo",
			args:     []string{"hello", "world"},
			wantCmd:  "/usr/sbin/chroot",
			wantArgs: []string{"/tmp", "echo", "hello", "world"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &hostexec{chrootDir: tt.chroot}

			c, a := h.wrapChroot(tt.cmd, tt.args...)
			if c != tt.wantCmd {
				t.Errorf("wrapChroot() c = %v, want %v", c, tt.wantCmd)
			}
			if !reflect.DeepEqual(a, tt.wantArgs) {
				t.Errorf("wrapChroot() a = %v, want %v", a, tt.wantArgs)
			}
		})
	}
}

func TestHostexec_wrap(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		args     []string
		chroot   string
		cmdMap   map[string]string
		wantCmd  string
		wantArgs []string
	}{
		{
			name: "no wrappers",
			cmd:  "/bin/echo",
			args: []string{
				"hello",
				"world",
			},
			chroot:  "",
			cmdMap:  nil,
			wantCmd: "/bin/echo",
			wantArgs: []string{
				"hello",
				"world",
			},
		},
		{
			name: "env wrapper",
			cmd:  "echo",
			args: []string{
				"hello",
				"world",
			},
			chroot:  "",
			cmdMap:  nil,
			wantCmd: "/usr/bin/env",
			wantArgs: []string{
				"-i", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "echo", "hello",
				"world",
			},
		},
		{
			name:    "chroot wrapper",
			cmd:     "/bin/echo",
			args:    []string{"hello", "world"},
			chroot:  "/tmp",
			cmdMap:  nil,
			wantCmd: "/usr/sbin/chroot",
			wantArgs: []string{
				"/tmp",
				"/bin/echo",
				"hello",
				"world",
			},
		},
		{
			name: "cmd path wrapper",
			cmd:  "echo",
			args: []string{
				"hello",
				"world",
			},
			chroot: "",
			cmdMap: map[string]string{
				"echo": "/bin/echo",
			},
			wantCmd: "/bin/echo",
			wantArgs: []string{
				"hello", "world",
			},
		},
		{
			name:    "chain wrappers",
			cmd:     "echo",
			args:    []string{"hello", "world"},
			chroot:  "/tmp",
			cmdMap:  map[string]string{"echo": "/bin/echo"},
			wantCmd: "/usr/sbin/chroot",
			wantArgs: []string{
				"/tmp",
				"/bin/echo",
				"hello", "world",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &hostexec{
				commandMap: tt.cmdMap,
				chrootDir:  tt.chroot,
			}

			c, a := h.wrap(tt.cmd, tt.args...)
			if c != tt.wantCmd {
				t.Errorf("wrap() c = %v, want %v", c, tt.wantCmd)
			}
			if !reflect.DeepEqual(a, tt.wantArgs) {
				t.Errorf("wrap() a = %v, want %v", a, tt.wantArgs)
			}
		})
	}
}

// dummyCmd statisfies the interface for exec.Cmd for testing purposes
type dummyCmd struct{}

func (dummyCmd) Run() error                         { return nil }
func (dummyCmd) CombinedOutput() ([]byte, error)    { return nil, nil }
func (dummyCmd) Output() ([]byte, error)            { return nil, nil }
func (dummyCmd) SetDir(string)                      {}
func (dummyCmd) SetStdin(io.Reader)                 {}
func (dummyCmd) SetStdout(io.Writer)                {}
func (dummyCmd) SetStderr(io.Writer)                {}
func (dummyCmd) SetEnv([]string)                    {}
func (dummyCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (dummyCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (dummyCmd) Start() error                       { return nil }
func (dummyCmd) Wait() error                        { return nil }
func (dummyCmd) Stop()                              {}

// dummyInterface satisfies the interface for exec.Interface for testing purposes
type dummyInterface struct {
	commandFunc        func(string, ...string) exec.Cmd
	commandContextFunc func(context.Context, string, ...string) exec.Cmd
	lookPathFunc       func(string) (string, error)
}

func (d *dummyInterface) Command(cmd string, args ...string) exec.Cmd {
	return d.commandFunc(cmd, args...)
}
func (d *dummyInterface) CommandContext(ctx context.Context, cmd string, args ...string) exec.Cmd {
	return d.commandContextFunc(ctx, cmd, args...)
}
func (d *dummyInterface) LookPath(path string) (string, error) { return d.lookPathFunc(path) }

func TestHostexec_Command(t *testing.T) {
	successCmd := dummyCmd{}

	i := &dummyInterface{
		commandFunc: func(cmd string, args ...string) exec.Cmd {
			if cmd != "/bin/echo" {
				t.Errorf("Command() cmd = %v, want %v", cmd, "echo")
			}
			if !reflect.DeepEqual(args, []string{"hello", "world"}) {
				t.Errorf("Command() args = %v, want %v", args, []string{"hello", "world"})
			}
			return successCmd
		},
	}

	h := &hostexec{
		Executor: i,
		commandMap: map[string]string{
			"echo": "/bin/echo",
		},
	}

	c := h.Command("echo", "hello", "world")
	if c != successCmd {
		t.Errorf("Command() c = %v, want %v", c, successCmd)
	}
}

func TestHostexec_CommandContext(t *testing.T) {
	successCmd := dummyCmd{}

	i := &dummyInterface{
		commandContextFunc: func(_ context.Context, cmd string, args ...string) exec.Cmd {
			if cmd != "/bin/echo" {
				t.Errorf("Command() cmd = %v, want %v", cmd, "echo")
			}
			if !reflect.DeepEqual(args, []string{"hello", "world"}) {
				t.Errorf("Command() args = %v, want %v", args, []string{"hello", "world"})
			}
			return successCmd
		},
	}

	h := &hostexec{
		Executor: i,
		commandMap: map[string]string{
			"echo": "/bin/echo",
		},
	}

	c := h.CommandContext(context.Background(), "echo", "hello", "world")
	if c != successCmd {
		t.Errorf("Command() c = %v, want %v", c, successCmd)
	}
}
