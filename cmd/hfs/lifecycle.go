package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/aldehir/hfs/internal/daemon"
	"github.com/aldehir/hfs/internal/hf"
	"github.com/spf13/cobra"
)

const stateFile = "/home/user/.hfs/state.json"

// Stages from which the Space will reach RUNNING on its own.
var startingStages = map[string]bool{
	"BUILDING":             true,
	"APP_STARTING":         true,
	"RUNNING_BUILDING":     true,
	"RUNNING_APP_STARTING": true,
}

var failedStages = map[string]bool{
	"BUILD_ERROR":   true,
	"CONFIG_ERROR":  true,
	"RUNTIME_ERROR": true,
	"NO_APP_FILE":   true,
	"DELETING":      true,
}

var errNoCapacity = errors.New("no hardware capacity")

const retryDelay = 2 * time.Minute

func (a *app) upCmd() *cobra.Command {
	var (
		retries     int
		hardware    string
		sleep       string
		noProvision bool
		restart     bool
		rebuild     bool
		timeout     time.Duration
		opts        provisionOpts
	)
	cmd := &cobra.Command{
		Use:   "up [profile]",
		Short: "Start the Space, enable Dev Mode, and provision it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if len(args) > 0 {
				opts.profile = args[0]
			}
			// Fail on a bad profile before touching the Space.
			if !noProvision {
				if _, err := a.cfg.ProfilePath(opts.profile); err != nil {
					return err
				}
			}

			rt, err := a.hf.Runtime(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("%s: %s on %s\n", a.hf.Space(), rt.Stage, rt.Hardware.Current)

			if sleep != "" {
				seconds, err := parseSleep(sleep)
				if err != nil {
					return err
				}
				if err := a.hf.SetSleepTime(ctx, seconds); err != nil {
					return err
				}
				fmt.Printf("sleep time set to %s\n", formatSleep(seconds))
			}
			if hardware != "" && hardware != rt.Hardware.Requested {
				fmt.Printf("requesting hardware %s\n", hardware)
				if err := a.hf.RequestHardware(ctx, hardware); err != nil {
					return err
				}
			}
			if a.useSSH() && !rt.DevMode {
				fmt.Println("enabling dev mode")
				if err := a.hf.SetDevMode(ctx, true); err != nil {
					return err
				}
			}
			restart = restart || rebuild
			if restart || (rt.Stage != "RUNNING" && !startingStages[rt.Stage]) {
				if rebuild {
					fmt.Println("rebuilding image and restarting space")
				} else {
					fmt.Println("restarting space")
				}
				if err := a.hf.Restart(ctx, rebuild); err != nil {
					return err
				}
			}

			// Restarting gives up the GPU, and on scarce hardware the Space
			// can fail to get one back. Only another restart fixes that.
			for attempt := 1; ; attempt++ {
				err := a.waitRunning(ctx, timeout, restart)
				if err == nil {
					break
				}
				if !errors.Is(err, errNoCapacity) || attempt > retries {
					return err
				}
				fmt.Printf("no hardware capacity; retrying in %s (%d/%d)\n", retryDelay, attempt, retries)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(retryDelay):
				}
				if err := a.hf.Restart(ctx, false); err != nil {
					return err
				}
				restart = true
			}
			if a.useSSH() {
				target, err := a.target(ctx)
				if err != nil {
					return err
				}
				fmt.Printf("waiting for ssh (%s)\n", target)
				if err := target.WaitSSH(ctx, 5*time.Minute); err != nil {
					return err
				}
			} else {
				rc, err := a.remote(ctx)
				if err != nil {
					return err
				}
				fmt.Println("waiting for hfsd")
				if err := a.waitHfsd(ctx, rc); err != nil {
					return err
				}
			}
			if noProvision {
				return nil
			}
			return a.provision(ctx, opts)
		},
	}
	cmd.Flags().StringVar(&hardware, "hw", "", "hardware flavor to request (e.g. l40sx1, cpu-basic)")
	cmd.Flags().StringVar(&sleep, "sleep", "", "idle sleep time (e.g. 30m, 1h, never)")
	cmd.Flags().BoolVar(&restart, "restart", false, "restart even if running (same image; wipes the container)")
	cmd.Flags().BoolVar(&rebuild, "rebuild", false, "factory reboot: rebuild the image from the Space repo's HEAD, then restart (a Dev Mode Space ignores pushes until this)")
	cmd.Flags().IntVar(&retries, "retries", 10, "restarts to attempt when the Space can't get hardware")
	cmd.Flags().BoolVar(&noProvision, "no-provision", false, "bring the Space up without running ansible")
	cmd.Flags().DurationVar(&timeout, "timeout", 20*time.Minute, "how long to wait for the Space to reach RUNNING")
	opts.bind(cmd)
	return cmd
}

// waitRunning polls until the Space is RUNNING with Dev Mode. After a restart
// the API keeps reporting the old container as RUNNING for a moment, so
// leaveFirst waits for the stage to change before trusting it.
func (a *app) waitRunning(ctx context.Context, timeout time.Duration, leaveFirst bool) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	grace := time.Now().Add(time.Minute)
	for {
		rt, err := a.hf.Runtime(ctx)
		if err != nil {
			return err
		}
		if rt.Stage != last {
			fmt.Printf("stage: %s\n", rt.Stage)
			last = rt.Stage
		}
		// Dev Mode is what gives us SSH, so RUNNING alone isn't enough.
		if rt.Stage != "RUNNING" {
			leaveFirst = false
		}
		if rt.Stage == "RUNNING" && rt.DevMode && (!leaveFirst || time.Now().After(grace)) {
			return nil
		}
		if failedStages[rt.Stage] && strings.Contains(rt.ErrorMessage, "Scheduling failure") {
			return fmt.Errorf("%w: %s", errNoCapacity, rt.ErrorMessage)
		}
		if failedStages[rt.Stage] {
			return fmt.Errorf("space is in %s: %s (see `hfs logs run` / `hfs logs build-image`)", rt.Stage, rt.ErrorMessage)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("space still %s after %s", last, timeout)
		case <-time.After(5 * time.Second):
		}
	}
}

func (a *app) downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Pause the Space",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.hf.Pause(cmd.Context()); err != nil {
				return err
			}
			fmt.Printf("%s: paused\n", a.hf.Space())
			return nil
		},
	}
}

func (a *app) statusCmd() *cobra.Command {
	var noRemote bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show Space, server, and provisioning state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			info, err := a.hf.Info(ctx)
			if err != nil {
				return err
			}
			rt := info.Runtime
			row("space", info.ID)
			row("stage", rt.Stage)
			if rt.ErrorMessage != "" {
				row("error", rt.ErrorMessage)
			}
			hw := rt.Hardware.Current
			if rt.Hardware.Requested != "" && rt.Hardware.Requested != hw {
				hw = fmt.Sprintf("%s (requested %s)", hw, rt.Hardware.Requested)
			}
			row("hardware", hw)
			row("dev mode", strconv.FormatBool(rt.DevMode))
			row("sleep", formatSleep(rt.SleepTime))
			if rt.Stage != "RUNNING" {
				return nil
			}

			row("server", a.health(ctx, info.Host))
			if noRemote {
				return nil
			}
			out, err := a.readRemote(ctx, info, stateFile)
			if err != nil || strings.TrimSpace(out) == "" {
				row("provisioned", "no")
				return nil
			}
			var st struct {
				Profile string `json:"profile"`
				Repo    string `json:"llama_repo"`
				Ref     string `json:"llama_ref"`
				SHA     string `json:"llama_sha"`
				Model   string `json:"model"`
				At      string `json:"provisioned_at"`
			}
			if err := json.Unmarshal([]byte(out), &st); err != nil {
				return fmt.Errorf("parse %s: %w", stateFile, err)
			}
			row("profile", st.Profile)
			row("llama.cpp", fmt.Sprintf("%s@%s (%.10s)", st.Repo, st.Ref, st.SHA))
			row("model", st.Model)
			row("provisioned", st.At)
			return nil
		},
	}
	cmd.Flags().BoolVar(&noRemote, "no-remote", false, "skip reading provisioning state from the Space")
	return cmd
}

// readRemote returns a remote file's contents over the active transport.
func (a *app) readRemote(ctx context.Context, info *hf.Info, path string) (string, error) {
	if a.useSSH() {
		return a.targetFor(info).Output(ctx, "cat "+path+" 2>/dev/null")
	}
	rc, err := a.remote(ctx)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	_, err = rc.Exec(ctx, daemon.ExecRequest{Argv: []string{"cat", path}}, nil, &out, io.Discard)
	return out.String(), err
}

// health reports llama-server's state through the Space's public host. nginx
// serves a placeholder with X-Upstream-Status: offline when the server is down.
func (a *app) health(ctx context.Context, host string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := a.hf.Get(ctx, host, "/health")
	if err != nil {
		return "unreachable: " + err.Error()
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	switch {
	case resp.Header.Get("X-Upstream-Status") == "offline":
		return "offline"
	case resp.StatusCode == http.StatusOK:
		return "healthy"
	case resp.StatusCode == http.StatusServiceUnavailable:
		return "loading model"
	default:
		return resp.Status
	}
}

func (a *app) infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show endpoint and connection details",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			info, err := a.hf.Info(cmd.Context())
			if err != nil {
				return err
			}
			row("space", hfURL(info))
			row("endpoint", info.Host)
			row("openai", info.Host+"/v1")
			row("ssh", a.targetFor(info).String())
			if info.Private {
				row("auth", "private space: send your HF token as the bearer / API key")
			}
			return nil
		},
	}
}

func hfURL(info *hf.Info) string { return "https://huggingface.co/spaces/" + info.ID }

func row(key, value string) { fmt.Printf("%-12s %s\n", key, value) }

func parseSleep(s string) (int, error) {
	if s == "never" || s == "-1" {
		return -1, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid sleep time %q, want a duration like 30m or \"never\"", s)
	}
	return int(d.Seconds()), nil
}

func formatSleep(seconds int) string {
	if seconds <= 0 {
		return "never"
	}
	return (time.Duration(seconds) * time.Second).String()
}
