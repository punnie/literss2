// Command literss is a minimalist river-of-news reader backed by Miniflux.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"literss/internal/config"
	"literss/internal/extract"
	"literss/internal/miniflux"
	"literss/internal/store"
	"literss/internal/web"
)

const inviteTTL = 24 * time.Hour

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: literss [-config FILE] COMMAND [ARGS]

Commands:
  serve              run the web server (default)
  invite NAME        print a single-use sign-in link for NAME, creating the user if needed
  users              list users
  delete-user NAME   delete a user, their passkeys and sessions
  sync               reconcile configured rivers and feeds with Miniflux, then exit

The config file defaults to $LITERSS_CONFIG, then ./config.toml.
`)
}

func main() {
	defaultConfig := os.Getenv("LITERSS_CONFIG")
	if defaultConfig == "" {
		defaultConfig = "config.toml"
	}
	configPath := flag.String("config", defaultConfig, "path to the TOML configuration")
	debug := flag.Bool("debug", false, "verbose logging")
	flag.Usage = usage
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := run(*configPath, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "literss:", err)
		os.Exit(1)
	}
}

func run(configPath string, args []string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "literss.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, cfg, st)
	case "invite":
		if len(args) != 1 {
			return errors.New("usage: literss invite NAME")
		}
		u, err := st.EnsureUser(ctx, args[0])
		if err != nil {
			return err
		}
		token, err := st.CreateLink(ctx, u.ID, inviteTTL)
		if err != nil {
			return err
		}
		fmt.Printf("%s/link/%s\n", cfg.BaseURL, token)
		fmt.Fprintf(os.Stderr, "Link for %q, valid once until %s.\n", u.Name, time.Now().Add(inviteTTL).Format(time.RFC1123))
		return nil
	case "users":
		users, err := st.Users(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tPASSKEYS\tSESSIONS\tCREATED")
		for _, u := range users {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", u.Name, u.Passkeys, u.Sessions, u.CreatedAt.Format("2006-01-02"))
		}
		return tw.Flush()
	case "delete-user":
		if len(args) != 1 {
			return errors.New("usage: literss delete-user NAME")
		}
		return st.DeleteUser(ctx, args[0])
	case "sync":
		return miniflux.NewRegistry(cfg, miniflux.New(cfg.Miniflux.URL, cfg.Miniflux.Token)).Sync(ctx)
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func serve(ctx context.Context, cfg *config.Config, st *store.Store) error {
	mf := miniflux.New(cfg.Miniflux.URL, cfg.Miniflux.Token)
	reg := miniflux.NewRegistry(cfg, mf)
	ex, err := extract.New(cfg, st, mf, reg, filepath.Join(cfg.DataDir, "images"))
	if err != nil {
		return err
	}
	defer ex.Close()
	srv, err := web.New(cfg, st, mf, reg, ex)
	if err != nil {
		return err
	}

	go reg.Run(ctx)
	go ex.Images.Run(ctx)
	go ex.Prefetch(ctx)
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			if err := st.Cleanup(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("cleanup failed", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Listen, "url", cfg.BaseURL)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}
