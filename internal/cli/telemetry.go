package cli

import (
	"fmt"
	"github.com/sentiolabs/observational-memory/internal/ledger"
	"github.com/sentiolabs/observational-memory/internal/telemetry"
	"github.com/spf13/cobra"
)

func telemetryCommand(opts *options) *cobra.Command {
	root := &cobra.Command{Use: "telemetry", Short: "Opt-in local content-free field measurements"}
	for _, name := range []string{"enable", "disable", "status", "report"} {
		root.AddCommand(&cobra.Command{Use: name, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.store == "" {
				return fmt.Errorf("telemetry requires --store")
			}
			if name == "enable" || name == "disable" {
				if err := telemetry.Configure(opts.store, name == "enable"); err != nil {
					return telemetry.Unavailable(err)
				}
			}
			result, err := telemetry.Report(opts.store)
			if err != nil {
				return err
			}
			return output(cmd, result)
		}})
	}
	root.AddCommand(&cobra.Command{Use: "feedback recovered|forgot|repeated-work|correction", Short: "Record an explicit user rating, never model self-rating", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !telemetry.ValidFeedback(args[0]) || opts.session == "" || opts.store == "" {
			return fmt.Errorf("feedback requires store, session and a closed rating")
		}
		l, err := ledger.Open(cmd.Context(), opts.store, opts.session)
		if err != nil {
			return err
		}
		defer l.Close()
		paused, err := l.Paused()
		if err != nil || paused {
			return fmt.Errorf("feedback unavailable while memory is paused")
		}
		err = telemetry.Write(opts.store, telemetry.Input{Session: opts.session, Category: "feedback", Feedback: args[0], Version: Version})
		if err != nil {
			return telemetry.Unavailable(err)
		}
		return output(cmd, map[string]bool{"recorded": true})
	}})
	return root
}
func observeValue(o *telemetry.Input, value any) {
	var c *ledger.Coverage
	switch v := value.(type) {
	case ledger.Status:
		c = &v.Coverage
	case ledger.PendingPage:
		c = &v.Coverage
	case ledger.ReceiptV2:
		c = &v.Coverage
	}
	if c != nil {
		o.Coverage = telemetry.SummaryCoverage(c.Pending.Units, c.Pending.Bytes, c.Reviewed.Units, c.Reviewed.Bytes, c.Deferred.Units, c.Deferred.Bytes)
	}
}
