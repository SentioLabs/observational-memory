package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"unicode/utf8"

	"github.com/sentiolabs/go-selfupdate/cobracmd"
	"github.com/sentiolabs/observational-memory/internal/adapters/codex"
	"github.com/sentiolabs/observational-memory/internal/ledger"
	"github.com/sentiolabs/observational-memory/internal/update"
	"github.com/spf13/cobra"
)

var Version = "dev"

const ProtocolVersion = 1

type options struct{ store, session string }

func New() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{Use: "om", Short: "Evidence-backed memory for coding agents", Version: Version, SilenceUsage: true, SilenceErrors: true}
	root.SetVersionTemplate("om {{.Version}}\n")
	root.PersistentFlags().StringVar(&opts.store, "store", os.Getenv("OBSERVATIONAL_MEMORY_STORE"), "Writable memory store")
	root.PersistentFlags().StringVar(&opts.session, "session", "", "Exact session identity")
	root.AddCommand(&cobra.Command{Use: "version", Short: "Show the runtime version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "om "+Version)
		return err
	}})
	root.AddCommand(&cobra.Command{Use: "capabilities", Short: "Show the CLI contract and supported clients", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return output(cmd, map[string]any{"version": Version, "protocol_version": ProtocolVersion, "ledger_schema": 1, "clients": []string{"codex"}})
	}})
	var protocol int
	var compatibleClient string
	compatibility := &cobra.Command{Use: "check-compatibility", Short: "Check a plugin's runtime requirements", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if protocol != ProtocolVersion || compatibleClient != "codex" {
			return fmt.Errorf("unsupported protocol/client; this runtime supports protocol %d and codex", ProtocolVersion)
		}
		return output(cmd, map[string]bool{"compatible": true})
	}}
	compatibility.Flags().IntVar(&protocol, "protocol", 0, "Required CLI protocol")
	compatibility.Flags().StringVar(&compatibleClient, "client", "", "Required client adapter")
	root.AddCommand(compatibility)
	var client string
	hook := &cobra.Command{Use: "hook", Short: "Handle a client lifecycle event", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if client != "codex" {
			return fmt.Errorf("hook requires --client codex")
		}
		result, err := runHook(cmd.Context(), cmd.InOrStdin(), opts.store)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "om: %v\n", err)
			result = map[string]any{"systemMessage": "Observational memory unavailable; use its status command to diagnose. The task can continue."}
		}
		return output(cmd, result)
	}}
	hook.Flags().StringVar(&client, "client", "", "Client event contract (codex)")
	root.AddCommand(hook)
	core := func(use, short string, args cobra.PositionalArgs, run func(*cobra.Command, *ledger.Ledger, []string) (any, error)) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: args, RunE: func(cmd *cobra.Command, args []string) error {
			if opts.store == "" {
				return fmt.Errorf("set --store or OBSERVATIONAL_MEMORY_STORE")
			}
			if opts.session == "" {
				return fmt.Errorf("--session is required")
			}
			l, err := ledger.Open(cmd.Context(), opts.store, opts.session)
			if err != nil {
				return err
			}
			defer l.Close()
			value, err := run(cmd, l, args)
			if err != nil {
				return err
			}
			return output(cmd, value)
		}}
	}
	root.AddCommand(core("pending", "Read the next source chunk", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) { return l.Pending() }))
	root.AddCommand(core("status", "Inspect session memory", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) { return l.Status() }))
	root.AddCommand(core("prime", "Load prepared memory and checkpoint status", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) { return l.Prime() }))
	root.AddCommand(core("capture", "Capture source JSON from stdin", cobra.NoArgs, func(cmd *cobra.Command, l *ledger.Ledger, _ []string) (any, error) {
		var capture struct {
			Kind string  `json:"kind"`
			Text string  `json:"text"`
			Key  *string `json:"key"`
		}
		data, err := input(cmd.InOrStdin())
		if err != nil {
			return nil, err
		}
		if err = ledger.Decode(data, &capture); err != nil {
			return nil, err
		}
		if capture.Key == nil {
			return nil, fmt.Errorf("capture key is required")
		}
		id, err := l.Capture(capture.Kind, capture.Text, *capture.Key)
		return map[string]string{"source_id": id}, err
	}))
	root.AddCommand(core("apply", "Apply checkpoint JSON from stdin", cobra.NoArgs, func(cmd *cobra.Command, l *ledger.Ledger, _ []string) (any, error) {
		var checkpoint ledger.Checkpoint
		data, err := input(cmd.InOrStdin())
		if err != nil {
			return nil, err
		}
		if err = ledger.Decode(data, &checkpoint); err != nil {
			return nil, err
		}
		return l.Apply(checkpoint)
	}))
	root.AddCommand(core("recall ID", "Recall memory and supporting evidence", cobra.ExactArgs(1), func(_ *cobra.Command, l *ledger.Ledger, args []string) (any, error) { return l.Recall(args[0]) }))
	var all bool
	view := core("view", "Show prepared memory", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) {
		if all {
			return l.Entries(true)
		}
		return l.View()
	})
	view.Flags().BoolVar(&all, "all", false, "Include retired entries as JSON")
	root.AddCommand(view)
	for _, name := range []string{"pause", "resume"} {
		root.AddCommand(core(name, name+" automatic memory", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) {
			if err := l.Pause(name == "pause"); err != nil {
				return nil, err
			}
			return l.Status()
		}))
	}
	var destination string
	fork := core("fork", "Copy a snapshot to a new session", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) { return l.Fork(destination) })
	fork.Flags().StringVar(&destination, "to-session", "", "Unused destination session identity")
	root.AddCommand(fork)
	var sourceStore, sourceSession string
	importMemory := core("import", "Import a snapshot, preserving this session's pending sources", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) {
		return l.Import(sourceStore, sourceSession)
	})
	importMemory.Flags().StringVar(&sourceStore, "from-store", "", "Explicit source store")
	importMemory.Flags().StringVar(&sourceSession, "from-session", "", "Exact source session identity")
	root.AddCommand(importMemory)
	root.AddCommand(cobracmd.New(update.New(Version)))
	return root
}
func input(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 4000001))
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("input must be valid UTF-8")
	}
	if len(data) > 4000000 {
		return nil, fmt.Errorf("input exceeds 4 MB limit")
	}
	return data, nil
}
func runHook(ctx context.Context, reader io.Reader, store string) (map[string]any, error) {
	if store == "" {
		return nil, fmt.Errorf("set --store or OBSERVATIONAL_MEMORY_STORE")
	}
	data, err := input(reader)
	if err != nil {
		return nil, err
	}
	var event map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&event); err != nil {
		return nil, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one hook event")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return codex.Handle(ctx, event, store, exe)
}
func output(cmd *cobra.Command, value any) error {
	if text, ok := value.(string); ok {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), text)
		return err
	}
	data, err := ledger.JSON(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
	return err
}
