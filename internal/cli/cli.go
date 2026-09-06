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

const ProtocolVersion = ledger.ProtocolVersionV2

type options struct{ store, session string }

func New() *cobra.Command {
	opts := &options{}
	root := &cobra.Command{Use: "om", Short: "Evidence-backed memory for coding agents", Version: Version, SilenceUsage: true, SilenceErrors: true}
	root.SetVersionTemplate("om {{.Version}}\n")
	root.PersistentFlags().StringVar(&opts.store, "store", os.Getenv("OBSERVATIONAL_MEMORY_STORE"), "Writable memory store")
	root.PersistentFlags().StringVar(&opts.session, "session", "", "Exact session identity")
	root.AddCommand(&cobra.Command{Use: "version", Short: "Show the runtime version", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return output(cmd, "om "+Version)
	}})
	root.AddCommand(&cobra.Command{Use: "capabilities", Short: "Show the CLI contract and supported clients", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return output(cmd, map[string]any{"version": Version, "protocol_version": ProtocolVersion, "ledger_schema": ledger.LedgerSchemaV2, "clients": []string{"codex"}})
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
	var pendingCursor string
	pending := core("pending", "Read a bounded pending evidence page", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) { return l.ReadPending(pendingCursor) })
	pending.Flags().StringVar(&pendingCursor, "cursor", "", "Opaque continuation token")
	root.AddCommand(pending)
	root.AddCommand(core("status", "Inspect session memory", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) { return l.Status() }))
	root.AddCommand(core("prime", "Load prepared memory and checkpoint status", cobra.NoArgs, func(_ *cobra.Command, l *ledger.Ledger, _ []string) (any, error) { return l.Prime() }))
	root.AddCommand(core("capture", "Capture source JSON from stdin", cobra.NoArgs, func(cmd *cobra.Command, l *ledger.Ledger, _ []string) (any, error) {
		var capture ledger.CaptureInput
		data, err := input(cmd.InOrStdin())
		if err != nil {
			return nil, err
		}
		if err = ledger.Decode(data, &capture); err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		if err = json.Unmarshal(data, &fields); err != nil {
			return nil, err
		}
		key, ok := fields["key"]
		if !ok || bytes.Equal(bytes.TrimSpace(key), []byte("null")) {
			return nil, fmt.Errorf("capture key is required")
		}
		return l.CaptureV2(capture)
	}))
	root.AddCommand(core("apply", "Apply checkpoint JSON from stdin", cobra.NoArgs, func(cmd *cobra.Command, l *ledger.Ledger, _ []string) (any, error) {
		data, err := input(cmd.InOrStdin())
		if err != nil {
			return nil, err
		}
		checkpoint, err := ledger.DecodeCheckpointV2(data)
		if err != nil {
			return nil, err
		}
		return l.ApplyV2(checkpoint)
	}))
	var recallCursor string
	recall := core("recall ID", "Recall a bounded memory and evidence page", cobra.ExactArgs(1), func(_ *cobra.Command, l *ledger.Ledger, args []string) (any, error) {
		return l.ReadRecall(args[0], recallCursor)
	})
	recall.Flags().StringVar(&recallCursor, "cursor", "", "Opaque continuation token")
	root.AddCommand(recall)
	var all bool
	var viewCursor string
	view := core("view", "Show prepared memory", cobra.NoArgs, func(cmd *cobra.Command, l *ledger.Ledger, _ []string) (any, error) {
		if all {
			return l.ReadEntries(viewCursor)
		}
		if cmd.Flags().Changed("cursor") {
			return nil, fmt.Errorf("--cursor requires view --all")
		}
		return l.View()
	})
	view.Flags().BoolVar(&all, "all", false, "Include retired entries as JSON")
	view.Flags().StringVar(&viewCursor, "cursor", "", "Opaque continuation token for --all")
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
	body, err := ledger.EncodeResponse(value)
	if err != nil {
		return err
	}
	_, err = cmd.OutOrStdout().Write(body)
	return err
}
