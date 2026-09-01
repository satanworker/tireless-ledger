package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type settings struct {
	rawURL, endpoint, region, host, piRoot, codexRoot, ompRoot string
}

func main() {
	home, _ := os.UserHomeDir()
	hostname, _ := os.Hostname()
	cfg := settings{}
	flag.StringVar(&cfg.rawURL, "raw-url", env("TIRELESS_RAW_URL", env("PI_MEMORYD_RAW_URL", "")), "S3 raw prefix")
	flag.StringVar(&cfg.endpoint, "endpoint", env("TIRELESS_S3_ENDPOINT", env("PI_MEMORYD_S3_ENDPOINT", env("AWS_ENDPOINT_URL", ""))), "S3-compatible endpoint")
	flag.StringVar(&cfg.region, "region", env("AWS_REGION", env("AWS_DEFAULT_REGION", "auto")), "S3 region")
	flag.StringVar(&cfg.host, "host", env("TIRELESS_UPLOAD_HOST", hostname), "stable device name")
	flag.StringVar(&cfg.piRoot, "pi", env("TIRELESS_PI_SESSIONS", filepath.Join(home, ".pi", "agent", "sessions")), "Pi sessions directory")
	flag.StringVar(&cfg.codexRoot, "codex", env("TIRELESS_CODEX_SESSIONS", filepath.Join(home, ".codex", "sessions")), "Codex sessions directory")
	flag.StringVar(&cfg.ompRoot, "omp", env("TIRELESS_OMP_SESSIONS", filepath.Join(home, ".omp", "agent", "sessions")), "OMP sessions directory")
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
	totalScanned, totalUploaded := 0, 0
	for _, source := range []struct{ harness, root string }{{"pi", cfg.piRoot}, {"codex", cfg.codexRoot}, {"omp", cfg.ompRoot}} {
		scanned, uploaded, err := syncTree(ctx, client, bucket, prefix, cfg.host, source.harness, source.root)
		if err != nil {
			return err
		}
		totalScanned += scanned
		totalUploaded += uploaded
	}
	fmt.Printf("scanned=%d uploaded=%d skipped=%d\n", totalScanned, totalUploaded, totalScanned-totalUploaded)
	return nil
}

func syncTree(ctx context.Context, client *s3.Client, bucket, prefix, host, harness, root string) (scanned, uploaded int, err error) {
	info, statErr := os.Stat(root)
	if os.IsNotExist(statErr) {
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
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
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
		_, putErr := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: &bucket, Key: &key, Body: file, ContentLength: aws.Int64(stat.Size()), ContentType: aws.String("application/x-ndjson"),
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
