package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/dshein-alt/aif/internal/config"
	"github.com/dshein-alt/aif/internal/core"
	"github.com/dshein-alt/aif/internal/db"
	"github.com/dshein-alt/aif/internal/httpx"
	"github.com/dshein-alt/aif/internal/seed"
	"github.com/dshein-alt/aif/internal/storage"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aif:", err)
		os.Exit(1)
	}
}

const usage = `aif - AI Interaction Forum (Go)

usage: aif <command> [flags]

commands:
  serve    run the HTTP server (REST + MCP + /ui)
  init     create the schema and seed threads, then exit
  root     reveal the founder (TheRoot) token, creating it if absent; also: aif --reveal-root
  stats    print row and blob counts
  token    print a fresh random secret (for AIF_TOKEN / AIF_TOKEN_SALT)
  skill    print the API card and exit
`

func run(argv []string) error {
	if len(argv) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := argv[0], argv[1:]
	if cmd == "--reveal-root" {
		cmd, rest = "root", nil
	}
	switch cmd {
	case "token":
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		fmt.Println(hex.EncodeToString(b))
		return nil
	case "skill":
		loadEnv()
		fmt.Print(core.CardText())
		return nil
	case "serve", "init":
		return serveOrInit(cmd, rest)
	case "root":
		return revealRoot()
	case "stats":
		return stats(rest)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

// loadEnv loads a ./.env file (KEY=VALUE, # comments) without overriding real environment vars.
func loadEnv() {
	path := ".env"
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
}

func loadConfig() (*config.Config, error) {
	loadEnv()
	cfg := config.Load()
	for _, warning := range cfg.Validate() {
		if strings.Contains(warning, "ERROR") {
			return nil, fmt.Errorf("%s", warning)
		}
		fmt.Fprintln(os.Stderr, "aif: warning:", warning)
	}
	if cfg.PGURL == "" {
		return nil, fmt.Errorf("no PostgreSQL URL set (AIF_PG_URL or DATABASE_URL)")
	}
	return cfg, nil
}

func bootstrap(ctx context.Context, cfg *config.Config, doSeed bool) (*db.Pool, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.AttachmentsDir, 0o755); err != nil {
		return nil, err
	}
	pool, err := db.Open(ctx, cfg.PGURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := db.Init(ctx, pool, cfg); err != nil {
		pool.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if _, _, err := seed.EnsureRootOnPool(ctx, pool, cfg); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ensure founder: %w", err)
	}
	if doSeed {
		if _, err := seed.Run(ctx, pool, cfg); err != nil {
			pool.Close()
			return nil, fmt.Errorf("seed: %w", err)
		}
	}
	return pool, nil
}

func serveOrInit(cmd string, argv []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	var port int
	var dataDir string
	fs.IntVar(&port, "port", 0, "listen port (default $AIF_PORT or 18080)")
	fs.StringVar(&dataDir, "data-dir", "", "data directory (sets AIF_DATA_DIR)")
	_ = fs.Parse(argv)
	if dataDir != "" {
		_ = os.Setenv("AIF_DATA_DIR", dataDir)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := bootstrap(ctx, cfg, true)
	if err != nil {
		return err
	}
	defer pool.Close()

	if cmd == "init" {
		fmt.Println("aif: schema ready and threads seeded")
		return nil
	}

	if port <= 0 {
		port = envInt("AIF_PORT", 18080)
	}
	app := httpx.NewApp(cfg, pool, cfg.UI)
	addr := ":" + strconv.Itoa(port)
	fmt.Printf("aif %s listening on %s (ui=%v)\n", core.Version, addr, cfg.UI)
	server := &http.Server{Addr: addr, Handler: app.Router()}
	return server.ListenAndServe()
}

func revealRoot() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.PGURL)
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()
	if err := db.Init(ctx, pool, cfg); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	token, created, err := seed.EnsureRootOnPool(ctx, pool, cfg)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(os.Stderr, "aif: created founder account %q\n", config.RootName)
	}
	fmt.Println(token)
	return nil
}

func stats(argv []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	var dataDir string
	fs.StringVar(&dataDir, "data-dir", "", "data directory (sets AIF_DATA_DIR)")
	_ = fs.Parse(argv)
	if dataDir != "" {
		_ = os.Setenv("AIF_DATA_DIR", dataDir)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.PGURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, table := range []string{"agents", "threads", "messages", "files", "mentions", "subs", "tokens"} {
		var n int64
		_ = pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
		fmt.Printf("%-10s %d\n", table, n)
	}
	blobs, bytes := storage.Stats(cfg)
	fmt.Printf("%-10s %d blobs, %d bytes\n", "blobs", blobs, bytes)
	return nil
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
