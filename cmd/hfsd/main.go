// hfsd runs inside the Space. `hfsd serve` is the daemon; every other
// subcommand is a client that talks to it over a unix socket.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/aldehir/hfs/internal/daemon"
	"github.com/spf13/cobra"
)

// exitError carries a process's exit code out through cobra.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	home := os.Getenv("HFSD_HOME")
	if home == "" {
		userHome, _ := os.UserHomeDir()
		home = filepath.Join(userHome, ".hfs")
	}

	root := &cobra.Command{
		Use:           "hfsd",
		Short:         "Process manager and HTTP front door for a llama.cpp Space",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&home, "home", home, "state directory (socket, logs, specs)")
	client := func() *daemon.Client { return daemon.NewClient(filepath.Join(home, "hfsd.sock")) }

	root.AddCommand(
		serveCmd(&home),
		runCmd(client),
		waitCmd(client),
		psCmd(client),
		statusCmd(client),
		logsCmd(client),
		actionCmd(client, "stop", "Stop a process"),
		actionCmd(client, "start", "Start a finished process again"),
		actionCmd(client, "restart", "Restart a process"),
		rmCmd(client),
		shutdownCmd(client),
	)

	if err := root.ExecuteContext(ctx); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serveCmd(home *string) *cobra.Command {
	var listen, upstream, persist string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mgr, err := daemon.NewManager(*home)
			if err != nil {
				return err
			}
			srv := &daemon.Server{
				Manager:  mgr,
				Upstream: upstream,
				Token:    remoteToken(persist),
				Persist:  persist,
				Quit:     make(chan struct{}),
				Reexec:   make(chan string, 1),
			}
			// Managed processes and remote shells inherit our environment;
			// the key to this API shouldn't be in it.
			os.Unsetenv("HFSD_TOKEN")
			if srv.Token == "" {
				log.Print("remote api disabled: no $HFSD_TOKEN or token file")
			}

			sock, err := daemon.ListenUnix(filepath.Join(*home, "hfsd.sock"))
			if err != nil {
				return err
			}
			control := &http.Server{Handler: srv.Control()}
			go control.Serve(sock)

			// The front door is best-effort: when hfsd is started by hand
			// next to another server holding the port, it still manages
			// processes.
			public := &http.Server{Handler: srv.Public()}
			if l, err := net.Listen("tcp", listen); err != nil {
				log.Printf("not serving http: %v", err)
			} else {
				log.Printf("serving http on %s, proxying to %s", listen, upstream)
				go public.Serve(l)
			}

			if err := mgr.Restore(); err != nil {
				log.Printf("restore: %v", err)
			}

			var reexec string
			select {
			case <-cmd.Context().Done():
			case <-srv.Quit:
			case reexec = <-srv.Reexec:
			}
			log.Print("shutting down")
			mgr.Shutdown()
			daemon.Shutdown(context.Background(), control, public)
			if reexec != "" {
				// The token was dropped from our environment at startup; the
				// next hfsd needs it back.
				env := os.Environ()
				if token := srv.Token; token != "" {
					env = append(env, "HFSD_TOKEN="+token)
				}
				return syscall.Exec(reexec, os.Args, env)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":8000", "public http address")
	cmd.Flags().StringVar(&persist, "persist", "/data/hfs/hfsd", "persistent copy of the binary, updated on upgrade")
	cmd.Flags().StringVar(&upstream, "upstream", "127.0.0.1:8080", "llama-server address to proxy to")
	return cmd
}

// remoteToken returns $HFSD_TOKEN (a Space secret), falling back to a token
// file next to the persistent binary.
func remoteToken(persist string) string {
	if t := os.Getenv("HFSD_TOKEN"); t != "" {
		return t
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(persist), "token"))
	return strings.TrimSpace(string(b))
}

func runCmd(client func() *daemon.Client) *cobra.Command {
	var (
		spec    daemon.Spec
		env     []string
		replace bool
		wait    bool
		follow  bool
	)
	cmd := &cobra.Command{
		Use:   "run NAME [flags] -- COMMAND [ARGS...]",
		Short: "Start a job or service; a no-op if it is already running with the same spec",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec.Name, spec.Command = args[0], args[1:]
			spec.Env = map[string]string{}
			for _, kv := range env {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("invalid --env %q, want KEY=VALUE", kv)
				}
				spec.Env[k] = v
			}
			if spec.Dir != "" {
				spec.Dir, _ = filepath.Abs(spec.Dir)
			}

			c := client()
			result, _, err := c.Apply(cmd.Context(), spec, replace)
			if err != nil {
				return err
			}
			fmt.Println(result)
			if follow {
				tail := 0
				if result == "unchanged" {
					tail = 50
				}
				if err := c.Logs(cmd.Context(), spec.Name, tail, true, os.Stdout); err != nil {
					return err
				}
			}
			if wait || follow {
				return waitFor(cmd.Context(), c, spec.Name, !follow, 0)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&spec.Restart, "restart", daemon.RestartNever, "restart policy: never, on-failure, always")
	f.StringVar(&spec.Dir, "dir", "", "working directory")
	f.StringArrayVar(&env, "env", nil, "environment variable KEY=VALUE (repeatable)")
	f.BoolVar(&replace, "replace", false, "replace a running process whose spec differs")
	f.BoolVar(&wait, "wait", false, "wait for the process to finish and exit with its status")
	f.BoolVar(&follow, "follow", false, "like --wait, but stream the log")
	return cmd
}

// exitStillRunning is what `wait --timeout` exits with when time runs out
// (EX_TEMPFAIL), so callers can tell "not yet" from "failed".
const exitStillRunning = 75

// waitFor blocks until the process finishes and mirrors its exit status. On
// failure it prints the end of the log, so the caller sees why.
func waitFor(ctx context.Context, c *daemon.Client, name string, showTail bool, timeout time.Duration) error {
	waitCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	st, err := c.Wait(waitCtx, name)
	if err != nil {
		if ctx.Err() == nil && waitCtx.Err() != nil {
			fmt.Fprintf(os.Stderr, "%s: still running\n", name)
			return exitError{exitStillRunning}
		}
		return err
	}
	if st.State == daemon.StateExited {
		return nil
	}
	if showTail {
		c.Logs(ctx, name, 40, false, os.Stderr)
	}
	fmt.Fprintf(os.Stderr, "%s: %s\n", name, describe(st))
	if st.ExitCode != nil && *st.ExitCode > 0 {
		return exitError{*st.ExitCode}
	}
	return exitError{1}
}

func waitCmd(client func() *daemon.Client) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait NAME",
		Short: "Wait for a process to finish and exit with its status",
		Long: "Wait for a process to finish and exit with its status.\n\n" +
			"With --timeout, exits 75 if the process is still running. The Dev Mode SSH\n" +
			"gateway drops sessions that are silent for 5 minutes, so remote callers\n" +
			"should poll with a short timeout instead of blocking.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return waitFor(cmd.Context(), client(), args[0], true, timeout)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up waiting after this long and exit 75")
	return cmd
}

func psCmd(client func() *daemon.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List processes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := client().List(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(list)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tSTATE\tRESTART\tCOMMAND")
			for _, st := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", st.Spec.Name, describe(st), st.Spec.Restart, truncate(strings.Join(st.Spec.Command, " "), 60))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func statusCmd(client func() *daemon.Client) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status NAME",
		Short: "Show one process; the first word is its state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := client().Status(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(st)
			}
			fmt.Println(describe(st))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "output JSON")
	return cmd
}

func logsCmd(client func() *daemon.Client) *cobra.Command {
	var (
		follow bool
		tail   int
	)
	cmd := &cobra.Command{
		Use:   "logs NAME",
		Short: "Show a process's log",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().Logs(cmd.Context(), args[0], tail, follow, os.Stdout)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming until the process finishes")
	cmd.Flags().IntVarP(&tail, "tail", "n", 200, "lines from the end to start at (0 for everything)")
	return cmd
}

func actionCmd(client func() *daemon.Client, action, short string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " NAME",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := client().Action(cmd.Context(), args[0], action)
			if err != nil {
				return err
			}
			fmt.Printf("%s: %s\n", args[0], describe(st))
			return nil
		},
	}
}

func rmCmd(client func() *daemon.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME",
		Short: "Stop a process and forget it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().Remove(cmd.Context(), args[0])
		},
	}
}

func shutdownCmd(client func() *daemon.Client) *cobra.Command {
	return &cobra.Command{
		Use:   "shutdown",
		Short: "Stop every process and exit the daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return client().Shutdown(cmd.Context())
		},
	}
}

func describe(st daemon.Status) string {
	s := st.State
	switch {
	case st.State == daemon.StateRunning && st.StartedAt != nil:
		s += fmt.Sprintf(" pid=%d up=%s", st.PID, time.Since(*st.StartedAt).Round(time.Second))
	case st.ExitCode != nil:
		s += fmt.Sprintf(" code=%d", *st.ExitCode)
	}
	if st.Restarts > 0 {
		s += fmt.Sprintf(" restarts=%d", st.Restarts)
	}
	if st.Error != "" {
		s += " (" + st.Error + ")"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
