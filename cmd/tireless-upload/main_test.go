package main

import (
	"context"
	"encoding/xml"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestParseS3URL(t *testing.T) {
	bucket, prefix, err := parseS3URL("s3://bucket/raw/v1/")
	if err != nil || bucket != "bucket" || prefix != "raw/v1" {
		t.Fatalf("bucket=%q prefix=%q err=%v", bucket, prefix, err)
	}
	if _, _, err := parseS3URL("https://bucket/raw"); err == nil {
		t.Fatal("accepted non-S3 URL")
	}
}

// -grokbot and -telegram default to empty: the source must be skipped without touching S3.
func TestSyncTreeSkipsUnsetRoot(t *testing.T) {
	for _, harness := range []string{"grokbot", "telegram"} {
		scanned, uploaded, err := syncTree(context.Background(), nil, "bucket", "raw", "grok-box", harness, "", nil)
		if scanned != 0 || uploaded != 0 || err != nil {
			t.Fatalf("%s: scanned=%d uploaded=%d err=%v", harness, scanned, uploaded, err)
		}
	}
}

// fakeS3 keeps PUT object sizes and answers ListObjectsV2 from them; it records every PUT key.
type fakeS3 struct {
	mu    sync.Mutex
	sizes map[string]int64
	puts  []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.sizes[key] = int64(len(body))
		f.puts = append(f.puts, key)
	case http.MethodGet:
		type object struct {
			Key  string
			Size int64
		}
		result := struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []object
		}{}
		for k, size := range f.sizes {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				result.Contents = append(result.Contents, object{k, size})
			}
		}
		_ = xml.NewEncoder(w).Encode(result)
	default:
		http.Error(w, "unexpected "+r.Method, http.StatusBadRequest)
	}
}

func newFakeS3(t *testing.T) (*fakeS3, *s3.Client) {
	fake := &fakeS3{sizes: map[string]int64{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{Region: "auto", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true, Credentials: aws.AnonymousCredentials{}})
	return fake, client
}

func writeFiles(t *testing.T, root string, files ...string) {
	for _, rel := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"id":1}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// chatOnly disables every harness so only chat sources are walked.
func chatOnly(t *testing.T, args ...string) settings {
	cfg := settings{host: "mac", chats: chatDirs{"telegram": "", "x": ""}}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Var(cfg.chats, "chat", "")
	fs.Var(alias{cfg.chats, "telegram"}, "telegram", "")
	fs.Var(alias{cfg.chats, "x"}, "x", "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func sortedPuts(f *fakeS3) []string {
	puts := append([]string(nil), f.puts...)
	sort.Strings(puts)
	return puts
}

func TestChatSourcesUploadUnderOwnPrefix(t *testing.T) {
	inbox := t.TempDir()
	writeFiles(t, inbox, "123/2026-11.jsonl", "-100456/2026-10.jsonl", "x/conv1/2026-11.jsonl", "x/conv2/2026-10.jsonl")
	fake, client := newFakeS3(t)

	_, uploaded, err := syncAll(context.Background(), client, "bucket", "raw", chatOnly(t, "--telegram", inbox, "--x", filepath.Join(inbox, "x")))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"raw/mac/telegram/-100456/2026-10.jsonl", "raw/mac/telegram/123/2026-11.jsonl", "raw/mac/x/conv1/2026-11.jsonl", "raw/mac/x/conv2/2026-10.jsonl"}
	if got := sortedPuts(fake); uploaded != 4 || !reflect.DeepEqual(got, want) {
		t.Fatalf("uploaded=%d keys=%v, want %v", uploaded, got, want)
	}

	// Idempotent: a second run against the stored sizes uploads nothing.
	fake.puts = nil
	scanned, uploaded, err := syncAll(context.Background(), client, "bucket", "raw", chatOnly(t, "--telegram", inbox, "--chat", "x="+filepath.Join(inbox, "x")))
	if err != nil || scanned != 4 || uploaded != 0 || len(fake.puts) != 0 {
		t.Fatalf("rerun: scanned=%d uploaded=%d puts=%v err=%v", scanned, uploaded, fake.puts, err)
	}
}

// Telegram never uploads <inbox>/x/, even when --x is not set.
func TestTelegramSkipsXFolderWithoutXSource(t *testing.T) {
	inbox := t.TempDir()
	writeFiles(t, inbox, "123/2026-11.jsonl", "x/conv1/2026-11.jsonl", "123/x/2026-11.jsonl")
	fake, client := newFakeS3(t)
	if _, _, err := syncAll(context.Background(), client, "bucket", "raw", chatOnly(t, "--telegram", inbox)); err != nil {
		t.Fatal(err)
	}
	// Only the top level is reserved: a nested x/ inside a chat folder is still uploaded.
	want := []string{"raw/mac/telegram/123/2026-11.jsonl", "raw/mac/telegram/123/x/2026-11.jsonl"}
	if got := sortedPuts(fake); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys=%v, want %v", got, want)
	}
}

// No x/ folder: --x on a missing directory is not an error and Telegram uploads as before.
func TestMissingXFolderIsNoop(t *testing.T) {
	inbox := t.TempDir()
	writeFiles(t, inbox, "123/2026-11.jsonl")
	fake, client := newFakeS3(t)
	scanned, uploaded, err := syncAll(context.Background(), client, "bucket", "raw", chatOnly(t, "--telegram", inbox, "--x", filepath.Join(inbox, "x")))
	want := []string{"raw/mac/telegram/123/2026-11.jsonl"}
	if got := sortedPuts(fake); err != nil || scanned != 1 || uploaded != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("scanned=%d uploaded=%d keys=%v err=%v", scanned, uploaded, got, err)
	}
}

func TestChatFlag(t *testing.T) {
	cfg := chatOnly(t, "--chat", "x=/a", "--telegram", "/t", "--chat", "signal=/s")
	if want := (chatDirs{"telegram": "/t", "x": "/a", "signal": "/s"}); !reflect.DeepEqual(cfg.chats, want) {
		t.Fatalf("chats=%v, want %v", cfg.chats, want)
	}
	if cfg := chatOnly(t, "--x", "/b"); cfg.chats["x"] != "/b" {
		t.Fatalf("--x alias: chats=%v", cfg.chats)
	}
	for _, bad := range []string{"x", "=/d", "a/b=/d", "..=/d", "claude=/d", "pi=/d"} {
		if err := (chatDirs{}).Set(bad); err == nil {
			t.Errorf("accepted --chat %q", bad)
		}
	}
}
