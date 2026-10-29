package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
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

func die(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

// emit prints the agent digest, or {key: items} with -json.
func emit(asJSON bool, key string, items []map[string]any, text func()) {
	if !asJSON {
		text()
		return
	}
	if items == nil {
		items = []map[string]any{}
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{key: items})
}

// fanOutOrDie queries every target store and exits only when all of them failed.
func fanOutOrDie(target []store, key string, call func(*index.Client) (json.RawMessage, error)) []storeResult {
	rs := fanOut(target, key, call)
	if reportErrors(rs) {
		os.Exit(1)
	}
	return rs
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
		fmt.Fprintf(os.Stderr, "usage: tireless [-server URL -embed URL] [-host] <init|stores|query|container|sessions|sync|watch>\n")
		os.Exit(2)
	}
	cmd := args[0]
	if cmd == "init" {
		if err := initCmd(args[1:]); err != nil {
			die(err)
		}
		return
	}
	// Stores come from the stores file unless -server/-embed name one explicitly;
	// without the file, the single store is TIRELESS_SERVER/TIRELESS_EMBED (default localhost).
	stores, err := loadStores(storesPath())
	if err != nil {
		die(err)
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "server" || f.Name == "embed" })
	if explicit || len(stores) == 0 {
		stores = []store{{server: *server, embed: *embed}}
	}
	switch cmd {
	case "stores":
		for _, s := range stores {
			fmt.Println(join(or(s.name, "(default)"), s.server, s.embed))
		}
	case "sync", "watch":
		sf := flag.NewFlagSet(cmd, flag.ExitOnError)
		storeName := sf.String("store", "", "store to write to (required when several are configured)")
		poll := sf.Duration("poll", 3*time.Second, "watch interval")
		dry := sf.Bool("dry-run", false, "parse only")
		limit := sf.Int("limit", 0, "max turns this run (0=all)")
		_ = sf.Parse(args[1:])
		target, err := pick(stores, *storeName)
		if err != nil {
			die(err)
		}
		if len(target) != 1 {
			die(fmt.Errorf("%s writes to one store: pass -store NAME", cmd))
		}
		cur, err := index.LoadCursor(*state)
		if err != nil {
			die(err)
		}
		cli := index.NewClient(target[0].server, target[0].embed, *host)
		cli.DryRun = *dry
		opts := index.SyncOpts{PiRoot: *pi, CodexRoot: *codex, OmpRoot: *omp, Host: *host, Cursor: cur, Client: cli, Poll: *poll, Limit: *limit}
		if cmd == "watch" {
			if err := opts.Watch(); err != nil {
				die(err)
			}
			return
		}
		a, s, n, err := opts.Once()
		if err != nil {
			die(err)
		}
		fmt.Printf("files=%d accepted=%d skipped=%d\n", n, a, s)
	case "query":
		sf := flag.NewFlagSet("query", flag.ExitOnError)
		storeName := sf.String("store", "", "search only this store (default: all)")
		emb := sf.Bool("embed", true, "hybrid ANN+BM25 (use -embed=false for BM25 only)")
		container := sf.String("container", "", "container_id filter (agent session id or Telegram chat id)")
		project := sf.String("project", "", "project filter")
		source := sf.String("source", "", "source filter (pi, codex, omp, telegram, ...)")
		kind := sf.String("kind", "", "kind filter (message, title, todo, task)")
		limit := sf.Int("limit", 8, "hits to return")
		asJSON := sf.Bool("json", false, "raw JSON instead of the digest")
		_ = sf.Parse(args[1:])
		target, err := pick(stores, *storeName)
		if err != nil {
			die(err)
		}
		q := strings.Join(sf.Args(), " ")
		filters := map[string]string{"container_id": *container, "project": *project, "source": *source, "kind": *kind}
		start := time.Now()
		rs := fanOutOrDie(target, "results", func(c *index.Client) (json.RawMessage, error) {
			return c.Query(q, *emb, filters, *limit)
		})
		hits := interleave(rs, *limit)
		emit(*asJSON, "results", hits, func() { printHits(os.Stdout, hits, time.Since(start)) })
	case "container":
		sf := flag.NewFlagSet("container", flag.ExitOnError)
		storeName := sf.String("store", "", "store the hit came from (default: all)")
		limit := sf.Int("limit", 50, "")
		afterTS := sf.Int64("after-ts", 0, "cursor timestamp")
		afterID := sf.String("after-id", "", "cursor id")
		cHost := sf.String("host", "", "ingesting host")
		cSource := sf.String("source", "", "source (pi, codex, omp, telegram, ...)")
		include := sf.String("include", "", "todos|tasks|titles|state")
		asJSON := sf.Bool("json", false, "raw JSON instead of the digest")
		_ = sf.Parse(args[1:])
		if sf.NArg() < 1 {
			fmt.Fprintln(os.Stderr, "usage: tireless container [flags] <container_id>")
			os.Exit(2)
		}
		target, err := pick(stores, *storeName)
		if err != nil {
			die(err)
		}
		ts := ""
		if *afterTS > 0 {
			ts = strconv.FormatInt(*afterTS, 10)
		}
		start := time.Now()
		rs := fanOutOrDie(target, "results", func(c *index.Client) (json.RawMessage, error) {
			// Container sets "limit" on the map, so each store gets its own.
			return c.Container(map[string]string{"container_id": sf.Arg(0), "host": *cHost, "source": *cSource, "after_ts": ts, "after_id": *afterID, "include": *include}, *limit)
		})
		hits := concat(rs)
		emit(*asJSON, "results", hits, func() { printHits(os.Stdout, hits, time.Since(start)) })
	case "sessions":
		sf := flag.NewFlagSet("sessions", flag.ExitOnError)
		storeName := sf.String("store", "", "list only this store (default: all)")
		since := sf.String("since", "24h", "window start: unix seconds, RFC3339, or a duration ago (24h)")
		until := sf.String("until", "", "window end (exclusive), same forms; empty = now")
		sHost := sf.String("host", "", "ingesting host")
		sSource := sf.String("source", "", "source (pi, codex, omp, telegram, ...)")
		project := sf.String("project", "", "project filter")
		asJSON := sf.Bool("json", false, "raw JSON instead of the digest")
		_ = sf.Parse(args[1:])
		target, err := pick(stores, *storeName)
		if err != nil {
			die(err)
		}
		params := map[string]string{"host": *sHost, "source": *sSource, "project": *project}
		for k, v := range map[string]string{"since": *since, "until": *until} {
			if v == "" {
				continue
			}
			ts, err := parseTime(v, time.Now())
			if err != nil {
				fmt.Fprintf(os.Stderr, "-%s: %v\n", k, err)
				os.Exit(2)
			}
			params[k] = strconv.FormatInt(ts, 10)
		}
		rs := fanOutOrDie(target, "sessions", func(c *index.Client) (json.RawMessage, error) { return c.Sessions(params) })
		sessions := concat(rs)
		emit(*asJSON, "sessions", sessions, func() { printSessions(os.Stdout, sessions) })
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}

// parseTime reads unix seconds, RFC3339, or a duration before now (24h, 90m).
func parseTime(s string, now time.Time) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d).Unix(), nil
	}
	return 0, fmt.Errorf("%q is not unix seconds, RFC3339, or a duration", s)
}
