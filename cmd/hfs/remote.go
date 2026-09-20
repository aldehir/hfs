package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aldehir/hfs/internal/daemon"
	"github.com/aldehir/hfs/internal/hf"
	"github.com/aldehir/hfs/internal/remote"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const (
	remoteHfsd        = "/home/user/.hfs/bin/hfsd"
	remoteHfsdPersist = "/data/hfs/hfsd"
	defaultSession    = "dev"
	remoteWorkdir     = "/app"
)

type provisionOpts struct {
	profile  string
	tags     []string
	repo     string
	ref      string
	pr       int
	model    string
	noUpdate bool
}

func (o *provisionOpts) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringSliceVar(&o.tags, "tags", nil, "only run these stages (build, server)")
	f.StringVar(&o.repo, "repo", "", "override llama.cpp repo (OWNER/REPO)")
	f.StringVar(&o.ref, "ref", "", "override llama.cpp branch, tag, or commit")
	f.IntVar(&o.pr, "pr", 0, "build this llama.cpp PR instead of a ref")
	f.StringVar(&o.model, "model", "", "override the model (-hf spec)")
	f.BoolVar(&o.noUpdate, "no-update", false, "build the remote checkout as-is, without fetching")
}

func (o *provisionOpts) vars() map[string]any {
	v := map[string]any{}
	if o.repo != "" {
		v["llama_repo"] = o.repo
	}
	if o.ref != "" {
		v["llama_ref"] = o.ref
	}
	if o.pr != 0 {
		v["llama_pr"] = o.pr
	}
	if o.model != "" {
		v["model"] = o.model
	}
	if o.noUpdate {
		v["llama_update"] = false
	}
	return v
}

// passthrough splits args at "--": everything after it goes to the wrapped command.
func passthrough(cmd *cobra.Command, args []string) (before, after []string) {
	if i := cmd.ArgsLenAtDash(); i >= 0 {
		return args[:i], args[i:]
	}
	return args, nil
}

func (a *app) playbook(ctx context.Context, name string, o provisionOpts, extraArgs []string) error {
	profile, err := a.cfg.ProfilePath(o.profile)
	if err != nil {
		return err
	}
	if _, err := os.Stat(a.cfg.HfsdBinary); err != nil {
		return fmt.Errorf("hfsd binary not found (run `make`): %w", err)
	}
	vars := o.vars()
	vars["profile"] = strings.TrimSuffix(filepath.Base(profile), ".yml")
	vars["results_dir"] = a.cfg.ResultsDir
	pb := remote.Playbook{
		Dir:       a.cfg.AnsibleDir,
		Name:      name,
		VarsFiles: []string{profile},
		Vars:      vars,
		Tags:      o.tags,
		ExtraArgs: extraArgs,
	}

	if a.useSSH() {
		target, err := a.target(ctx)
		if err != nil {
			return err
		}
		// The copy on /data is what the Space's entrypoint launches at boot;
		// the hfsd role installs and starts it.
		if uploaded, err := target.Upload(ctx, a.cfg.HfsdBinary, remoteHfsdPersist); err != nil {
			return err
		} else if uploaded {
			fmt.Println("uploaded hfsd to", remoteHfsdPersist)
		}
		return target.Run(ctx, pb)
	}

	// hfsd is already running (it's how we get in), so it upgrades itself
	// and the hfsd role has nothing to do.
	rc, err := a.remote(ctx)
	if err != nil {
		return err
	}
	if upgraded, err := rc.Upgrade(ctx, a.cfg.HfsdBinary, false); err != nil {
		return fmt.Errorf("upgrade hfsd: %w", err)
	} else if upgraded {
		fmt.Println("upgraded hfsd")
		if err := a.waitHfsdVersion(ctx, rc); err != nil {
			return err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	info, err := a.hf.Info(ctx)
	if err != nil {
		return err
	}
	vars["hfsd_managed"] = false
	return pb.RunHfsd(ctx, a.hf.Space(), info.Host, self, a.cfg.Path())
}

// waitHfsdVersion waits until the running hfsd is the local binary. Just
// asking whether hfsd answers would race the re-exec and hear from the old one.
func (a *app) waitHfsdVersion(ctx context.Context, rc *daemon.RemoteClient) error {
	b, err := os.ReadFile(a.cfg.HfsdBinary)
	if err != nil {
		return err
	}
	want := fmt.Sprintf("%x", sha256.Sum256(b))

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var last error
	for {
		got, err := rc.Version(ctx)
		if err == nil && got == want {
			return nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hfsd did not come back after upgrading: %v", last)
		case <-time.After(time.Second):
		}
	}
}

// waitHfsd waits for hfsd to answer, e.g. after the Space starts.
func (a *app) waitHfsd(ctx context.Context, rc *daemon.RemoteClient) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		if _, err := rc.Version(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("hfsd did not come back after upgrading")
		case <-time.After(time.Second):
		}
	}
}

func (a *app) provision(ctx context.Context, o provisionOpts) error {
	return a.playbook(ctx, "provision.yml", o, nil)
}

func (a *app) provisionCmd() *cobra.Command {
	var opts provisionOpts
	cmd := &cobra.Command{
		Use:   "provision [profile] [-- ansible-playbook args]",
		Short: "Build llama.cpp and (re)start llama-server with ansible",
		RunE: func(cmd *cobra.Command, args []string) error {
			args, extra := passthrough(cmd, args)
			if len(args) > 1 {
				return fmt.Errorf("accepts at most 1 profile, received %d", len(args))
			}
			if len(args) == 1 {
				opts.profile = args[0]
			}
			return a.playbook(cmd.Context(), "provision.yml", opts, extra)
		},
	}
	opts.bind(cmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "profiles",
		Short: "List available profiles",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			names, err := a.cfg.Profiles()
			if err != nil {
				return err
			}
			for _, n := range names {
				if n == a.cfg.Profile {
					n += " (default)"
				}
				fmt.Println(n)
			}
			return nil
		},
	})
	return cmd
}

func (a *app) benchCmd() *cobra.Command {
	var opts provisionOpts
	cmd := &cobra.Command{
		Use:   "bench [profile] [-- ansible-playbook args]",
		Short: "Run llama-bench on the Space and fetch the results",
		Long: "Builds the profile's llama.cpp, stops llama-server to free the GPU, runs\n" +
			"llama-bench, and fetches the JSON results. The server is left stopped;\n" +
			"bring it back with `hfs provision --tags server`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			args, extra := passthrough(cmd, args)
			if len(args) > 1 {
				return fmt.Errorf("accepts at most 1 profile, received %d", len(args))
			}
			if len(args) == 1 {
				opts.profile = args[0]
			}
			return a.playbook(cmd.Context(), "bench.yml", opts, extra)
		},
	}
	opts.bind(cmd)
	cmd.Flags().Lookup("tags").Hidden = true
	return cmd
}

func (a *app) sshCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "ssh [-- command]",
		Aliases: []string{"shell"},
		Short:   "Open a tmux session on the Space, or run a command on a terminal",
		Long: "Open a tmux session on the Space, or run a command on a terminal.\n\n" +
			"Goes through hfsd's websocket by default, so it needs no Dev Mode and isn't\n" +
			"dropped when idle. With --ssh it uses the Dev Mode SSH gateway.",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, command := passthrough(cmd, args)
			if len(command) == 0 {
				command = []string{"tmux", "new", "-A", "-s", defaultSession}
			}
			if a.useSSH() {
				return a.interactive(cmd.Context(), true, command...)
			}
			return a.terminal(cmd.Context(), command)
		},
	}
}

// terminal runs argv on a remote pty attached to the local terminal.
func (a *app) terminal(ctx context.Context, argv []string) error {
	rc, err := a.remote(ctx)
	if err != nil {
		return err
	}
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errors.New("stdin is not a terminal; use `hfs exec` for non-interactive commands")
	}
	cols, rows, err := term.GetSize(fd)
	if err != nil {
		return err
	}

	resize := make(chan [2]uint16, 1)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			if c, r, err := term.GetSize(fd); err == nil {
				resize <- [2]uint16{uint16(r), uint16(c)}
			}
		}
	}()

	// Raw mode hands every keystroke, ^C included, to the remote side.
	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	code, err := rc.ExecTTY(ctx, daemon.ExecRequest{
		Argv: argv,
		Dir:  remoteWorkdir,
		Term: os.Getenv("TERM"),
		Rows: uint16(rows),
		Cols: uint16(cols),
	}, os.Stdin, os.Stdout, resize)
	term.Restore(fd, state)

	if err != nil {
		return err
	}
	if code != 0 {
		return exitError{code}
	}
	return nil
}

func (a *app) ctlCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "ctl [hfsd args]",
		Short:              "Run the hfsd client on the Space (ps, restart llama-server, logs build -f, ...)",
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				args = []string{"ps"}
			}
			return a.run(cmd.Context(), false, append([]string{remoteHfsd}, args...)...)
		},
	}
}

func (a *app) interactive(ctx context.Context, tty bool, command ...string) error {
	target, err := a.target(ctx)
	if err != nil {
		return err
	}
	c := target.SSH(ctx, tty, command...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c.Run()
}

func (a *app) logsCmd() *cobra.Command {
	var (
		follow bool
		tail   int
	)
	cmd := &cobra.Command{
		Use:   "logs [NAME|run|build-image]",
		Short: "Show an hfsd process log (default llama-server; also build, bench, ...) or the Space's container logs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "llama-server"
			if len(args) > 0 {
				name = args[0]
			}
			switch name {
			case "run", "build-image":
				// The API stream stays open until interrupted.
				return a.hf.Logs(cmd.Context(), name == "build-image", func(l hf.LogLine) {
					fmt.Println(strings.TrimRight(l.Data, "\n"))
				})
			}
			command := []string{remoteHfsd, "logs", name, "-n", strconv.Itoa(tail)}
			if follow {
				command = append(command, "-f")
			}
			return a.run(cmd.Context(), false, command...)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until the process finishes")
	cmd.Flags().IntVarP(&tail, "tail", "n", 200, "lines from the end to start at (0 for everything)")
	return cmd
}

// shellQuote joins argv into one string for the remote shell, since ssh
// flattens its arguments.
func shellQuote(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
