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
	"strings"
	"syscall"
	"time"

	"dots-lite/internal/executor"
	"dots-lite/internal/protocol"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "dots-lite executor:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("executor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	probe := flags.Bool("probe", false, "initialize a separate local app-server and check account configuration without running a model")
	configPath := flags.String("config", "", "private executor JSON configuration for the persistent control service")
	codex := flags.String("codex", "codex", "Codex executable path")
	timeout := flags.Duration("timeout", 15*time.Second, "deadline for the complete probe")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || (*probe && *configPath != "") || (!*probe && *configPath == "") {
		return errors.New("choose -probe or -config <private JSON file>")
	}
	if *configPath != "" {
		return runService(ctx, *configPath, stderr)
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

type serviceConfig struct {
	Scope     protocol.Scope    `json:"scope"`
	Folders   []protocol.Folder `json:"folders"`
	StatePath string            `json:"state_path"`
	HubURL    string            `json:"hub_url"`
	TokenFile string            `json:"token_file"`
	Codex     string            `json:"codex"`
}

func runService(ctx context.Context, configPath string, stderr io.Writer) (err error) {
	file, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("open executor configuration: %w", err)
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var cfg serviceConfig
	decodeErr := decoder.Decode(&cfg)
	if decodeErr == nil {
		var extra any
		if next := decoder.Decode(&extra); next == nil {
			decodeErr = errors.New("configuration must contain one JSON object")
		} else if !errors.Is(next, io.EOF) {
			decodeErr = next
		}
	}
	if err := errors.Join(decodeErr, file.Close()); err != nil {
		return fmt.Errorf("read executor configuration: %w", err)
	}
	if cfg.HubURL == "" || cfg.TokenFile == "" {
		return errors.New("hub_url and token_file are required")
	}
	tokenFile, err := os.Open(cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("open executor token: %w", err)
	}
	tokenBytes, readErr := io.ReadAll(io.LimitReader(tokenFile, 4097))
	if err := errors.Join(readErr, tokenFile.Close()); err != nil {
		return fmt.Errorf("read executor token: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(tokenBytes) > 4096 {
		return errors.New("executor token must contain 1 to 4096 bytes")
	}
	if cfg.Codex == "" {
		cfg.Codex = "codex"
	}
	service, err := executor.NewService(executor.ServiceConfig{
		Scope: cfg.Scope, Folders: cfg.Folders, StatePath: cfg.StatePath,
		Native: executor.Options{
			Command: []string{cfg.Codex, "app-server", "--listen", "stdio://"},
			Stderr:  stderr,
		},
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, service.Close()) }()
	return service.Run(ctx, cfg.HubURL, token)
}
