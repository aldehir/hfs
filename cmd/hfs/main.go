package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aldehir/hfs/internal/config"
	"github.com/aldehir/hfs/internal/hf"
	"github.com/aldehir/hfs/internal/remote"
	"github.com/spf13/cobra"
)

type app struct {
	configPath string
	ssh        bool
	cfg        *config.Config
	hf         *hf.Client
}

func (a *app) init(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	client, err := hf.New(cfg.Space, hf.Token())
	if err != nil {
		return err
	}
	a.cfg, a.hf = cfg, client
	return nil
}

// target resolves the Dev Mode SSH endpoint; the SSH user is the Space's subdomain.
func (a *app) target(ctx context.Context) (remote.Target, error) {
	info, err := a.hf.Info(ctx)
	if err != nil {
		return remote.Target{}, err
	}
	return a.targetFor(info), nil
}

func (a *app) targetFor(info *hf.Info) remote.Target {
	return remote.Target{
		Host:         a.cfg.SSH.Host,
		User:         info.Subdomain,
		IdentityFile: a.cfg.SSH.IdentityFile,
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := &app{}
	root := &cobra.Command{
		Use:               "hfs",
		Short:             "Control a Hugging Face Space running llama.cpp",
		SilenceUsage:      true,
		SilenceErrors:     true,
		PersistentPreRunE: a.init,
	}
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "path to hfs.yml")
	root.PersistentFlags().BoolVar(&a.ssh, "ssh", false, "reach the Space over Dev Mode SSH instead of hfsd")
	root.AddCommand(
		a.upCmd(),
		a.downCmd(),
		a.statusCmd(),
		a.infoCmd(),
		a.sshCmd(),
		a.provisionCmd(),
		a.benchCmd(),
		a.logsCmd(),
		a.ctlCmd(),
		a.execCmd(),
		a.putCmd(),
		a.fetchCmd(),
		a.tokenCmd(),
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
