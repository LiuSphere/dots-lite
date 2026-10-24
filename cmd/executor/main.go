package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"dots-duet-lite/internal/executor"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "dots-duet-lite executor:", err)
		os.Exit(1)
	}
}

// run currently exposes only the first-stage, read-only app-server probe.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("executor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	probe := flags.Bool("probe", false, "initialize a separate local app-server and check account configuration without running a model")
	codex := flags.String("codex", "codex", "Codex executable path")
	timeout := flags.Duration("timeout", 15*time.Second, "deadline for the complete probe")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if !*probe || flags.NArg() != 0 {
		return errors.New("use -probe for the local read-only check; the remote execution service is not implemented")
	}
	if *timeout <= 0 {
		return errors.New("timeout must be greater than zero")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	client, err := executor.Start(ctx, executor.Options{
		Command: []string{*codex, "app-server", "--listen", "stdio://"},
		Stderr:  stderr,
	})
	if err != nil {
		return fmt.Errorf("initialize app-server: %w", err)
	}
	defer func() { err = errors.Join(err, client.Close()) }()
	accountJSON, err := client.Call(ctx, "account/read", map[string]bool{"refreshToken": false})
	if err != nil {
		return fmt.Errorf("read account configuration: %w", err)
	}
	// The native result includes personal account data. Emit only readiness facts.
	var account struct {
		Account            json.RawMessage `json:"account"`
		RequiresOpenAIAuth *bool           `json:"requiresOpenaiAuth"`
	}
	if err := json.Unmarshal(accountJSON, &account); err != nil {
		return fmt.Errorf("decode account configuration: %w", err)
	}
	if len(account.Account) == 0 || account.RequiresOpenAIAuth == nil {
		return errors.New("account/read result is missing required fields")
	}
	return json.NewEncoder(stdout).Encode(struct {
		Initialized         bool `json:"initialized"`
		AccountConfigured   bool `json:"accountConfigured"`
		RequiresOpenAIAuth  bool `json:"requiresOpenaiAuth"`
		InferenceVerified   bool `json:"inferenceVerified"`
		DesktopChatAttached bool `json:"desktopChatAttached"`
	}{
		Initialized:         true,
		AccountConfigured:   string(account.Account) != "null",
		RequiresOpenAIAuth:  *account.RequiresOpenAIAuth,
		InferenceVerified:   false,
		DesktopChatAttached: false,
	})
}
