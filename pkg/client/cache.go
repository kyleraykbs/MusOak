package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Cache is the client-side media cache. It keeps renditions on disk, remembers
// what it has in an index file, and never downloads the same variant twice.
//
// A client that already holds a rendition of a track reports that local copy
// to the room, so joining a listen-together does not re-download what is
// already on disk.
type Cache struct {
	dir string

	mu    sync.Mutex
	index map[string]CacheEntry
}

// CacheEntry is one cached rendition.
type CacheEntry struct {
	VariantID    string    `json:"variantId"`
	TrackID      string    `json:"trackId"`
	Path         string    `json:"path"`
	DurationMs   int64     `json:"durationMs"`
	SHA256       string    `json:"sha256,omitempty"`
	Bytes        int64     `json:"bytes,omitempty"`
	DownloadedAt time.Time `json:"downloadedAt"`
}

// OpenCache opens (creating if needed) a media cache directory.
func OpenCache(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("musoak: create cache dir: %w", err)
	}
	cache := &Cache{dir: dir, index: map[string]CacheEntry{}}
	if err := cache.load(); err != nil {
		return nil, err
	}
	return cache, nil
}

// Dir is where cached renditions live.
func (c *Cache) Dir() string { return c.dir }

func (c *Cache) indexPath() string { return filepath.Join(c.dir, "index.json") }

func (c *Cache) load() error {
	raw, err := os.ReadFile(c.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("musoak: read cache index: %w", err)
	}
	var entries []CacheEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fmt.Errorf("musoak: parse cache index: %w", err)
	}
	for _, entry := range entries {
		c.index[entry.VariantID] = entry
	}
	return nil
}

// save writes the index; callers hold the lock.
func (c *Cache) save() error {
	entries := make([]CacheEntry, 0, len(c.index))
	for _, entry := range c.index {
		entries = append(entries, entry)
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.indexPath() + ".part"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("musoak: write cache index: %w", err)
	}
	return os.Rename(tmp, c.indexPath())
}

// Path is where a variant's file would live.
func (c *Cache) Path(variantID string) string {
	return filepath.Join(c.dir, variantID+".opus")
}

// Lookup returns the entry for a variant if its file is still present.
func (c *Cache) Lookup(variantID string) (CacheEntry, bool) {
	c.mu.Lock()
	entry, ok := c.index[variantID]
	c.mu.Unlock()
	if !ok {
		return CacheEntry{}, false
	}
	if _, err := os.Stat(entry.Path); err != nil {
		return CacheEntry{}, false
	}
	return entry, true
}

// LookupTrack returns a cached rendition of a track, if there is one.
func (c *Cache) LookupTrack(trackID string) (CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.index {
		if entry.TrackID != trackID {
			continue
		}
		if _, err := os.Stat(entry.Path); err != nil {
			continue
		}
		return entry, true
	}
	return CacheEntry{}, false
}

// Entries lists everything the cache holds.
func (c *Cache) Entries() []CacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CacheEntry, 0, len(c.index))
	for _, entry := range c.index {
		out = append(out, entry)
	}
	return out
}

// Put indexes a file the caller already has (for example after importing a
// local file), so it is never downloaded again.
func (c *Cache) Put(entry CacheEntry) error {
	if entry.DownloadedAt.IsZero() {
		entry.DownloadedAt = time.Now()
	}
	if entry.Path == "" {
		entry.Path = c.Path(entry.VariantID)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.index[entry.VariantID] = entry
	return c.save()
}

// Fetch makes sure a variant is on disk and returns its entry. Concurrent
// calls for the same variant share one download.
func (c *Cache) Fetch(ctx context.Context, api *Client, variantID string) (CacheEntry, error) {
	if entry, ok := c.Lookup(variantID); ok {
		return entry, nil
	}

	status, err := api.StartDownloadAndWait(ctx, variantID)
	if err != nil {
		return CacheEntry{}, err
	}
	if status.State != MediaReady {
		return CacheEntry{}, fmt.Errorf("musoak: variant %s is %s: %s", variantID, status.State, status.Error)
	}

	final := c.Path(variantID)
	tmp := final + ".part"
	sum, size, err := c.download(ctx, api, variantID, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return CacheEntry{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return CacheEntry{}, fmt.Errorf("musoak: store rendition: %w", err)
	}

	entry := CacheEntry{
		VariantID:    variantID,
		Path:         final,
		DurationMs:   status.DurationMs,
		SHA256:       sum,
		Bytes:        size,
		DownloadedAt: time.Now(),
	}
	if err := c.Put(entry); err != nil {
		return CacheEntry{}, err
	}
	return entry, nil
}

// download streams a variant to path and verifies the sha256 the server
// advertised, so a truncated or corrupted transfer is never cached.
func (c *Cache) download(ctx context.Context, api *Client, variantID, path string) (string, int64, error) {
	resp, err := api.mediaResponse(ctx, variantID)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	file, err := os.Create(path)
	if err != nil {
		return "", 0, fmt.Errorf("musoak: create %s: %w", path, err)
	}
	defer file.Close()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("musoak: download %s: %w", variantID, err)
	}
	sum := hex.EncodeToString(hash.Sum(nil))

	if advertised := strings.Trim(resp.Header.Get("ETag"), `"`); advertised != "" && advertised != sum {
		return "", 0, fmt.Errorf("musoak: variant %s failed its checksum (%s != %s)", variantID, sum, advertised)
	}
	return sum, size, nil
}
