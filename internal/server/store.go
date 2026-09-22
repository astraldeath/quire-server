// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"database/sql"
	"errors"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	maxUploadBytes int64
	db             *sql.DB
	data           string
	fileMu         sync.Mutex
	backupMu       sync.Mutex
	trackingMu     sync.Mutex
	metadata       *metadataCache
}

func Open(path string) (*Store, error) {
	return OpenWithOptions(path, StoreOptions{MaxUploadBytes: DefaultMaxUploadBytes})
}

func OpenWithOptions(path string, options StoreOptions) (*Store, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Create with private permissions before SQLite opens it on Unix.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	if _, err = db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;`); err != nil {
		return fail(err)
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fail(err)
	}
	if version > 14 {
		return fail(errors.New("database was created by a newer server"))
	}
	if version == 0 {
		tx, err := db.Begin()
		if err != nil {
			return fail(err)
		}
		_, err = tx.Exec(`
CREATE TABLE users (id TEXT PRIMARY KEY, username TEXT UNIQUE NOT NULL, salt BLOB NOT NULL, password_hash BLOB NOT NULL);
CREATE TABLE sessions (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), token_hash BLOB UNIQUE NOT NULL, device_name TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE cursors (user_id TEXT PRIMARY KEY REFERENCES users(id), value INTEGER NOT NULL);
CREATE TABLE records (user_id TEXT NOT NULL REFERENCES users(id), book_id TEXT NOT NULL, kind TEXT NOT NULL, record_id TEXT NOT NULL, revision INTEGER NOT NULL, candidates TEXT NOT NULL, PRIMARY KEY(user_id,book_id,kind,record_id));
CREATE TABLE changes (user_id TEXT NOT NULL REFERENCES users(id), cursor INTEGER NOT NULL, record TEXT NOT NULL, PRIMARY KEY(user_id,cursor));
CREATE TABLE operations (user_id TEXT NOT NULL REFERENCES users(id), id TEXT NOT NULL, digest BLOB NOT NULL, result TEXT NOT NULL, PRIMARY KEY(user_id,id));
PRAGMA user_version=1;`)
		if err != nil {
			tx.Rollback()
			return fail(err)
		}
		if err = tx.Commit(); err != nil {
			return fail(err)
		}
	}
	if version < 2 {
		_, err = db.Exec(`BEGIN; CREATE TABLE watch_roots (id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id),root TEXT NOT NULL,identity TEXT NOT NULL,UNIQUE(user_id,root)); CREATE TABLE files (user_id TEXT NOT NULL REFERENCES users(id),book_id TEXT NOT NULL,kind TEXT NOT NULL,source TEXT NOT NULL,size INTEGER NOT NULL,PRIMARY KEY(user_id,book_id,kind,source)); PRAGMA user_version=2; COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 3 {
		_, err = db.Exec(`BEGIN;
 ALTER TABLE users ADD COLUMN admin INTEGER NOT NULL DEFAULT 0;
 ALTER TABLE users ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
 CREATE TABLE invites (id TEXT PRIMARY KEY, code_hash BLOB UNIQUE NOT NULL, expires_at INTEGER NOT NULL, redeemed_by TEXT REFERENCES users(id), revoked INTEGER NOT NULL DEFAULT 0);
 PRAGMA user_version=3; COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 4 {
		_, err = db.Exec(`BEGIN;
 CREATE TABLE libraries(id TEXT PRIMARY KEY,name TEXT NOT NULL,owner TEXT NOT NULL UNIQUE REFERENCES users(id));
 CREATE TABLE library_members(library_id TEXT NOT NULL REFERENCES libraries(id),user_id TEXT NOT NULL REFERENCES users(id),PRIMARY KEY(library_id,user_id));
 CREATE TABLE invite_libraries(invite_id TEXT NOT NULL REFERENCES invites(id),library_id TEXT NOT NULL REFERENCES libraries(id),PRIMARY KEY(invite_id,library_id));
 PRAGMA user_version=4;COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 5 {
		_, err = db.Exec(`BEGIN;CREATE TABLE server_settings(id INTEGER PRIMARY KEY CHECK(id=1),name TEXT NOT NULL,scan_seconds INTEGER NOT NULL);CREATE TABLE scan_status(watch_id TEXT PRIMARY KEY,last_at INTEGER NOT NULL,error TEXT NOT NULL);PRAGMA user_version=5;COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 6 {
		_, err = db.Exec(`BEGIN;ALTER TABLE scan_status ADD COLUMN imported INTEGER NOT NULL DEFAULT 0;ALTER TABLE scan_status ADD COLUMN existing INTEGER NOT NULL DEFAULT 0;ALTER TABLE scan_status ADD COLUMN skipped INTEGER NOT NULL DEFAULT 0;PRAGMA user_version=6;COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 7 {
		_, err = db.Exec(`BEGIN;
CREATE TABLE tracking_accounts(user_id TEXT PRIMARY KEY REFERENCES users(id),provider_id TEXT NOT NULL,name TEXT NOT NULL,secret BLOB NOT NULL);
CREATE TABLE tracking_links(user_id TEXT NOT NULL REFERENCES users(id),book_id TEXT NOT NULL,series_key TEXT NOT NULL,series_id INTEGER NOT NULL,title TEXT NOT NULL,volume REAL NOT NULL,auto INTEGER NOT NULL,complete_entry INTEGER NOT NULL,last_step INTEGER NOT NULL DEFAULT 0,last_sync INTEGER NOT NULL DEFAULT 0,error TEXT NOT NULL DEFAULT '',next_attempt INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(user_id,book_id));
PRAGMA user_version=7;COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 8 {
		_, err = db.Exec(`BEGIN; ALTER TABLE tracking_links ADD COLUMN last_chapter INTEGER NOT NULL DEFAULT 0; PRAGMA user_version=8; COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 9 {
		_, err = db.Exec(`BEGIN; CREATE TABLE reading_activities(user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,id TEXT NOT NULL,cursor INTEGER NOT NULL,activity TEXT NOT NULL,PRIMARY KEY(user_id,id),UNIQUE(user_id,cursor)); PRAGMA user_version=9; COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 10 {
		_, err = db.Exec(`BEGIN; CREATE TABLE privacy_settings(user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,revision INTEGER NOT NULL,state TEXT NOT NULL); PRAGMA user_version=10; COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 11 {
		_, err = db.Exec(`BEGIN; ALTER TABLE tracking_links ADD COLUMN is_private INTEGER NOT NULL DEFAULT 1; PRAGMA user_version=11; COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 12 {
		_, err = db.Exec(`BEGIN;
ALTER TABLE scan_status ADD COLUMN skipped_files TEXT NOT NULL DEFAULT '[]';
ALTER TABLE scan_status ADD COLUMN omitted_skipped_files INTEGER NOT NULL DEFAULT 0;
PRAGMA user_version=12;COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 13 {
		_, err = db.Exec(`BEGIN; CREATE TABLE folder_catalog(user_id TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,revision INTEGER NOT NULL,state TEXT NOT NULL); PRAGMA user_version=13; COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	if version < 14 {
		_, err = db.Exec(`BEGIN;
CREATE TABLE catalog_sources(user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,id TEXT NOT NULL,name TEXT NOT NULL,url TEXT NOT NULL,revision INTEGER NOT NULL,deleted INTEGER NOT NULL,secret BLOB,PRIMARY KEY(user_id,id));
CREATE TABLE catalog_operations(user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,id TEXT NOT NULL,digest BLOB NOT NULL,result TEXT NOT NULL,PRIMARY KEY(user_id,id));
CREATE TABLE opds_passwords(id TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,name TEXT NOT NULL,digest BLOB UNIQUE NOT NULL,created_at INTEGER NOT NULL);
PRAGMA user_version=14;COMMIT;`)
		if err != nil {
			return fail(err)
		}
	}
	return &Store{maxUploadBytes: options.MaxUploadBytes, db: db, data: filepath.Dir(path), metadata: newMetadataCache(128, 16<<20, formatMetadata)}, nil
}
func (s *Store) Close() error { return s.db.Close() }
