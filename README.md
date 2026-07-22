# pi-memoryd

`pi-memoryd` is a central memory ingestion and retrieval daemon for OpenData Vector. It exposes a small JSON API, enforces single-writer ingestion through an internal FIFO queue, and stores memory records in OpenData Vector backed by SlateDB on AWS S3.

## Architecture

```text
[ Client 1: Mac Indexer ] -- HTTP JSON -->
[ Client 2: Copilot/IDE ] -- HTTP JSON --> [ pi-memoryd ] --> [ OpenData Vector ] --> [ Amazon S3 Bucket ]
[ Client 3: iOS App ]     -- HTTP JSON -->
```

Clients compute embeddings locally. `pi-memoryd` never computes vectors.

## Storage

Production storage URL:

```text
s3://pi-memoryd-vector-midnight/vector-index
```

Current S3-compatible backend: Backblaze B2 bucket `pi-memoryd-vector-midnight`, endpoint `https://s3.eu-central-003.backblazeb2.com`, region `eu-central-003`.

Default AWS region fallback: `eu-west-1`.

`pi-memoryd` uses the AWS Go SDK default credential chain to validate S3 access at startup, then generates an OpenData Vector config using native `Aws` object-store settings.

Required IAM permissions, limited to `vector-index/`:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::YOUR_PRODUCTION_BUCKET_NAME",
      "Condition": { "StringLike": { "s3:prefix": ["vector-index/*", "vector-index"] } }
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject"],
      "Resource": "arn:aws:s3:::YOUR_PRODUCTION_BUCKET_NAME/vector-index/*"
    }
  ]
}
```

## Mac / native (primary)

On the Mac, run a **native optimized Go binary**. Do not use Docker for the Mac daemon.

```bash
make doctor          # toolchain check
make build           # CGO_ENABLED=0, -trimpath, -ldflags='-s -w' -> bin/pi-memoryd
make install         # ~/.local/bin/pi-memoryd + ~/.local/share/pi-memoryd
make secrets-edit    # put B2/S3 secrets in SOPS (never plaintext in git)
make run             # decrypt SOPS -> .env, exec native binary
```

Build flags used:

| Flag | Why |
|---|---|
| `CGO_ENABLED=0` | pure Go, no libc dependency, portable binary |
| `-trimpath` | reproducible build paths |
| `-ldflags='-s -w'` | strip symbol/DWARF tables (smaller binary) |

Release both Apple arches:

```bash
make release   # bin/pi-memoryd-darwin-arm64 + bin/pi-memoryd-darwin-amd64
```

Wrapper that loads SOPS env and execs the binary:

```bash
./scripts/run-native.sh
```

Optional launchd agent template: `config/com.earendil.pi-memoryd.plist` (point `ProgramArguments` at `scripts/run-native.sh`).

Install OpenData Vector separately and put `opendata-vector` on `PATH` if using `PI_MEMORYD_START_VECTOR=true`.

Manual run without the helper:

```bash
make secrets-decrypt
set -a && source .env && set +a
export PI_MEMORYD_STORAGE_URL=s3://${PI_MEMORYD_S3_BUCKET}/${PI_MEMORYD_S3_PREFIX}
export AWS_ENDPOINT_URL=${PI_MEMORYD_S3_ENDPOINT}

./bin/pi-memoryd \
  --listen :8090 \
  --dimensions 384 \
  --batch-threshold 500 \
  --flush-seconds 300 \
  --state "$HOME/.local/share/pi-memoryd/dedup_state.json" \
  --vector-config "$HOME/.local/share/pi-memoryd/vector.yaml"
```

To let `pi-memoryd` spawn OpenData Vector:

```bash
PI_MEMORYD_START_VECTOR=true make run
```

Default data dir for native installs: `~/.local/share/pi-memoryd/`.

## Docker (optional, non-Mac / server)

Docker is optional and intended for Linux servers or CI — **not** the Mac daemon path.

```bash
make up     # decrypt SOPS, render vector config, compose up
make down
```

Compose services:

- `pi-memoryd` on port `8090`
- `vector-writer` on port `8080`

Generated `config/vector.s3.yaml` and decrypted `.env` are gitignored.

## SOPS secrets

Encrypted S3 bucket settings live in:

```text
secrets/pi-memoryd.sops.env
```

Edit secrets:

```bash
make secrets-edit
```

Decrypt to `.env` without starting Docker:

```bash
make secrets-decrypt
```

Render OpenData Vector config from decrypted env:

```bash
make render-config
```

SOPS uses the SSH Age recipient configured in `.sops.yaml`. Decrypt uses your `SOPS_AGE_SSH_PRIVATE_KEY_CMD` flow, e.g. `~/.local/bin/sops-age-ssh-key-op`, which fetches the SSH private key from 1Password. Replace `.sops.yaml` with your production SSH Age/KMS recipient if needed.

Secret keys currently stored encrypted:

- `PI_MEMORYD_S3_BUCKET`
- `PI_MEMORYD_S3_PREFIX`
- `PI_MEMORYD_S3_ENDPOINT`
- `AWS_REGION`
- OpenData Vector dimension/cache/batch tuning values

## API

### `POST /v1/memory/ingest`

Input:

```json
{
  "records": [
    {
      "id": "repo_path_chunkindex",
      "vector": [0.1, 0.2],
      "forward_content": "raw source code or chat turn",
      "metadata": {
        "scope": "code_memory",
        "project_name": "backend-api",
        "file_path": "src/main.go",
        "file_hash": "sha256...",
        "timestamp": 1781980800
      }
    }
  ]
}
```

Behavior:

- Validates schema and vector dimensions.
- Deduplicates by `file_path` + `file_hash` against local registry.
- Enqueues accepted records into FIFO queue.
- One background writer batches sequential upserts to OpenData Vector.
- Flushes when batch reaches 500 rows or 300 seconds elapse.

### `POST /v1/memory/query`

Input:

```json
{
  "query_vector": [0.1, 0.2],
  "scope": "code_memory",
  "project_name": "backend-api",
  "limit": 5
}
```

Behavior:

- Sends metadata filters to OpenData Vector as pre-filters:
  - `scope == <scope>`
  - `project_name == <project_name>`
- Requests `forward_content` and metadata fields.
- Returns raw text content plus score and metadata.

## Environment

| Variable | Default | Purpose |
|---|---:|---|
| `PI_MEMORYD_LISTEN` | `:8090` | Daemon HTTP listen address |
| `PI_MEMORYD_VECTOR_URL` | `http://127.0.0.1:8080` | OpenData Vector HTTP endpoint |
| `PI_MEMORYD_STORAGE_URL` | `s3://YOUR_PRODUCTION_BUCKET_NAME/vector-index` | S3 storage URL |
| `PI_MEMORYD_S3_ENDPOINT` | empty | S3-compatible endpoint override, e.g. Backblaze B2 |
| `AWS_REGION` / `AWS_DEFAULT_REGION` | `eu-west-1` | AWS region |
| `PI_MEMORYD_DIMENSIONS` | `384` | Vector dimensions |
| `PI_MEMORYD_FLUSH_SECONDS` | `300` | Debounced flush window |
| `PI_MEMORYD_BATCH_THRESHOLD` | `500` | Batch flush threshold |
| `PI_MEMORYD_QUEUE_DEPTH` | `10000` | FIFO channel capacity |
| `PI_MEMORYD_STATE` | `./data/dedup_state.json` | Dedup registry |
| `PI_MEMORYD_VECTOR_CONFIG` | `./data/vector.yaml` | Generated OpenData Vector config |
| `PI_MEMORYD_START_VECTOR` | `false` | Spawn OpenData Vector process |
| `PI_MEMORYD_VECTOR_BINARY` | `opendata-vector` | Vector binary path |
| `PI_MEMORYD_DRY_RUN_S3` | `false` | Skip AWS SDK S3 startup check |

## OpenData Vector mapping

Incoming memory item is upserted to OpenData Vector as:

```json
{
  "upsertVectors": [
    {
      "id": "repo_path_chunkindex",
      "attributes": {
        "vector": [0.1, 0.2],
        "forward_content": "raw source code or chat turn",
        "scope": "code_memory",
        "project_name": "backend-api",
        "file_path": "src/main.go",
        "file_hash": "sha256...",
        "timestamp": 1781980800
      }
    }
  ]
}
```
