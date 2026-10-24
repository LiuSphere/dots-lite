package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"dots-duet-lite/internal/hub"
	"dots-duet-lite/internal/store"
)

func run(ctx context.Context, args []string, stderr io.Writer) (err error) {
	flags := flag.NewFlagSet("hub", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "", "private JSON binding configuration")
	state := flags.String("state", "state/hub.db", "hub SQLite path")
	socket := flags.String("socket", "state/hub.sock", "profile Unix socket path")
	listen := flags.String("listen", "127.0.0.1:8025", "executor WebSocket listen address")
	if err = flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New("-config is required; configure profile and executor bindings before starting")
	}
	info, err := os.Stat(*configPath)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("binding configuration must be private (mode 0600)")
	}
	f, err := os.Open(*configPath)
	if err != nil {
		return err
	}
	var cfg hub.Config
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	decodeErr := d.Decode(&cfg)
	if decodeErr == nil {
		if next := d.Decode(new(any)); !errors.Is(next, io.EOF) {
			decodeErr = errors.New("configuration must contain exactly one JSON object")
		}
	}
	if err = errors.Join(decodeErr, f.Close()); err != nil {
		return err
	}
	db, err := store.Open(*state)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	h, err := hub.New(cfg, db)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, h.Close()) }()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	tcp, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer tcp.Close()
	server := &http.Server{Handler: h.WebsocketHandler(), ReadHeaderTimeout: 5 * time.Second}
	results := make(chan error, 3)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if e := h.Reconcile(ctx); e != nil {
					if ctx.Err() == nil {
						results <- e
					}
					return
				}
			}
		}
	}()
	go func() { results <- h.ServeUnix(ctx, *socket) }()
	go func() {
		e := server.Serve(tcp)
		if errors.Is(e, http.ErrServerClosed) {
			e = nil
		}
		results <- e
	}()
	select {
	case err = <-results:
	case <-ctx.Done():
	}
	cancel()
	closeErr := h.Close()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return errors.Join(err, closeErr, server.Shutdown(shutdown))
}
