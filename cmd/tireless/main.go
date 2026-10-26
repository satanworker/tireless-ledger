package main

import (
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/earendil-works/pi-memoryd/cmd/tireless/index"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return "."
}

func main() {
	fs := flag.NewFlagSet("tireless", flag.ExitOnError)
	server := fs.String("server", env("TIRELESS_SERVER", "http://127.0.0.1:8090"), "pi-memoryd")
	embed := fs.String("embed", env("TIRELESS_EMBED", "http://127.0.0.1:8091"), "embedding server")
	host := fs.String("host", env("TIRELESS_HOST", ""), "machine tag")
	pi := fs.String("pi", env("TIRELESS_PI_SESSIONS", filepath.Join(home(), ".pi/agent/sessions")), "pi sessions dir")
	codex := fs.String("codex", env("TIRELESS_CODEX_SESSIONS", filepath.Join(home(), ".codex/sessions")), "codex sessions dir")
	omp := fs.String("omp", env("TIRELESS_OMP_SESSIONS", filepath.Join(home(), ".omp/agent/sessions")), "omp sessions dir")
	state := fs.String("state", env("TIRELESS_STATE", filepath.Join(home(), ".local/share/tireless-ledger/cursor.json")), "byte-offset cursor")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if *host == "" {
		h, _ := os.Hostname()
		*host = h
	}
	args := fs.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "usage: tireless [-server] [-embed] [-host] <sync|watch|query|container>\n")
		os.Exit(2)
	}
	cur, err := index.LoadCursor(*state)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cli := index.NewClient(*server, *embed, *host)
	cmd := args[0]
	switch cmd {
	case "sync", "watch":
		sf := flag.NewFlagSet(cmd, flag.ExitOnError)
		poll := sf.Duration("poll", 3*time.Second, "watch interval")
		dry := sf.Bool("dry-run", false, "parse only")
		limit := sf.Int("limit", 0, "max turns this run (0=all)")
		_ = sf.Parse(args[1:])
		cli.DryRun = *dry
		opts := index.SyncOpts{PiRoot: *pi, CodexRoot: *codex, OmpRoot: *omp, Host: *host, Cursor: cur, Client: cli, Poll: *poll, Limit: *limit}
		if cmd == "watch" {
			if err := opts.Watch(); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return
		}
		a, s, n, err := opts.Once()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("files=%d accepted=%d skipped=%d\n", n, a, s)
	case "query":
		sf := flag.NewFlagSet("query", flag.ExitOnError)
		emb := sf.Bool("embed", true, "hybrid ANN+BM25 (use -embed=false for BM25 only)")
		container := sf.String("container", "", "container_id filter (agent session id or Telegram chat id)")
		project := sf.String("project", "", "project filter")
		source := sf.String("source", "", "source filter (pi, codex, omp, telegram, ...)")
		kind := sf.String("kind", "", "kind filter (message, title, todo, task)")
		limit := sf.Int("limit", 8, "")
		_ = sf.Parse(args[1:])
		q := ""
		if rest := sf.Args(); len(rest) > 0 {
			q = rest[0]
		}
		b, err := cli.Query(q, *emb, map[string]string{"container_id": *container, "project": *project, "source": *source, "kind": *kind}, *limit)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Stdout.Write(b)
		fmt.Println()
	case "container":
		sf := flag.NewFlagSet("container", flag.ExitOnError)
		limit := sf.Int("limit", 50, "")
		afterTS := sf.Int64("after-ts", 0, "cursor timestamp")
		afterID := sf.String("after-id", "", "cursor id")
		cHost := sf.String("host", "", "ingesting host")
		cSource := sf.String("source", "", "source (pi, codex, omp, telegram, ...)")
		include := sf.String("include", "", "todos|tasks|titles|state")
		_ = sf.Parse(args[1:])
		if sf.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: tireless container [flags] <container_id>")
			os.Exit(2)
		}
		ts := ""
		if *afterTS > 0 {
			ts = strconv.FormatInt(*afterTS, 10)
		}
		b, err := cli.Container(map[string]string{"container_id": sf.Arg(0), "host": *cHost, "source": *cSource, "after_ts": ts, "after_id": *afterID, "include": *include}, *limit)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Stdout.Write(b)
		fmt.Println()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}
