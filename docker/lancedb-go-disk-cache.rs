// SPDX-License-Identifier: Apache-2.0
//
// Bounded, persistent read-through cache for immutable Lance object ranges.

use async_trait::async_trait;
use bytes::Bytes;
use futures::{stream::BoxStream, StreamExt};
use lance::dataset::ReadParams;
use lance::io::{ObjectStoreParams, WrappingObjectStore};
use object_store::{
    path::Path, CopyOptions, GetOptions, GetResult, ListResult, MultipartUpload, ObjectMeta,
    ObjectStore, PutMultipartOptions, PutOptions, PutPayload, PutResult, RenameOptions,
    Result as ObjectStoreResult,
};
use sha2::{Digest, Sha256};
use std::collections::HashMap;
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
    state: Arc<Mutex<CacheState>>,
    metrics: Arc<CacheMetrics>,
    temp_id: Arc<AtomicU64>,
}

impl DiskRangeCache {
    fn new(root: PathBuf, max_bytes: u64) -> std::io::Result<Self> {
        fs::create_dir_all(&root)?;
        let mut state = CacheState::default();
        load_existing_entries(&root, &mut state)?;
        let cache = Self {
            root: Arc::new(root),
            max_bytes,
            state: Arc::new(Mutex::new(state)),
            metrics: Arc::new(CacheMetrics::default()),
            temp_id: Arc::new(AtomicU64::new(0)),
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
        let mut state = self
            .state
            .lock()
            .unwrap_or_else(|poisoned| poisoned.into_inner());
        match fs::read(&path) {
            Ok(data) if data.len() == expected_len => {
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

fn cacheable(location: &Path) -> bool {
    location
        .as_ref()
        .split('/')
        .any(|part| matches!(part, "data" | "_indices" | "_deletions"))
}

#[derive(Debug)]
struct DiskCacheWrapper {
    cache: DiskRangeCache,
}

impl WrappingObjectStore for DiskCacheWrapper {
    fn wrap(&self, namespace: &str, original: Arc<dyn ObjectStore>) -> Arc<dyn ObjectStore> {
        Arc::new(DiskCacheStore {
            original,
            cache: self.cache.clone(),
            namespace: namespace.to_string(),
        })
    }
}

#[derive(Debug)]
struct DiskCacheStore {
    original: Arc<dyn ObjectStore>,
    cache: DiskRangeCache,
    namespace: String,
}

impl Display for DiskCacheStore {
    fn fmt(&self, f: &mut Formatter<'_>) -> std::fmt::Result {
        write!(f, "DiskCacheStore({})", self.original)
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
        self.original.get_opts(location, options).await
    }

    async fn get_ranges(
        &self,
        location: &Path,
        ranges: &[Range<u64>],
    ) -> ObjectStoreResult<Vec<Bytes>> {
        if !cacheable(location) {
            return self.original.get_ranges(location, ranges).await;
        }

        let location_string = location.as_ref().to_string();
        let mut output = vec![None; ranges.len()];
        let mut misses = Vec::new();
        for (index, range) in ranges.iter().enumerate() {
            let hit = self
                .cache
                .get(
                    self.namespace.clone(),
                    location_string.clone(),
                    range.clone(),
                )
                .await;
            if let Some(bytes) = hit {
                output[index] = Some(bytes);
            } else {
                misses.push((index, range.clone()));
            }
        }

        if !misses.is_empty() {
            let missing_ranges: Vec<_> = misses.iter().map(|(_, range)| range.clone()).collect();
            let fetched = self.original.get_ranges(location, &missing_ranges).await?;
            if fetched.len() != misses.len() {
                return Err(object_store::Error::Generic {
                    store: "LanceDiskCache",
                    source: std::io::Error::other(format!(
                        "get_ranges returned {} results for {} ranges",
                        fetched.len(),
                        misses.len()
                    ))
                    .into(),
                });
            }
            for ((index, range), bytes) in misses.into_iter().zip(fetched) {
                output[index] = Some(bytes.clone());
                self.cache
                    .put(
                        self.namespace.clone(),
                        location_string.clone(),
                        range,
                        bytes,
                    )
                    .await;
            }
        }

        output
            .into_iter()
            .map(|value| {
                value.ok_or_else(|| object_store::Error::Generic {
                    store: "LanceDiskCache",
                    source: std::io::Error::other("range result was not populated").into(),
                })
            })
            .collect()
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
            match DiskRangeCache::new(PathBuf::from(root), max_bytes) {
                Ok(cache) => {
                    eprintln!(
                        "lancedb-go disk range cache dir={} max_bytes={} existing_bytes={}",
                        cache.root.display(),
                        max_bytes,
                        cache.initial_bytes()
                    );
                    Some(Arc::new(DiskCacheWrapper { cache }))
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
        let cache = DiskRangeCache::new(temp.path().to_path_buf(), max_bytes).unwrap();
        let original = Arc::new(InMemory::new());
        let store = DiskCacheStore {
            original: original.clone(),
            cache,
            namespace: "s3$test".to_string(),
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
        assert_eq!(store.cache.stats(), (1, 1, 6, 6));
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
        let (original, store, _temp) = test_store(8).await;
        let path = Path::from("db/chunks.lance/data/vectors.lance");
        original.put(&path, Bytes::from_static(b"abcdefghijkl").into()).await.unwrap();

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
}
