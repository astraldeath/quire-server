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
	db       *sql.DB
	data     string
	fileMu   sync.Mutex
	backupMu sync.Mutex
}

func Open(path string) (*Store, error) {
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
	if version > 6 {
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
	return &Store{db: db, data: filepath.Dir(path)}, nil
}
func (s *Store) Close() error { return s.db.Close() }
