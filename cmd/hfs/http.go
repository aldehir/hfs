package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/aldehir/hfs/internal/daemon"
	"github.com/spf13/cobra"
)

// exitUnreachable is what `hfs exec` exits with when hfsd couldn't be reached.
const exitUnreachable = 255

// exitError carries a remote command's exit code out through cobra.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// remote returns a client for hfsd's API on the Space's public host. This is
// the default transport: it needs no Dev Mode and isn't subject to the SSH
// gateway's limits.
func (a *app) remote(ctx context.Context) (*daemon.RemoteClient, error) {
	token := a.cfg.HfsdToken()
	if token == "" {
		return nil, errors.New("no hfsd token: run `hfs token init`, or pass --ssh to go through Dev Mode")
	}
	info, err := a.hf.Info(ctx)
	if err != nil {
		return nil, err
	}
	return daemon.NewRemoteClient(info.Host, a.hf.Token(), token), nil
}

// useSSH reports whether to go through Dev Mode SSH instead of hfsd.
func (a *app) useSSH() bool { return a.ssh || a.cfg.HfsdToken() == "" }

// run executes argv on the Space over whichever transport is active, wired to
// this process's stdio.
func (a *app) run(ctx context.Context, tty bool, argv ...string) error {
	if a.useSSH() {
		return a.interactive(ctx, tty, shellQuote(argv))
	}
	rc, err := a.remote(ctx)
	if err != nil {
		return err
	}
	code, err := rc.Exec(ctx, daemon.ExecRequest{Argv: argv}, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func (a *app) execCmd() *cobra.Command {
	var (
		shell string
		dir   string
	)
	cmd := &cobra.Command{
		Use:   "exec [--shell STRING | -- COMMAND [ARGS...]]",
		Short: "Run a command on the Space through hfsd, with stdio attached",
		Long: "Run a command on the Space through hfsd, with stdio attached.\n\n" +
			"The command is killed if the connection drops; use `hfs ctl run` for work\n" +
			"that should outlive it. This is also what the ansible connection plugin calls.",
		RunE: func(cmd *cobra.Command, args []string) error {
			argv := args
			if shell != "" {
				argv = []string{"/bin/sh", "-c", shell}
			}
			if len(argv) == 0 {
				return errors.New("nothing to run")
			}
			rc, err := a.remote(cmd.Context())
			if err != nil {
				return err
			}
			code, err := rc.Exec(cmd.Context(), daemon.ExecRequest{Argv: argv, Dir: dir}, os.Stdin, os.Stdout, os.Stderr)
			if err != nil && code < 0 {
				// Never reached the command: tell callers (the ansible
				// plugin) apart from a command that ran and failed.
				fmt.Fprintln(os.Stderr, "error:", err)
				return exitError{exitUnreachable}
			}
			if err != nil {
				return err
			}
			if code != 0 {
				return exitError{code}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "run this string with /bin/sh -c")
	cmd.Flags().StringVar(&dir, "dir", "", "working directory")
	return cmd
}

func (a *app) putCmd() *cobra.Command {
	var mode string
	cmd := &cobra.Command{
		Use:   "put LOCAL REMOTE",
		Short: "Upload a file to the Space through hfsd",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := strconv.ParseUint(mode, 8, 32)
			if err != nil {
				return fmt.Errorf("invalid mode %q", mode)
			}
			rc, err := a.remote(cmd.Context())
			if err != nil {
				return err
			}
			return rc.Put(cmd.Context(), args[0], args[1], os.FileMode(m))
		},
	}
	cmd.Flags().StringVar(&mode, "mode", "644", "file mode (octal)")
	return cmd
}

func (a *app) fetchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "fetch REMOTE LOCAL",
		Short: "Download a file from the Space through hfsd",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			rc, err := a.remote(cmd.Context())
			if err != nil {
				return err
			}
			return rc.Fetch(cmd.Context(), args[0], args[1])
		},
	}
}

func (a *app) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage the hfsd token that guards remote execution",
	}
	var restartOK bool
	init := &cobra.Command{
		Use:   "init",
		Short: "Generate a token, save it locally, and set it as the Space's HFSD_TOKEN secret",
		Long: "Generate a token, save it locally, and set it as the Space's HFSD_TOKEN secret.\n\n" +
			"Changing a secret restarts the Space, which wipes the container and, on\n" +
			"scarce hardware, can lose the GPU to a scheduling failure.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !restartOK {
				return errors.New("this restarts the Space; pass --restart-ok to go ahead")
			}
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return err
			}
			token := hex.EncodeToString(b)
			if err := a.hf.SetSecret(cmd.Context(), "HFSD_TOKEN", token); err != nil {
				return err
			}
			if err := os.WriteFile(a.cfg.HfsdTokenPath(), []byte(token+"\n"), 0o600); err != nil {
				return err
			}
			fmt.Println("HFSD_TOKEN set on the Space; saved to", a.cfg.HfsdTokenPath())
			return nil
		},
	}
	init.Flags().BoolVar(&restartOK, "restart-ok", false, "acknowledge that the Space will restart")
	cmd.AddCommand(init)
	return cmd
}
