package stashgram

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type FileCache struct {
	dir       string
	maxSize   int64 // in bytes
	expireDur time.Duration
	mu        sync.RWMutex
	index     map[string]*cacheEntry
}

type cacheEntry struct {
	Path      string
	Size      int64
	CreatedAt time.Time
	ExpiresAt time.Time
	Key       string
}

func NewFileCache(maxSizeMB int, expireDays int) *FileCache {
	cacheDir := filepath.Join(".stashcli_cache")
	os.MkdirAll(cacheDir, 0755)

	fc := &FileCache{
		dir:       cacheDir,
		maxSize:   int64(maxSizeMB) * 1024 * 1024,
		expireDur: time.Duration(expireDays) * 24 * time.Hour,
		index:     make(map[string]*cacheEntry),
	}

	// Load existing cache index
	fc.loadIndex()
	go fc.cleanupLoop()

	return fc
}

func (fc *FileCache) cachePath(key string) string {
	hash := sha256.Sum256([]byte(key))
	return filepath.Join(fc.dir, hex.EncodeToString(hash[:]))
}

func (fc *FileCache) Has(key string) bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	entry, exists := fc.index[key]
	if !exists {
		return false
	}

	if time.Now().After(entry.ExpiresAt) {
		return false
	}

	// Verify file still exists
	if _, err := os.Stat(entry.Path); os.IsNotExist(err) {
		return false
	}

	return true
}

func (fc *FileCache) Get(key string) ([]byte, error) {
	fc.mu.RLock()
	entry, exists := fc.index[key]
	fc.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("cache miss: %s", key)
	}

	if time.Now().After(entry.ExpiresAt) {
		fc.Delete(key)
		return nil, fmt.Errorf("cache expired: %s", key)
	}

	data, err := os.ReadFile(entry.Path)
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (fc *FileCache) GetRaw(key string) ([]byte, bool) {
	data, err := fc.Get(key)
	return data, err == nil
}

func (fc *FileCache) Set(key string, data []byte) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	// Check if we need to evict
	fc.evictIfNeeded(int64(len(data)))

	path := fc.cachePath(key)
	entry := &cacheEntry{
		Path:      path,
		Size:      int64(len(data)),
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(fc.expireDur),
		Key:       key,
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return
	}

	fc.index[key] = entry
	fc.saveIndex()
}

func (fc *FileCache) Delete(key string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	if entry, exists := fc.index[key]; exists {
		os.Remove(entry.Path)
		delete(fc.index, key)
		fc.saveIndex()
	}
}

// DeletePrefix removes every cached entry whose key starts with prefix.
// Used when a folder (and everything under it) is deleted or renamed, so
// stale per-file / per-chunk cache entries for paths that no longer exist
// don't just sit around until they naturally expire — they're cleared
// immediately, freeing that disk space right away.
func (fc *FileCache) DeletePrefix(prefix string) {
	if prefix == "" {
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()

	changed := false
	for key, entry := range fc.index {
		if strings.HasPrefix(key, prefix) {
			os.Remove(entry.Path)
			delete(fc.index, key)
			changed = true
		}
	}
	if changed {
		fc.saveIndex()
	}
}

func (fc *FileCache) Clear() {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	for _, entry := range fc.index {
		os.Remove(entry.Path)
	}
	fc.index = make(map[string]*cacheEntry)
	fc.saveIndex()
}

// evictIfNeeded removes expired entries first, then — if we're still over
// budget — removes the truly oldest entries (by CreatedAt) until there's
// room. Iterating in CreatedAt order (rather than random map order) means
// frequently-reused chunks survive evictions while stale, once-touched
// chunks go first — better cache hit rate, fewer repeat Telegram downloads.
func (fc *FileCache) evictIfNeeded(needed int64) {
	var totalSize int64
	for _, entry := range fc.index {
		totalSize += entry.Size
	}

	// Remove expired entries first.
	for key, entry := range fc.index {
		if time.Now().After(entry.ExpiresAt) {
			os.Remove(entry.Path)
			delete(fc.index, key)
			totalSize -= entry.Size
		}
	}

	if totalSize+needed <= fc.maxSize {
		return
	}

	// Still over budget: evict oldest-first.
	type kv struct {
		key   string
		entry *cacheEntry
	}
	entries := make([]kv, 0, len(fc.index))
	for k, e := range fc.index {
		entries = append(entries, kv{k, e})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].entry.CreatedAt.Before(entries[j].entry.CreatedAt)
	})

	for _, e := range entries {
		if totalSize+needed <= fc.maxSize {
			break
		}
		os.Remove(e.entry.Path)
		delete(fc.index, e.key)
		totalSize -= e.entry.Size
	}
}

func (fc *FileCache) loadIndex() {
	indexPath := filepath.Join(fc.dir, "index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return
	}

	var index map[string]*cacheEntry
	if err := json.Unmarshal(data, &index); err != nil {
		return
	}

	// Validate entries
	for key, entry := range index {
		if _, err := os.Stat(entry.Path); os.IsNotExist(err) {
			delete(index, key)
			continue
		}
		if time.Now().After(entry.ExpiresAt) {
			os.Remove(entry.Path)
			delete(index, key)
			continue
		}
	}

	fc.index = index
}

func (fc *FileCache) saveIndex() {
	indexPath := filepath.Join(fc.dir, "index.json")
	data, err := json.Marshal(fc.index)
	if err != nil {
		return
	}
	os.WriteFile(indexPath, data, 0644)
}

func (fc *FileCache) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	for range ticker.C {
		fc.mu.Lock()
		for key, entry := range fc.index {
			if time.Now().After(entry.ExpiresAt) {
				os.Remove(entry.Path)
				delete(fc.index, key)
			}
		}
		fc.saveIndex()
		fc.mu.Unlock()
	}
}
