// SPDX-License-Identifier: Apache-2.0
//
// Bounded, persistent read-through cache for immutable Lance object ranges.

use async_trait::async_trait;
use bytes::{Bytes, BytesMut};
use chrono::{DateTime, Utc};
use futures::{
    future::{BoxFuture, Shared},
    stream::BoxStream,
    FutureExt, StreamExt, TryStreamExt,
};
use lance::dataset::ReadParams;
use lance::io::{ObjectStoreParams, WrappingObjectStore};
use object_store::{
    path::Path, Attributes, CopyOptions, GetOptions, GetRange, GetResult, GetResultPayload,
    ListResult, MultipartUpload, ObjectMeta, ObjectStore, PutMultipartOptions, PutOptions,
    PutPayload, PutResult, RenameOptions, Result as ObjectStoreResult,
};
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet};
use std::fmt::{Display, Formatter};
use std::fs;
use std::io::Write;
use std::ops::Range;
use std::path::{Path as FsPath, PathBuf};
use std::sync::{
    atomic::{AtomicU64, Ordering},
    Arc, Mutex, OnceLock,
};
use std::time::UNIX_EPOCH;

const CACHE_DIR_ENV: &str = "LANCE_DISK_CACHE_DIR";
const CACHE_BYTES_ENV: &str = "LANCE_DISK_CACHE_MAX_BYTES";
const DEFAULT_CACHE_BYTES: u64 = 2 * 1024 * 1024 * 1024;
/// Single-request reads (`get_opts`) are cached as aligned blocks, so any later
/// read of an already-fetched region is served locally, not just identical ranges.
#[cfg(not(test))]
const BLOCK_BYTES: u64 = 256 * 1024;
#[cfg(test)]
const BLOCK_BYTES: u64 = 4;
/// Whole-object GETs larger than this stream from R2 uncached.
const WHOLE_OBJECT_MAX_BYTES: u64 = 8 * 1024 * 1024;
/// Background warm-up copies each table's immutable files in chunks of this size.
const WARM_CHUNK_BYTES: u64 = 64 * BLOCK_BYTES;
const WARM_CONCURRENCY: usize = 8;
/// A file's first read is Lance fetching its footer. Index files up to this size are
/// then fetched whole in that one request, data files get their last
/// `DATA_TAIL_BYTES` (footer, column and page metadata), so the dependent reads that
/// follow are cache hits instead of round trips. 0 disables it.
const PROMOTE_BYTES_ENV: &str = "LANCE_DISK_CACHE_PROMOTE_BYTES";
const DEFAULT_PROMOTE_BYTES: u64 = 64 * 1024 * 1024;
const DATA_TAIL_BYTES: u64 = 8 * 1024 * 1024;
const FOOTER_READ_BYTES: u64 = 64 * 1024;
/// `LANCE_DISK_CACHE_WARM=0` turns the background table copy off.
const WARM_ENV: &str = "LANCE_DISK_CACHE_WARM";

/// One remote fetch shared by every concurrent reader of its blocks: file size (meta),
/// fetched range, bytes. `None` when the fetch failed; readers then fetch themselves.
type SpanFuture = Shared<BoxFuture<'static, Option<(ObjectMeta, Range<u64>, Bytes)>>>;

#[derive(Default)]
struct Inflight(Mutex<HashMap<(String, u64), SpanFuture>>);

impl std::fmt::Debug for Inflight {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        f.write_str("Inflight")
    }
}

#[derive(Debug, Clone)]
struct CacheEntry {
    object_hash: String,
    size: u64,
    access: u64,
}

#[derive(Debug, Default)]
struct CacheState {
    total_bytes: u64,
    clock: u64,
    entries: HashMap<PathBuf, CacheEntry>,
}

#[derive(Debug, Default)]
struct CacheMetrics {
    hits: AtomicU64,
    misses: AtomicU64,
    hit_bytes: AtomicU64,
    fetched_bytes: AtomicU64,
}

#[derive(Debug, Clone)]
struct DiskRangeCache {
    root: Arc<PathBuf>,
    max_bytes: u64,
    promote_bytes: u64,
    state: Arc<Mutex<CacheState>>,
    metrics: Arc<CacheMetrics>,
    temp_id: Arc<AtomicU64>,
    inflight: Arc<Inflight>,
}

impl DiskRangeCache {
    fn new(root: PathBuf, max_bytes: u64, promote_bytes: u64) -> std::io::Result<Self> {
        fs::create_dir_all(&root)?;
        let mut state = CacheState::default();
        load_existing_entries(&root, &mut state)?;
        let cache = Self {
            root: Arc::new(root),
            max_bytes,
            promote_bytes,
            state: Arc::new(Mutex::new(state)),
            metrics: Arc::new(CacheMetrics::default()),
            temp_id: Arc::new(AtomicU64::new(0)),
            inflight: Arc::new(Inflight::default()),
        };
        cache.evict_to(max_bytes)?;
        Ok(cache)
    }

    fn initial_bytes(&self) -> u64 {
        self.state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .total_bytes
    }

    async fn get(&self, namespace: String, location: String, range: Range<u64>) -> Option<Bytes> {
        let cache = self.clone();
        let result = tokio::task::spawn_blocking(move || {
            cache.get_blocking(&namespace, &location, &range)
        })
        .await
        .ok()
        .flatten();
        match &result {
            Some(bytes) => {
                self.metrics.hits.fetch_add(1, Ordering::Relaxed);
                self.metrics
                    .hit_bytes
                    .fetch_add(bytes.len() as u64, Ordering::Relaxed);
            }
            None => {
                self.metrics.misses.fetch_add(1, Ordering::Relaxed);
            }
        }
        result
    }

    fn get_blocking(&self, namespace: &str, location: &str, range: &Range<u64>) -> Option<Bytes> {
        let object_hash = object_hash(namespace, location);
        let path = self.range_path(&object_hash, range);
        let expected_len = range.end.checked_sub(range.start)? as usize;
        self.read_entry(object_hash, path, Some(expected_len))
    }

    fn read_entry(&self, object_hash: String, path: PathBuf, expected_len: Option<usize>) -> Option<Bytes> {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        match fs::read(&path) {
            Ok(data) if expected_len.is_none_or(|len| data.len() == len) => {
                state.clock = state.clock.saturating_add(1);
                let access = state.clock;
                if let Some(entry) = state.entries.get_mut(&path) {
                    entry.access = access;
                } else {
                    let size = data.len() as u64;
                    state.total_bytes = state.total_bytes.saturating_add(size);
                    state.entries.insert(
                        path,
                        CacheEntry {
                            object_hash,
                            size,
                            access,
                        },
                    );
                }
                Some(Bytes::from(data))
            }
            Ok(_) => {
                remove_entry(&mut state, &path);
                let _ = fs::remove_file(path);
                None
            }
            Err(_) => None,
        }
    }

    async fn get_meta(&self, namespace: String, location: Path) -> Option<ObjectMeta> {
        let cache = self.clone();
        tokio::task::spawn_blocking(move || {
            let object_hash = object_hash(&namespace, location.as_ref());
            let path = cache.meta_path(&object_hash);
            let data = cache.read_entry(object_hash, path, None)?;
            decode_meta(location, &data)
        })
        .await
        .ok()
        .flatten()
    }

    async fn put_meta(&self, namespace: String, meta: ObjectMeta) {
        let cache = self.clone();
        if let Err(err) = tokio::task::spawn_blocking(move || {
            let object_hash = object_hash(&namespace, meta.location.as_ref());
            let path = cache.meta_path(&object_hash);
            cache.write_entry(object_hash, path, &encode_meta(&meta));
        })
        .await
        {
            log::warn!("Lance disk cache metadata write task failed: {err}");
        }
    }

    async fn put(&self, namespace: String, location: String, range: Range<u64>, bytes: Bytes) {
        self.metrics
            .fetched_bytes
            .fetch_add(bytes.len() as u64, Ordering::Relaxed);
        if bytes.is_empty() || bytes.len() as u64 > self.max_bytes {
            return;
        }
        let cache = self.clone();
        if let Err(err) = tokio::task::spawn_blocking(move || {
            cache.put_blocking(&namespace, &location, &range, &bytes)
        })
        .await
        {
            log::warn!("Lance disk cache write task failed: {err}");
        }
    }

    fn put_blocking(
        &self,
        namespace: &str,
        location: &str,
        range: &Range<u64>,
        bytes: &[u8],
    ) {
        let object_hash = object_hash(namespace, location);
        let path = self.range_path(&object_hash, range);
        self.write_entry(object_hash, path, bytes);
    }

    fn write_entry(&self, object_hash: String, path: PathBuf, bytes: &[u8]) {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());

        if let Ok(metadata) = fs::metadata(&path) {
            if metadata.len() == bytes.len() as u64 {
                state.clock = state.clock.saturating_add(1);
                let access = state.clock;
                if let Some(entry) = state.entries.get_mut(&path) {
                    entry.access = access;
                }
                return;
            }
            remove_entry(&mut state, &path);
            let _ = fs::remove_file(&path);
        }

        while state.total_bytes.saturating_add(bytes.len() as u64) > self.max_bytes {
            if !evict_oldest(&mut state) {
                break;
            }
        }
        if state.total_bytes.saturating_add(bytes.len() as u64) > self.max_bytes {
            return;
        }
        let Some(parent) = path.parent() else {
            return;
        };
        if let Err(err) = fs::create_dir_all(parent) {
            log::warn!("Lance disk cache could not create {}: {err}", parent.display());
            return;
        }

        let tmp_id = self.temp_id.fetch_add(1, Ordering::Relaxed);
        let tmp = path.with_extension(format!("tmp-{}-{tmp_id}", std::process::id()));
        let write_result = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&tmp)
            .and_then(|mut file| file.write_all(bytes));
        if let Err(err) = write_result {
            let _ = fs::remove_file(&tmp);
            log::warn!("Lance disk cache could not write {}: {err}", tmp.display());
            return;
        }
        if let Err(err) = fs::rename(&tmp, &path) {
            let _ = fs::remove_file(&tmp);
            if !path.exists() {
                log::warn!("Lance disk cache could not publish {}: {err}", path.display());
            }
            return;
        }
        state.clock = state.clock.saturating_add(1);
        let access = state.clock;
        let size = bytes.len() as u64;
        state.total_bytes = state.total_bytes.saturating_add(size);
        state.entries.insert(
            path,
            CacheEntry {
                object_hash,
                size,
                access,
            },
        );
    }

    fn invalidate(&self, namespace: &str, location: &str) {
        let hash = object_hash(namespace, location);
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        let paths: Vec<_> = state
            .entries
            .iter()
            .filter(|(_, entry)| entry.object_hash == hash)
            .map(|(path, _)| path.clone())
            .collect();
        for path in paths {
            remove_entry(&mut state, &path);
            let _ = fs::remove_file(path);
        }
    }

    fn evict_to(&self, target_bytes: u64) -> std::io::Result<()> {
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        while state.total_bytes > target_bytes {
            if !evict_oldest(&mut state) {
                break;
            }
        }
        Ok(())
    }

    fn range_path(&self, object_hash: &str, range: &Range<u64>) -> PathBuf {
        self.root.join(&object_hash[..2]).join(format!(
            "{object_hash}-{:016x}-{:016x}.cache",
            range.start, range.end
        ))
    }

    fn meta_path(&self, object_hash: &str) -> PathBuf {
        self.root
            .join(&object_hash[..2])
            .join(format!("{object_hash}-meta.cache"))
    }

    async fn has_all(&self, namespace: String, location: String, ranges: Vec<Range<u64>>) -> bool {
        let cache = self.clone();
        tokio::task::spawn_blocking(move || {
            let object_hash = object_hash(&namespace, &location);
            ranges.iter().all(|range| {
                fs::metadata(cache.range_path(&object_hash, range))
                    .is_ok_and(|metadata| metadata.len() == range.end - range.start)
            })
        })
        .await
        .unwrap_or(false)
    }

    #[cfg(test)]
    fn stats(&self) -> (u64, u64, u64, u64) {
        (
            self.metrics.hits.load(Ordering::Relaxed),
            self.metrics.misses.load(Ordering::Relaxed),
            self.metrics.hit_bytes.load(Ordering::Relaxed),
            self.metrics.fetched_bytes.load(Ordering::Relaxed),
        )
    }
}

fn load_existing_entries(root: &FsPath, state: &mut CacheState) -> std::io::Result<()> {
    for shard in fs::read_dir(root)? {
        let shard = shard?;
        if !shard.file_type()?.is_dir() {
            continue;
        }
        for item in fs::read_dir(shard.path())? {
            let item = item?;
            if !item.file_type()?.is_file() {
                continue;
            }
            let path = item.path();
            let Some(name) = path.file_name().and_then(|name| name.to_str()) else {
                continue;
            };
            if !name.ends_with(".cache") || name.len() < 64 {
                if name.contains(".tmp-") {
                    let _ = fs::remove_file(path);
                }
                continue;
            }
            let object_hash = name[..64].to_string();
            let metadata = item.metadata()?;
            let access = metadata
                .modified()
                .ok()
                .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
                .map(|duration| duration.as_nanos().min(u64::MAX as u128) as u64)
                .unwrap_or(0);
            state.clock = state.clock.max(access);
            state.total_bytes = state.total_bytes.saturating_add(metadata.len());
            state.entries.insert(
                path,
                CacheEntry {
                    object_hash,
                    size: metadata.len(),
                    access,
                },
            );
        }
    }
    Ok(())
}

fn evict_oldest(state: &mut CacheState) -> bool {
    let oldest = state
        .entries
        .iter()
        .min_by_key(|(_, entry)| entry.access)
        .map(|(path, _)| path.clone());
    let Some(path) = oldest else {
        return false;
    };
    remove_entry(state, &path);
    let _ = fs::remove_file(path);
    true
}

fn remove_entry(state: &mut CacheState, path: &FsPath) {
    if let Some(entry) = state.entries.remove(path) {
        state.total_bytes = state.total_bytes.saturating_sub(entry.size);
    }
}

fn object_hash(namespace: &str, location: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(namespace.as_bytes());
    hasher.update([0]);
    hasher.update(location.as_bytes());
    format!("{:x}", hasher.finalize())
}

// Size, last-modified nanos, then the e-tag; enough to answer a cached GET.
fn encode_meta(meta: &ObjectMeta) -> Vec<u8> {
    let e_tag = meta.e_tag.as_deref().unwrap_or("");
    let mut out = Vec::with_capacity(16 + e_tag.len());
    out.extend_from_slice(&meta.size.to_le_bytes());
    out.extend_from_slice(&meta.last_modified.timestamp_nanos_opt().unwrap_or(0).to_le_bytes());
    out.extend_from_slice(e_tag.as_bytes());
    out
}

fn decode_meta(location: Path, data: &[u8]) -> Option<ObjectMeta> {
    let size = u64::from_le_bytes(data.get(..8)?.try_into().ok()?);
    let nanos = i64::from_le_bytes(data.get(8..16)?.try_into().ok()?);
    let e_tag = std::str::from_utf8(&data[16..]).ok()?;
    Some(ObjectMeta {
        location,
        last_modified: DateTime::<Utc>::from_timestamp_nanos(nanos),
        size,
        e_tag: (!e_tag.is_empty()).then(|| e_tag.to_string()),
        version: None,
    })
}

fn bytes_result(meta: ObjectMeta, range: Range<u64>, attributes: Attributes, bytes: Bytes) -> GetResult {
    GetResult {
        payload: GetResultPayload::Stream(futures::stream::once(async move { Ok(bytes) }).boxed()),
        meta,
        range,
        attributes,
        extensions: Default::default(),
    }
}

fn cacheable(location: &Path) -> bool {
    location
        .as_ref()
        .split('/')
        .any(|part| matches!(part, "data" | "_indices" | "_deletions"))
}

/// `.../chunks.lance/data/x.lance` -> `.../chunks.lance`
fn table_root(location: &Path) -> Option<Path> {
    let parts: Vec<_> = location.parts().collect();
    let end = parts.iter().position(|part| part.as_ref().ends_with(".lance"))?;
    (end + 1 < parts.len()).then(|| Path::from_iter(parts[..=end].iter().cloned()))
}

#[derive(Debug)]
struct DiskCacheWrapper {
    cache: DiskRangeCache,
    warmed: Option<Arc<Mutex<HashSet<String>>>>,
}

impl WrappingObjectStore for DiskCacheWrapper {
    fn wrap(&self, namespace: &str, original: Arc<dyn ObjectStore>) -> Arc<dyn ObjectStore> {
        Arc::new(DiskCacheStore {
            original,
            cache: self.cache.clone(),
            namespace: namespace.to_string(),
            warmed: self.warmed.clone(),
        })
    }

    /// Listings are never cached (manifests and version lists must stay fresh), so the
    /// pushed-down pager can bypass the wrapper.
    fn wrap_paginated(
        &self,
        _store_prefix: &str,
        original: Arc<dyn object_store::list::PaginatedListStore>,
    ) -> Option<Arc<dyn object_store::list::PaginatedListStore>> {
        Some(original)
    }
}

#[derive(Debug, Clone)]
struct DiskCacheStore {
    original: Arc<dyn ObjectStore>,
    cache: DiskRangeCache,
    namespace: String,
    /// Table roots already queued for warm-up; `None` disables warm-up.
    warmed: Option<Arc<Mutex<HashSet<String>>>>,
}

impl Display for DiskCacheStore {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        write!(f, "DiskCacheStore({})", self.original)
    }
}

impl DiskCacheStore {
    /// The first read of a table starts copying its immutable files into the
    /// block cache in the background. Index files always come first: every
    /// search scans them. Data files are only read for result rows, so they are
    /// copied only while indexes plus data fit in half the cache; otherwise
    /// result rows are fetched on demand and kept by the LRU.
    fn maybe_warm(&self, location: &Path) {
        let (Some(warmed), Some(root)) = (&self.warmed, table_root(location)) else {
            return;
        };
        let key = format!("{}\0{root}", self.namespace);
        let first = warmed
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner())
            .insert(key);
        if first {
            let store = self.clone();
            tokio::spawn(async move { store.warm_root(root).await });
        }
    }

    async fn warm_root(&self, root: Path) {
        let started = std::time::Instant::now();
        let mut objects = Vec::new();
        let mut data = Vec::new();
        for dir in ["_indices", "_deletions", "data"] {
            let into = if dir == "data" { &mut data } else { &mut objects };
            match self.original.list(Some(&root.clone().join(dir))).try_collect::<Vec<_>>().await {
                Ok(found) => into.extend(found),
                Err(err) => log::warn!("Lance disk cache warm-up could not list {root}/{dir}: {err}"),
            }
        }
        let size = |metas: &[ObjectMeta]| metas.iter().map(|meta| meta.size).sum::<u64>();
        let with_data = size(&objects) + size(&data) <= self.cache.max_bytes / 2;
        if with_data {
            objects.extend(data);
        }
        let files = objects.len();
        let chunks: Vec<(ObjectMeta, Range<u64>)> = objects
            .into_iter()
            .flat_map(|meta| {
                (0..meta.size)
                    .step_by(WARM_CHUNK_BYTES as usize)
                    .map(move |start| (meta.clone(), start..(start + WARM_CHUNK_BYTES).min(meta.size)))
                    .collect::<Vec<_>>()
            })
            .collect();
        let fetched: u64 = futures::stream::iter(chunks)
            .map(|(meta, range)| self.warm_chunk(meta, range))
            .buffer_unordered(WARM_CONCURRENCY)
            .fold(0, |total, bytes| async move { total + bytes })
            .await;
        eprintln!(
            "lancedb-go disk cache warmed root={root} files={files} with_data={with_data} fetched_bytes={fetched} elapsed_ms={}",
            started.elapsed().as_millis()
        );
    }

    /// Returns the bytes fetched from the remote store (0 when already cached).
    async fn warm_chunk(&self, meta: ObjectMeta, range: Range<u64>) -> u64 {
        let key = meta.location.as_ref().to_string();
        let blocks = (range.start..range.end)
            .step_by(BLOCK_BYTES as usize)
            .map(|start| start..(start + BLOCK_BYTES).min(meta.size))
            .collect();
        if self.cache.has_all(self.namespace.clone(), key, blocks).await {
            return 0;
        }
        if self.cache.get_meta(self.namespace.clone(), meta.location.clone()).await.is_none() {
            self.cache.put_meta(self.namespace.clone(), meta.clone()).await;
        }
        let options = GetOptions::new().with_range(Some(range.clone()));
        match self.get_blocks(&meta.location, range.clone(), options, false).await {
            Ok(_) => range.end - range.start,
            Err(err) => {
                log::warn!("Lance disk cache warm-up could not read {}: {err}", meta.location);
                0
            }
        }
    }

    /// Lance's first read of a file is its 64 KiB footer, so `range.end` is the file
    /// size. Widen that fetch to the whole file for small index files and to the tail
    /// of data files. `size` is the cached file size, if the warm-up already recorded
    /// it; the footer blocks being uncached is what marks the first touch.
    fn widen(&self, location: &Path, size: Option<u64>, range: &Range<u64>, lo: u64, hi: u64) -> (u64, u64) {
        let promote = self.cache.promote_bytes;
        let footer = range.end - range.start == FOOTER_READ_BYTES && size.is_none_or(|size| size == range.end);
        if promote == 0 || !footer {
            return (lo, hi);
        }
        let path = location.as_ref();
        if path.contains("/_indices/") && range.end <= promote {
            return (0, hi);
        }
        if path.contains("/data/") {
            let start = range.end.saturating_sub(DATA_TAIL_BYTES) / BLOCK_BYTES;
            return (start.min(lo), hi);
        }
        (lo, hi)
    }

    /// Fetches blocks `lo..=hi` in one request and caches them.
    async fn fetch_span(&self, location: Path, lo: u64, hi: u64, options: GetOptions) -> ObjectStoreResult<(ObjectMeta, Range<u64>, Bytes)> {
        let fetch = GetOptions {
            range: Some(GetRange::Bounded(lo * BLOCK_BYTES..(hi + 1) * BLOCK_BYTES)),
            extensions: options.extensions,
            ..Default::default()
        };
        let result = self.original.get_opts(&location, fetch).await?;
        let (meta, fetched) = (result.meta.clone(), result.range.clone());
        if self.cache.get_meta(self.namespace.clone(), location.clone()).await.is_none() {
            self.cache.put_meta(self.namespace.clone(), meta.clone()).await;
        }
        let bytes = result.bytes().await?;
        let key = location.as_ref().to_string();
        for block in lo..=hi {
            let start = block * BLOCK_BYTES;
            if start >= fetched.end {
                break;
            }
            let end = (start + BLOCK_BYTES).min(fetched.end);
            let chunk = bytes.slice((start - fetched.start) as usize..(end - fetched.start) as usize);
            self.cache.put(self.namespace.clone(), key.clone(), start..end, chunk).await;
        }
        Ok((meta, fetched, bytes))
    }

    /// Serves `range` from aligned cached blocks, fetching the missing span of
    /// blocks in one request. Ranges past the end are clamped like S3 does.
    /// `share`: publish the fetch so concurrent readers of its blocks wait for it
    /// instead of fetching again. Warm-up chunks are not published: a query's 256 KiB
    /// row read must not queue behind a 16 MiB copy.
    async fn get_blocks(&self, location: &Path, range: Range<u64>, options: GetOptions, share: bool) -> ObjectStoreResult<GetResult> {
        let mut meta = self.cache.get_meta(self.namespace.clone(), location.clone()).await;
        if meta.as_ref().is_some_and(|meta| range.start >= meta.size) {
            return self.original.get_opts(location, options).await;
        }
        let first = range.start / BLOCK_BYTES;
        let last = (range.end - 1) / BLOCK_BYTES;
        let key = location.as_ref().to_string();
        let mut blocks: Vec<Option<Bytes>> = vec![None; (last - first + 1) as usize];
        if let Some(meta) = &meta {
            for (slot, block) in blocks.iter_mut().zip(first..=last) {
                let start = block * BLOCK_BYTES;
                if start >= meta.size {
                    break;
                }
                let end = (start + BLOCK_BYTES).min(meta.size);
                *slot = self.cache.get(self.namespace.clone(), key.clone(), start..end).await;
            }
        }
        let size = meta.as_ref().map_or(u64::MAX, |meta| meta.size);
        let needed = |block: u64| block * BLOCK_BYTES < size;
        let missing: Vec<u64> = (first..=last)
            .filter(|&block| needed(block) && blocks[(block - first) as usize].is_none())
            .collect();
        let fill = |blocks: &mut Vec<Option<Bytes>>, fetched: &Range<u64>, bytes: &Bytes| {
            for block in first..=last {
                let start = block * BLOCK_BYTES;
                if blocks[(block - first) as usize].is_some() || start < fetched.start || start >= fetched.end {
                    continue;
                }
                let end = (start + BLOCK_BYTES).min(fetched.end);
                blocks[(block - first) as usize] = Some(bytes.slice((start - fetched.start) as usize..(end - fetched.start) as usize));
            }
        };
        if let (Some(&lo), Some(&hi)) = (missing.first(), missing.last()) {
            let (lo, hi) = self.widen(location, meta.as_ref().map(|meta| meta.size), &range, lo, hi);
            let hash = object_hash(&self.namespace, &key);
            // Single flight: concurrent readers of the same blocks (the vector and text
            // channels open the same files at the same time) share one request.
            let (own, waits) = {
                let mut map = self.cache.inflight.0.lock().unwrap_or_else(|poisoned| poisoned.into_inner());
                let waits: Option<Vec<SpanFuture>> = missing.iter().map(|block| map.get(&(hash.clone(), *block)).cloned()).collect();
                match waits {
                    Some(waits) => (None, waits),
                    None => {
                        let store = self.clone();
                        let (loc, ext) = (location.clone(), GetOptions { extensions: options.extensions.clone(), ..Default::default() });
                        let fut: SpanFuture = async move {
                            match store.fetch_span(loc.clone(), lo, hi, ext).await {
                                Ok(span) => Some(span),
                                Err(err) => {
                                    log::warn!("Lance disk cache fetch of {loc} failed: {err}");
                                    None
                                }
                            }
                        }
                        .boxed()
                        .shared();
                        if share {
                            for block in lo..=hi {
                                map.entry((hash.clone(), block)).or_insert_with(|| fut.clone());
                            }
                        }
                        (Some(fut), Vec::new())
                    }
                }
            };
            if let Some(fut) = own {
                let result = fut.clone().await;
                {
                    let mut map = self.cache.inflight.0.lock().unwrap_or_else(|poisoned| poisoned.into_inner());
                    for block in lo..=hi {
                        if map.get(&(hash.clone(), block)).is_some_and(|entry| Shared::ptr_eq(entry, &fut)) {
                            map.remove(&(hash.clone(), block));
                        }
                    }
                }
                if let Some((fetched_meta, fetched, bytes)) = result {
                    meta.get_or_insert(fetched_meta);
                    fill(&mut blocks, &fetched, &bytes);
                }
            }
            for fut in waits {
                if let Some((fetched_meta, fetched, bytes)) = fut.await {
                    meta.get_or_insert(fetched_meta);
                    fill(&mut blocks, &fetched, &bytes);
                }
            }
            // Anything still missing (a shared fetch failed or did not cover it): fetch
            // directly so the real error surfaces.
            let size = meta.as_ref().map_or(u64::MAX, |meta| meta.size);
            let still: Vec<u64> = (first..=last)
                .filter(|&block| block * BLOCK_BYTES < size && blocks[(block - first) as usize].is_none())
                .collect();
            if let (Some(&lo), Some(&hi)) = (still.first(), still.last()) {
                let (fetched_meta, fetched, bytes) = self.fetch_span(location.clone(), lo, hi, options).await?;
                meta.get_or_insert(fetched_meta);
                fill(&mut blocks, &fetched, &bytes);
            }
        }

        let meta = meta.expect("metadata is cached or was just fetched");
        let end = range.end.min(meta.size);
        let mut parts = Vec::with_capacity(blocks.len());
        for (block, data) in (first..=last).zip(&blocks) {
            let start = block * BLOCK_BYTES;
            if start >= end {
                break;
            }
            let data = data.as_ref().ok_or_else(|| object_store::Error::Generic {
                store: "LanceDiskCache",
                source: std::io::Error::other(format!("block {block} of {location} was not populated")).into(),
            })?;
            let from = (range.start.max(start) - start) as usize;
            let to = (end.min(start + data.len() as u64) - start) as usize;
            parts.push(data.slice(from..to));
        }
        let bytes = if parts.len() == 1 {
            parts.pop().unwrap_or_default()
        } else {
            let mut out = BytesMut::with_capacity((end - range.start) as usize);
            parts.iter().for_each(|part| out.extend_from_slice(part));
            out.freeze()
        };
        Ok(bytes_result(meta, range.start..end, Attributes::default(), bytes))
    }
}

#[async_trait]
#[deny(clippy::missing_trait_methods)]
impl ObjectStore for DiskCacheStore {
    async fn put_opts(
        &self,
        location: &Path,
        payload: PutPayload,
        options: PutOptions,
    ) -> ObjectStoreResult<PutResult> {
        self.cache.invalidate(&self.namespace, location.as_ref());
        self.original.put_opts(location, payload, options).await
    }

    async fn put_multipart_opts(
        &self,
        location: &Path,
        options: PutMultipartOptions,
    ) -> ObjectStoreResult<Box<dyn MultipartUpload>> {
        self.cache.invalidate(&self.namespace, location.as_ref());
        self.original.put_multipart_opts(location, options).await
    }

    async fn get_opts(
        &self,
        location: &Path,
        options: GetOptions,
    ) -> ObjectStoreResult<GetResult> {
        let conditional = options.if_match.is_some()
            || options.if_none_match.is_some()
            || options.if_modified_since.is_some()
            || options.if_unmodified_since.is_some()
            || options.version.is_some()
            || options.head;
        if !cacheable(location) || conditional {
            return self.original.get_opts(location, options).await;
        }
        self.maybe_warm(location);
        match options.range.clone() {
            Some(GetRange::Bounded(range)) if range.start < range.end => {
                self.get_blocks(location, range, options, true).await
            }
            None => match self.cache.get_meta(self.namespace.clone(), location.clone()).await {
                Some(meta) if meta.size > 0 && meta.size <= WHOLE_OBJECT_MAX_BYTES => {
                    let mut result = self.get_blocks(location, 0..meta.size, options, true).await?;
                    result.meta = meta;
                    Ok(result)
                }
                Some(_) => self.original.get_opts(location, options).await,
                None => {
                    let result = self.original.get_opts(location, options).await?;
                    if result.meta.size > WHOLE_OBJECT_MAX_BYTES || result.range != (0..result.meta.size) {
                        return Ok(result);
                    }
                    let (meta, range, attributes) = (result.meta.clone(), result.range.clone(), result.attributes.clone());
                    let bytes = result.bytes().await?;
                    self.cache.put_meta(self.namespace.clone(), meta.clone()).await;
                    let key = location.as_ref().to_string();
                    for start in (0..meta.size).step_by(BLOCK_BYTES as usize) {
                        let end = (start + BLOCK_BYTES).min(meta.size);
                        let chunk = bytes.slice(start as usize..end as usize);
                        self.cache.put(self.namespace.clone(), key.clone(), start..end, chunk).await;
                    }
                    Ok(bytes_result(meta, range, attributes, bytes))
                }
            },
            _ => self.original.get_opts(location, options).await,
        }
    }

    async fn get_ranges(
        &self,
        location: &Path,
        ranges: &[Range<u64>],
    ) -> ObjectStoreResult<Vec<Bytes>> {
        if !cacheable(location) {
            return self.original.get_ranges(location, ranges).await;
        }
        self.maybe_warm(location);

        futures::future::try_join_all(ranges.iter().map(|range| async move {
            if range.start >= range.end {
                return Ok(Bytes::new());
            }
            let options = GetOptions::new().with_range(Some(range.clone()));
            self.get_blocks(location, range.clone(), options, true).await?.bytes().await
        }))
        .await
    }

    fn delete_stream(
        &self,
        locations: BoxStream<'static, ObjectStoreResult<Path>>,
    ) -> BoxStream<'static, ObjectStoreResult<Path>> {
        let cache = self.cache.clone();
        let namespace = self.namespace.clone();
        let locations = locations
            .map(move |result| {
                if let Ok(location) = &result {
                    cache.invalidate(&namespace, location.as_ref());
                }
                result
            })
            .boxed();
        self.original.delete_stream(locations)
    }

    fn list(&self, prefix: Option<&Path>) -> BoxStream<'static, ObjectStoreResult<ObjectMeta>> {
        self.original.list(prefix)
    }

    fn list_with_offset(
        &self,
        prefix: Option<&Path>,
        offset: &Path,
    ) -> BoxStream<'static, ObjectStoreResult<ObjectMeta>> {
        self.original.list_with_offset(prefix, offset)
    }

    async fn list_with_delimiter(
        &self,
        prefix: Option<&Path>,
    ) -> ObjectStoreResult<ListResult> {
        self.original.list_with_delimiter(prefix).await
    }

    async fn copy_opts(
        &self,
        from: &Path,
        to: &Path,
        options: CopyOptions,
    ) -> ObjectStoreResult<()> {
        self.cache.invalidate(&self.namespace, to.as_ref());
        self.original.copy_opts(from, to, options).await
    }

    async fn rename_opts(
        &self,
        from: &Path,
        to: &Path,
        options: RenameOptions,
    ) -> ObjectStoreResult<()> {
        self.cache.invalidate(&self.namespace, from.as_ref());
        self.cache.invalidate(&self.namespace, to.as_ref());
        self.original.rename_opts(from, to, options).await
    }
}

static CONFIGURED_WRAPPER: OnceLock<Option<Arc<DiskCacheWrapper>>> = OnceLock::new();

fn configured_wrapper() -> Option<Arc<DiskCacheWrapper>> {
    CONFIGURED_WRAPPER
        .get_or_init(|| {
            let root = std::env::var_os(CACHE_DIR_ENV).filter(|value| !value.is_empty())?;
            let max_bytes = match std::env::var(CACHE_BYTES_ENV) {
                Ok(value) => match value.parse::<u64>() {
                    Ok(0) => return None,
                    Ok(value) => value,
                    Err(err) => {
                        log::warn!("Ignoring invalid {CACHE_BYTES_ENV}={value:?}: {err}");
                        return None;
                    }
                },
                Err(_) => DEFAULT_CACHE_BYTES,
            };
            let promote_bytes = match std::env::var(PROMOTE_BYTES_ENV) {
                Ok(value) => value.parse::<u64>().unwrap_or(DEFAULT_PROMOTE_BYTES),
                Err(_) => DEFAULT_PROMOTE_BYTES,
            };
            let warm = std::env::var(WARM_ENV).map_or(true, |value| value != "0");
            match DiskRangeCache::new(PathBuf::from(root), max_bytes, promote_bytes) {
                Ok(cache) => {
                    eprintln!(
                        "lancedb-go disk range cache dir={} max_bytes={} existing_bytes={}",
                        cache.root.display(),
                        max_bytes,
                        cache.initial_bytes()
                    );
                    eprintln!("lancedb-go disk cache promote_bytes={promote_bytes} warm={warm}");
                    Some(Arc::new(DiskCacheWrapper { cache, warmed: warm.then(Default::default) }))
                }
                Err(err) => {
                    log::warn!("Lance disk cache disabled: {err}");
                    None
                }
            }
        })
        .clone()
}

pub(crate) fn table_read_params() -> Option<ReadParams> {
    configured_wrapper().map(|wrapper| ReadParams {
        store_options: Some(ObjectStoreParams {
            object_store_wrapper: Some(wrapper),
            ..Default::default()
        }),
        ..Default::default()
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use object_store::{memory::InMemory, ObjectStoreExt};
    use tempfile::tempdir;

    async fn test_store(max_bytes: u64) -> (Arc<InMemory>, DiskCacheStore, tempfile::TempDir) {
        let temp = tempdir().unwrap();
        let cache = DiskRangeCache::new(temp.path().to_path_buf(), max_bytes, 0).unwrap();
        let original = Arc::new(InMemory::new());
        let store = DiskCacheStore {
            original: original.clone(),
            cache,
            namespace: "s3$test".to_string(),
            warmed: None,
        };
        (original, store, temp)
    }

    #[tokio::test]
    async fn repeated_immutable_range_survives_remote_removal() {
        let (original, store, _temp) = test_store(1024).await;
        let path = Path::from("db/chunks.lance/data/vectors.lance");
        original.put(&path, Bytes::from_static(b"abcdefghijkl").into()).await.unwrap();

        let first = store.get_ranges(&path, &[2..8]).await.unwrap();
        assert_eq!(first, vec![Bytes::from_static(b"cdefgh")]);
        original.delete(&path).await.unwrap();
        let second = store.get_ranges(&path, &[2..8]).await.unwrap();
        assert_eq!(second, first);
        // The first read fetched blocks 0..4 and 4..8; the second hit both.
        assert_eq!(store.cache.stats(), (2, 0, 8, 8));
    }

    #[tokio::test]
    async fn mutable_manifest_is_never_cached() {
        let (original, store, _temp) = test_store(1024).await;
        let path = Path::from("db/chunks.lance/_latest.manifest");
        original.put(&path, Bytes::from_static(b"manifest").into()).await.unwrap();
        assert_eq!(
            store.get_ranges(&path, &[0..4]).await.unwrap(),
            vec![Bytes::from_static(b"mani")]
        );
        original.delete(&path).await.unwrap();
        assert!(store.get_ranges(&path, &[0..4]).await.is_err());
        assert_eq!(store.cache.stats(), (0, 0, 0, 0));
    }

    #[tokio::test]
    async fn capacity_evicts_least_recently_used_range() {
        let temp = tempdir().unwrap();
        let original = Arc::new(InMemory::new());
        let path = Path::from("db/chunks.lance/data/vectors.lance");
        original.put(&path, Bytes::from_static(b"abcdefghijkl").into()).await.unwrap();
        // Room for the object's metadata record plus two 4-byte blocks.
        let meta_len = encode_meta(&original.head(&path).await.unwrap()).len() as u64;
        let store = DiskCacheStore {
            original: original.clone(),
            cache: DiskRangeCache::new(temp.path().to_path_buf(), meta_len + 8, 0).unwrap(),
            namespace: "s3$test".to_string(),
            warmed: None,
        };

        store.get_ranges(&path, &[0..4]).await.unwrap();
        store.get_ranges(&path, &[4..8]).await.unwrap();
        store.get_ranges(&path, &[0..4]).await.unwrap();
        store.get_ranges(&path, &[8..12]).await.unwrap();
        original.delete(&path).await.unwrap();

        assert_eq!(
            store.get_ranges(&path, &[0..4]).await.unwrap(),
            vec![Bytes::from_static(b"abcd")]
        );
        assert!(store.get_ranges(&path, &[4..8]).await.is_err());
        assert_eq!(
            store.get_ranges(&path, &[8..12]).await.unwrap(),
            vec![Bytes::from_static(b"ijkl")]
        );
    }

    #[tokio::test]
    async fn overwrite_invalidates_cached_ranges() {
        let (_original, store, _temp) = test_store(1024).await;
        let path = Path::from("db/chunks.lance/data/vectors.lance");
        store
            .put(&path, Bytes::from_static(b"old-value").into())
            .await
            .unwrap();
        assert_eq!(
            store.get_ranges(&path, &[0..3]).await.unwrap(),
            vec![Bytes::from_static(b"old")]
        );
        store
            .put(&path, Bytes::from_static(b"new-value").into())
            .await
            .unwrap();
        assert_eq!(
            store.get_ranges(&path, &[0..3]).await.unwrap(),
            vec![Bytes::from_static(b"new")]
        );
    }

    #[tokio::test]
    async fn whole_immutable_object_survives_remote_removal() {
        let (original, store, _temp) = test_store(1024).await;
        let path = Path::from("db/chunks.lance/data/small.lance");
        original.put(&path, Bytes::from_static(b"abcdefghij").into()).await.unwrap();

        let first = store.get(&path).await.unwrap();
        assert_eq!(first.meta.size, 10);
        assert_eq!(first.bytes().await.unwrap(), Bytes::from_static(b"abcdefghij"));
        original.delete(&path).await.unwrap();
        let second = store.get(&path).await.unwrap();
        assert_eq!((second.meta.size, second.range.clone()), (10, 0..10));
        assert_eq!(second.bytes().await.unwrap(), Bytes::from_static(b"abcdefghij"));
    }

    #[tokio::test]
    async fn single_range_reads_are_served_from_overlapping_blocks() {
        let (original, store, _temp) = test_store(1024).await;
        let path = Path::from("db/chunks.lance/data/vectors.lance");
        original.put(&path, Bytes::from_static(b"abcdefghijkl").into()).await.unwrap();

        assert_eq!(store.get_range(&path, 1..6).await.unwrap(), Bytes::from_static(b"bcdef"));
        original.delete(&path).await.unwrap();
        // Different ranges inside the blocks fetched above (0..4, 4..8).
        assert_eq!(store.get_range(&path, 2..5).await.unwrap(), Bytes::from_static(b"cde"));
        assert_eq!(store.get_range(&path, 4..8).await.unwrap(), Bytes::from_static(b"efgh"));
        assert!(store.get_range(&path, 7..10).await.is_err());
    }

    #[tokio::test]
    async fn range_past_end_is_clamped_like_the_remote() {
        let (original, store, _temp) = test_store(1024).await;
        let path = Path::from("db/chunks.lance/data/vectors.lance");
        original.put(&path, Bytes::from_static(b"abcdefghij").into()).await.unwrap();
        let options = || GetOptions::new().with_range(Some(GetRange::Bounded(6..20)));

        let remote = original.get_opts(&path, options()).await.unwrap().range;
        let fetched = store.get_opts(&path, options()).await.unwrap();
        assert_eq!(fetched.range, remote);
        assert_eq!(fetched.bytes().await.unwrap(), Bytes::from_static(b"ghij"));
        original.delete(&path).await.unwrap();
        let cached = store.get_opts(&path, options()).await.unwrap();
        assert_eq!(cached.range, remote);
        assert_eq!(cached.bytes().await.unwrap(), Bytes::from_static(b"ghij"));
        assert!(store.get_range(&path, 10..12).await.is_err());
    }

    /// Warms a table with one index and one data file, removes both remotely,
    /// and reports whether each is still readable from the cache.
    async fn warm_then_read(max_bytes: u64) -> (bool, bool) {
        let (original, store, _temp) = test_store(max_bytes).await;
        let data = Path::from("db/chunks.lance/data/a.lance");
        let index = Path::from("db/chunks.lance/_indices/ivf/part.idx");
        let index_bytes = Bytes::from_static(b"0123456789abcdefghijklmnopqrstuvwxyz");
        original.put(&data, Bytes::from_static(b"abcdefghij").into()).await.unwrap();
        original.put(&index, index_bytes.clone().into()).await.unwrap();
        assert_eq!(table_root(&data), Some(Path::from("db/chunks.lance")));

        store.warm_root(Path::from("db/chunks.lance")).await;
        original.delete(&data).await.unwrap();
        original.delete(&index).await.unwrap();
        let index_cached = match store.get(&index).await {
            Ok(result) => result.bytes().await.unwrap() == index_bytes,
            Err(_) => false,
        };
        (index_cached, store.get_range(&data, 2..5).await.is_ok())
    }

    #[tokio::test]
    async fn warm_up_copies_indexes_and_data_that_fit_half_the_cache() {
        assert_eq!(warm_then_read(4096).await, (true, true));
    }

    #[tokio::test]
    async fn warm_up_skips_data_files_when_the_table_exceeds_half_the_cache() {
        // 36 index bytes + 10 data bytes > 80 / 2, but the index alone fits.
        assert_eq!(warm_then_read(80).await, (true, false));
    }

    #[tokio::test]
    async fn conditional_and_mutable_reads_bypass_the_cache() {
        let (original, store, _temp) = test_store(1024).await;
        let data = Path::from("db/chunks.lance/data/vectors.lance");
        let manifest = Path::from("db/chunks.lance/_versions/3.manifest");
        original.put(&data, Bytes::from_static(b"abcdefgh").into()).await.unwrap();
        original.put(&manifest, Bytes::from_static(b"manifest").into()).await.unwrap();

        store.get(&data).await.unwrap().bytes().await.unwrap();
        store.get(&manifest).await.unwrap().bytes().await.unwrap();
        original.delete(&data).await.unwrap();
        original.delete(&manifest).await.unwrap();
        assert!(store.get(&manifest).await.is_err());
        assert!(store.get_opts(&data, GetOptions::new().with_if_match(Some("*"))).await.is_err());
        assert!(store.get(&data).await.is_ok());
    }
}
