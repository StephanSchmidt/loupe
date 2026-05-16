package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/StephanSchmidt/loupe/cmd/cmdbaseline"
	"github.com/StephanSchmidt/loupe/cmd/cmdexport"
	"github.com/StephanSchmidt/loupe/cmd/cmdinit"
	"github.com/StephanSchmidt/loupe/cmd/cmdpresent"
	"github.com/StephanSchmidt/loupe/cmd/cmdrender"
	"github.com/StephanSchmidt/loupe/cmd/cmdrun"
	"github.com/StephanSchmidt/loupe/cmd/cmdstats"
	"github.com/StephanSchmidt/loupe/cmd/cmdstatus"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "loupe",
		Short: "Diagnostic CLI that measures AI coding-assistant impact",
		Long: `Loupe analyzes git + Jira data and produces a reveal.js exec deck
showing the impact of AI coding assistants across an engineering org.

Run once for a baseline (` + "`loupe baseline`" + `), then weekly to track
impact (` + "`loupe run`" + `). Output is a slide deck the CTO presents in the
exec meeting — no SaaS, no login, no data leaves the customer environment.`,
		Version:      fmt.Sprintf("%s (%s, %s)", version, commit, date),
		SilenceUsage: true,
	}

	root.AddCommand(
		cmdinit.BuildInitCmd(),
		cmdbaseline.BuildBaselineCmd(),
		cmdrun.BuildRunCmd(),
		cmdrender.BuildRenderCmd(),
		cmdstatus.BuildStatusCmd(version),
		cmdstats.BuildStatsCmd(),
		cmdpresent.BuildPresentCmd(),
		cmdexport.BuildExportCmd(),
	)

	return root
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
