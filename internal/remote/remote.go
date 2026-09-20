// Package remote runs ssh and ansible-playbook against the Space's Dev Mode
// SSH endpoint.
package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Target struct {
	Host         string
	User         string
	IdentityFile string
}

var sshOptions = []string{
	"-o", "StrictHostKeyChecking=accept-new",
	"-o", "ServerAliveInterval=30",
	"-o", "ServerAliveCountMax=5",
}

func (t Target) String() string { return t.User + "@" + t.Host }

func (t Target) sshArgs() []string {
	args := append([]string{}, sshOptions...)
	if t.IdentityFile != "" {
		args = append(args, "-i", t.IdentityFile, "-o", "IdentitiesOnly=yes")
	}
	return args
}

// SSH returns an ssh command for the target. With no remote command it opens
// an interactive login shell.
func (t Target) SSH(ctx context.Context, tty bool, command ...string) *exec.Cmd {
	args := t.sshArgs()
	if tty {
		args = append(args, "-t")
	}
	args = append(args, t.String())
	args = append(args, command...)
	return exec.CommandContext(ctx, "ssh", args...)
}

// Output runs a command on the target and returns its stdout.
func (t Target) Output(ctx context.Context, command string) (string, error) {
	cmd := t.SSH(ctx, false, command)
	cmd.Args = append(cmd.Args[:1], append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, cmd.Args[1:]...)...)
	out, err := cmd.Output()
	return string(out), err
}

// WaitSSH retries until the target accepts a connection.
func (t Target) WaitSSH(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if _, err := t.Output(ctx, "true"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ssh to %s not reachable after %s", t, timeout)
		case <-time.After(5 * time.Second):
		}
	}
}

// Upload copies a local file to the target if its checksum differs, and
// reports whether it did. Ansible's piped transfer corrupts large binaries
// through the Dev Mode gateway; a plain ssh pipe does not.
func (t Target) Upload(ctx context.Context, local, dest string) (bool, error) {
	b, err := os.ReadFile(local)
	if err != nil {
		return false, err
	}
	sum := fmt.Sprintf("%x", sha256.Sum256(b))
	out, _ := t.Output(ctx, "sha256sum "+dest+" 2>/dev/null")
	if strings.HasPrefix(out, sum) {
		return false, nil
	}

	script := fmt.Sprintf("mkdir -p %[1]s && cat > %[2]s.tmp && echo '%[3]s  %[2]s.tmp' | sha256sum -c --quiet && chmod +x %[2]s.tmp && mv %[2]s.tmp %[2]s",
		filepath.Dir(dest), dest, sum)
	cmd := t.SSH(ctx, false, script)
	cmd.Args = append(cmd.Args[:1], append([]string{"-T"}, cmd.Args[1:]...)...)
	cmd.Stdin = bytes.NewReader(b)
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("upload %s: %w: %s", dest, err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

type Playbook struct {
	Dir       string
	Name      string
	VarsFiles []string
	Vars      map[string]any
	Tags      []string
	ExtraArgs []string
}

// Run executes the playbook against the target, streaming ansible's output.
func (t Target) Run(ctx context.Context, p Playbook) error {
	args := []string{
		"-i", t.Host + ",",
		"-u", t.User,
		"--ssh-common-args", strings.Join(sshOptions, " "),
	}
	if t.IdentityFile != "" {
		args = append(args, "--private-key", t.IdentityFile)
	}
	for _, f := range p.VarsFiles {
		args = append(args, "-e", "@"+f)
	}
	if len(p.Vars) > 0 {
		b, err := json.Marshal(p.Vars)
		if err != nil {
			return err
		}
		args = append(args, "-e", string(b))
	}
	if len(p.Tags) > 0 {
		args = append(args, "--tags", strings.Join(p.Tags, ","))
	}
	args = append(args, p.ExtraArgs...)
	args = append(args, p.Name)

	cmd := exec.CommandContext(ctx, "ansible-playbook", args...)
	cmd.Dir = p.Dir
	cmd.Env = append(os.Environ(), "ANSIBLE_CONFIG="+filepath.Join(p.Dir, "ansible.cfg"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
