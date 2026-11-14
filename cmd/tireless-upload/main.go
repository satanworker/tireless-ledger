package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type settings struct {
	rawURL, endpoint, region, host, piRoot, codexRoot, ompRoot, claudeRoot, grokRoot, grokbotRoot string
	chats                                                                                         chatDirs
}

// harnesses are the agent session sources; a chat source may not reuse their names.
var harnesses = []string{"pi", "codex", "omp", "claude", "grok", "grokbot"}

// chatDirs maps a chat source (platform tag, e.g. telegram, x) to its export directory.
// Each uploads as <host>/<source>/<rel>; an empty directory disables the source.
type chatDirs map[string]string

func (c chatDirs) String() string { return "" }

// Set takes <source>=<dir> for the repeatable --chat flag.
func (c chatDirs) Set(value string) error {
	source, dir, ok := strings.Cut(value, "=")
	if !ok {
		return fmt.Errorf("want <source>=<dir>, got %q", value)
	}
	return c.set(source, dir)
}

func (c chatDirs) set(source, dir string) error {
	if source == "" || source == "." || source == ".." || strings.ContainsAny(source, "/\\") {
		return fmt.Errorf("chat source %q must be a single path segment", source)
	}
	for _, harness := range harnesses {
		if source == harness {
			return fmt.Errorf("chat source %q is an agent harness name", source)
		}
	}
	c[source] = dir
	return nil
}

// alias is a flag.Value for --telegram/--x: shorthand for --chat <source>=<dir>.
type alias struct {
	chats  chatDirs
	source string
}

func (a alias) String() string       { return "" }
func (a alias) Set(dir string) error { return a.chats.set(a.source, dir) }

func main() {
	home, _ := os.UserHomeDir()
	hostname, _ := os.Hostname()
	// telegram and x are always chat sources, so their walks skip each other's folder even when unset.
	cfg := settings{chats: chatDirs{"telegram": env("TIRELESS_TELEGRAM_DIR", ""), "x": env("TIRELESS_X_DIR", "")}}
	flag.StringVar(&cfg.rawURL, "raw-url", env("TIRELESS_RAW_URL", env("PI_MEMORYD_RAW_URL", "")), "S3 raw prefix")
	flag.StringVar(&cfg.endpoint, "endpoint", env("TIRELESS_S3_ENDPOINT", env("PI_MEMORYD_S3_ENDPOINT", env("AWS_ENDPOINT_URL", ""))), "S3-compatible endpoint")
	flag.StringVar(&cfg.region, "region", env("AWS_REGION", env("AWS_DEFAULT_REGION", "auto")), "S3 region")
	flag.StringVar(&cfg.host, "host", env("TIRELESS_UPLOAD_HOST", hostname), "stable device name")
	flag.StringVar(&cfg.piRoot, "pi", env("TIRELESS_PI_SESSIONS", filepath.Join(home, ".pi", "agent", "sessions")), "Pi sessions directory")
	flag.StringVar(&cfg.codexRoot, "codex", env("TIRELESS_CODEX_SESSIONS", filepath.Join(home, ".codex", "sessions")), "Codex sessions directory")
	flag.StringVar(&cfg.ompRoot, "omp", env("TIRELESS_OMP_SESSIONS", filepath.Join(home, ".omp", "agent", "sessions")), "OMP sessions directory")
	flag.StringVar(&cfg.claudeRoot, "claude", env("TIRELESS_CLAUDE_SESSIONS", filepath.Join(home, ".claude", "projects")), "Claude Code projects directory")
	flag.StringVar(&cfg.grokRoot, "grok", env("TIRELESS_GROK_SESSIONS", filepath.Join(home, ".grok", "sessions")), "Grok sessions directory")
	flag.StringVar(&cfg.grokbotRoot, "grokbot", env("TIRELESS_GROKBOT_SESSIONS", ""), "Grok Bot chat export directory (disabled when empty)")
	flag.Var(cfg.chats, "chat", "chat export `source=dir`, uploaded to <host>/<source>/ (repeatable)")
	flag.Var(alias{cfg.chats, "telegram"}, "telegram", "Telegram export `dir`, same as --chat telegram=<dir> (env TIRELESS_TELEGRAM_DIR; disabled when empty)")
	flag.Var(alias{cfg.chats, "x"}, "x", "X DM export `dir`, same as --chat x=<dir> (env TIRELESS_X_DIR; disabled when empty)")
	flag.Parse()
	if err := run(context.Background(), cfg); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cfg settings) error {
	if cfg.rawURL == "" {
		return fmt.Errorf("--raw-url or TIRELESS_RAW_URL is required")
	}
	if cfg.host == "" || strings.ContainsAny(cfg.host, "/\\") {
		return fmt.Errorf("invalid host %q", cfg.host)
	}
	bucket, prefix, err := parseS3URL(cfg.rawURL)
	if err != nil {
		return err
	}
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.region))
	if err != nil {
		return err
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.endpoint)
			o.UsePathStyle = true
		}
	})
	scanned, uploaded, err := syncAll(ctx, client, bucket, prefix, cfg)
	if err != nil {
		return err
	}
	fmt.Printf("scanned=%d uploaded=%d skipped=%d\n", scanned, uploaded, scanned-uploaded)
	return nil
}

func syncAll(ctx context.Context, client *s3.Client, bucket, prefix string, cfg settings) (totalScanned, totalUploaded int, err error) {
	type source struct {
		harness, root string
		skip          map[string]bool
	}
	sources := []source{{"pi", cfg.piRoot, nil}, {"codex", cfg.codexRoot, nil}, {"omp", cfg.ompRoot, nil}, {"claude", cfg.claudeRoot, nil}, {"grok", cfg.grokRoot, nil}, {"grokbot", cfg.grokbotRoot, nil}}
	// A chat inbox may hold another platform's export (e.g. <inbox>/x/): skip top-level folders named after a chat source.
	chatNames := map[string]bool{}
	names := make([]string, 0, len(cfg.chats))
	for name := range cfg.chats {
		chatNames[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sources = append(sources, source{name, cfg.chats[name], chatNames})
	}
	for _, source := range sources {
		scanned, uploaded, err := syncTree(ctx, client, bucket, prefix, cfg.host, source.harness, source.root, source.skip)
		if err != nil {
			return totalScanned, totalUploaded, err
		}
		totalScanned += scanned
		totalUploaded += uploaded
	}
	return totalScanned, totalUploaded, nil
}

// syncTree uploads every *.jsonl under root to <prefix>/<host>/<harness>/<rel>, skipping files whose
// remote size already matches and top-level directories named in skipTop.
func syncTree(ctx context.Context, client *s3.Client, bucket, prefix, host, harness, root string, skipTop map[string]bool) (scanned, uploaded int, err error) {
	info, statErr := os.Stat(root)
	if root == "" || os.IsNotExist(statErr) {
		return 0, 0, nil
	}
	if statErr != nil {
		return 0, 0, statErr
	}
	if !info.IsDir() {
		return 0, 0, fmt.Errorf("%s is not a directory", root)
	}
	destination := path.Join(prefix, host, harness) + "/"
	remote, err := listSizes(ctx, client, bucket, destination)
	if err != nil {
		return 0, 0, fmt.Errorf("list %s: %w", harness, err)
	}
	err = filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && skipTop[entry.Name()] && filepath.Dir(filePath) == filepath.Clean(root) {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
			return nil
		}
		// Grok keeps several JSONL logs per session; updates.jsonl holds the clean message stream.
		if harness == "grok" && entry.Name() != "updates.jsonl" {
			return nil
		}
		scanned++
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		key := destination + filepath.ToSlash(rel)
		stat, err := entry.Info()
		if err != nil {
			return err
		}
		if remote[key] == stat.Size() {
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		// Live sessions keep appending: send exactly the bytes stat saw, or the PUT fails on a
		// body longer than ContentLength. A cut last line is skipped by the parser and re-sent next run.
		_, putErr := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &bucket, Key: &key, Body: io.NewSectionReader(file, 0, stat.Size()), ContentLength: aws.Int64(stat.Size()), ContentType: aws.String("application/x-ndjson"),
		})
		closeErr := file.Close()
		if putErr != nil {
			return fmt.Errorf("upload %s: %w", filePath, putErr)
		}
		if closeErr != nil {
			return closeErr
		}
		uploaded++
		return nil
	})
	return scanned, uploaded, err
}

func listSizes(ctx context.Context, client *s3.Client, bucket, prefix string) (map[string]int64, error) {
	out := map[string]int64{}
	pager := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range page.Contents {
			out[aws.ToString(object.Key)] = aws.ToInt64(object.Size)
		}
	}
	return out, nil
}

func parseS3URL(raw string) (bucket, prefix string, err error) {
	if !strings.HasPrefix(raw, "s3://") {
		return "", "", fmt.Errorf("invalid S3 URL %q", raw)
	}
	rest := strings.TrimPrefix(raw, "s3://")
	parts := strings.SplitN(rest, "/", 2)
	if parts[0] == "" {
		return "", "", fmt.Errorf("invalid S3 URL %q", raw)
	}
	if len(parts) == 2 {
		return parts[0], strings.Trim(parts[1], "/"), nil
	}
	return parts[0], "", nil
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
