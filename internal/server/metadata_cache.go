// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"container/list"
	"os"
	"sync"
)

type metadataEntry struct {
	key   string
	value bookMetadata
	size  int
}
type metadataFlight struct {
	done  chan struct{}
	value bookMetadata
}
type metadataCache struct {
	mu                          sync.Mutex
	entries                     map[string]*list.Element
	lru                         *list.List
	flights                     map[string]*metadataFlight
	maxEntries, maxBytes, bytes int
	load                        func(string) bookMetadata
}

func newMetadataCache(entries, bytes int, load func(string) bookMetadata) *metadataCache {
	return &metadataCache{entries: make(map[string]*list.Element), lru: list.New(), flights: make(map[string]*metadataFlight), maxEntries: entries, maxBytes: bytes, load: load}
}

func copyMetadata(m bookMetadata) bookMetadata {
	if m.Volume != nil {
		v := *m.Volume
		m.Volume = &v
	}
	return m
}

// Keys are immutable, hash-verified Store object paths, never watched source paths.
// Decode outside the mutex and share concurrent work for the same object.
func (c *metadataCache) get(key string) bookMetadata {
	c.mu.Lock()
	if e := c.entries[key]; e != nil {
		c.lru.MoveToFront(e)
		value := copyMetadata(e.Value.(metadataEntry).value)
		c.mu.Unlock()
		return value
	}
	if f := c.flights[key]; f != nil {
		c.mu.Unlock()
		<-f.done
		return copyMetadata(f.value)
	}
	f := &metadataFlight{done: make(chan struct{})}
	c.flights[key] = f
	c.mu.Unlock()
	value := c.load(key)
	size := len(key) + len(value.Title) + len(value.Author) + len(value.Series) + len(value.Cover) + 8
	c.mu.Lock()
	if c.maxEntries > 0 && size <= c.maxBytes {
		for c.lru.Len() >= c.maxEntries || c.bytes+size > c.maxBytes {
			old := c.lru.Back()
			entry := old.Value.(metadataEntry)
			delete(c.entries, entry.key)
			c.bytes -= entry.size
			c.lru.Remove(old)
		}
		c.entries[key] = c.lru.PushFront(metadataEntry{key, copyMetadata(value), size})
		c.bytes += size
	}
	f.value = copyMetadata(value)
	delete(c.flights, key)
	close(f.done)
	c.mu.Unlock()
	return value
}

func (s *Store) objectMetadata(user, id string) bookMetadata {
	path := s.objectPath(user, id)
	// Do not serve a cached cover for an object that has since been deleted.
	if _, err := os.Stat(path); err != nil {
		return bookMetadata{}
	}
	return s.metadata.get(path)
}
