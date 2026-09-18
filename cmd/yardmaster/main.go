// Command yardmaster is a minimal high-performance LLM proxy for CLI agents.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bacnh85/yardmaster/internal/auth"
	"github.com/bacnh85/yardmaster/internal/config"
	"github.com/bacnh85/yardmaster/internal/provider"
	"github.com/bacnh85/yardmaster/internal/proxy"
	"github.com/bacnh85/yardmaster/internal/server"
	"github.com/bacnh85/yardmaster/internal/store"
)

// overridable at link time: -ldflags "-X main.version=…" (CI/Dockerfile stamp it)
var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "key":
		cmdKey(os.Args[2:])
	case "bench":
		cmdBench(os.Args[2:])
	case "oauth":
		cmdOAuth(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("yardmaster", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `yardmaster — self-hosted LLM proxy for CLI agents

usage:
  yardmaster run [-c config.yaml] [-listen :8787]   start the proxy + dashboard
  yardmaster key add NAME                           generate an API key (prints YAML)
  yardmaster oauth import claude-code|codex|opencode [file]
                                                      import a CLI subscription login (prints YAML)
  yardmaster bench [-c config.yaml] [flags]         run the perf harness
  yardmaster version
`)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	listen := fs.String("listen", "", "override listen addr")
	cfgPath := fs.String("c", "", "config path")
	fs.Parse(args)
	if *cfgPath == "" {
		*cfgPath = os.Getenv("AGENT_ROUTER_CONFIG")
	}
	if *cfgPath == "" {
		*cfgPath = "config.yaml"
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	reg := provider.New(cfg)
	keys := auth.NewKeyStore(cfg.Keys)
	pool := auth.NewOAuthPool(st)
	defer pool.Close()
	pool.RegisterConfig(cfg)
	p := proxy.NewProxy(reg, st, cfg.CostFor)
	p.Pool = pool
	p.Version = version
	srv := server.New(p, keys, st, *cfgPath, cfg.AdminPassword, version)

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigCh {
			if sig == syscall.SIGHUP {
				srv.Reload()
				continue
			}
			fmt.Println("\nshutting down (in-flight streams drain)...")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			httpSrv.Shutdown(ctx)
		}
	}()

	fmt.Printf("yardmaster %s listening on %s (db: %s, providers: %d, keys: %d)\n",
		version, cfg.Listen, cfg.DBPath, len(cfg.Providers), len(cfg.Keys))
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(err)
	}
	// give in-flight stats a moment to flush
	time.Sleep(700 * time.Millisecond)
}

func cmdKey(args []string) {
	if len(args) < 2 || args[0] != "add" {
		fatal(fmt.Errorf("usage: yardmaster key add NAME"))
	}
	name := args[1]
	k := config.GenKey()
	out, _ := json.MarshalIndent([]map[string]any{
		{"key": k, "name": name, "allow": []string{"*"}},
	}, "", "  ")
	fmt.Printf("add to your config under `keys:`:\n\n  keys:\n    %s\n\n(key starts with ar- — give it to your CLI agent)\n",
		string(out))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "yardmaster:", err)
	os.Exit(1)
}
