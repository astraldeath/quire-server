// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/term"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"quire.local/server/internal/server"
	"strings"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("public URL must be an origin without credentials, path, query, or fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())) {
		return nil
	}
	return errors.New("public URL requires HTTPS (HTTP is allowed only for localhost development)")
}
func readPassword(path string) (string, error) {
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, 1027))
		if err != nil {
			return "", err
		}
		if len(b) > 1026 {
			return "", errors.New("password file too large")
		}
		return strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"), nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("use an interactive terminal or -password-file; passwords are never accepted as command arguments")
	}
	fmt.Fprint(os.Stderr, "Password (at least 12 characters): ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Confirm password: ")
	confirmation, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(b) != string(confirmation) {
		return "", errors.New("passwords do not match")
	}
	return string(b), nil
}
func run(args []string) error {
	command := "serve"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if command != "backup" && command != "restore" && command != "admin-promote" && command != "serve" && command != "user-add" && command != "password-reset" && command != "watch-add" && command != "scan" && command != "watch-list" && command != "watch-remove" {
		return errors.New("usage: quire-server [serve|backup|restore|user-add|password-reset|watch-add|watch-list|watch-remove|scan] [flags]")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	data := flags.String("data", env("QUIRE_DATA", "./data"), "persistent data directory")

	if command == "backup" || command == "restore" {
		output := flags.String("output", "", "new backup archive filename")
		input := flags.String("input", "", "server backup archive to restore")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		if command == "restore" {
			if *input == "" {
				return errors.New("-input is required; -data must name a new directory")
			}
			if err := server.RestoreBackup(*input, *data); err != nil {
				return err
			}
			fmt.Println("Restored to", *data, ". Sessions revoked; automatic scans paused. Check watched folders before enabling scans.")
			return nil
		}
		if *output == "" {
			return errors.New("-output is required")
		}
		database := filepath.Join(*data, "quire.db")
		if _, err := os.Stat(database); err != nil {
			return err
		}
		store, err := server.Open(database)
		if err != nil {
			return err
		}
		defer store.Close()
		if err = store.Backup(context.Background(), *output); err != nil {
			return err
		}
		fmt.Println("Backup saved to", *output)
		return nil
	}
	if command == "watch-add" || command == "scan" || command == "watch-list" || command == "watch-remove" {
		username := flags.String("username", "", "owner of the watched library")
		root := flags.String("path", "", "read-only EPUB folder")
		idFlag := flags.String("id", "", "watched folder ID")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		store, err := server.Open(filepath.Join(*data, "quire.db"))
		if err != nil {
			return err
		}
		defer store.Close()
		if command == "watch-list" {
			w, err := store.Watches()
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(w)
		}
		if command == "watch-remove" {
			if *idFlag == "" {
				return errors.New("-id is required")
			}
			return store.RemoveWatch(*idFlag)
		}
		if command == "scan" {
			return store.ScanAll()
		}
		id, err := store.AddWatch(*username, *root)
		if err != nil {
			return err
		}
		fmt.Println("Watched folder added:", id)
		return store.ScanWatch(id)
	}
	if command == "admin-promote" {
		username := flags.String("username", "", "existing account to promote")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if *username == "" || flags.NArg() != 0 {
			return errors.New("-username is required")
		}
		store, err := server.Open(filepath.Join(*data, "quire.db"))
		if err != nil {
			return err
		}
		defer store.Close()
		return store.Promote(*username)
	}
	if command != "serve" {
		username := flags.String("username", "", "account name (lowercase letters, digits, dot, dash, underscore)")
		passwordFile := flags.String("password-file", "", "read password from a protected file instead of prompting")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected arguments")
		}
		if *username == "" {
			return errors.New("-username is required")
		}
		password, err := readPassword(*passwordFile)
		if err != nil {
			return err
		}
		store, err := server.Open(filepath.Join(*data, "quire.db"))
		if err != nil {
			return err
		}
		defer store.Close()
		if command == "user-add" {
			err = store.CreateUser(*username, password)
		} else {
			err = store.ResetPassword(*username, password)
		}
		if err == nil {
			fmt.Println("Account updated.")
		}
		return err
	}
	webDir := flags.String("web-dir", env("QUIRE_WEB_DIR", "./web"), "built Quire reader directory")
	listen := flags.String("listen", env("QUIRE_LISTEN", "127.0.0.1:8080"), "HTTP listen address; use a TLS reverse proxy for remote access")
	publicURL := flags.String("public-url", env("QUIRE_PUBLIC_URL", "http://localhost:8080"), "public HTTPS origin used for discovery")
	name := flags.String("name", env("QUIRE_NAME", "Quire"), "server display name")
	origins := flags.String("allowed-origins", env("QUIRE_ALLOWED_ORIGINS", ""), "comma-separated browser origins; empty disables browser access")
	scanInterval := flags.Duration("scan-interval", 5*time.Minute, "watched-folder scan interval; 0 disables background scans")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if err := validateURL(*publicURL); err != nil {
		return err
	}
	store, err := server.Open(filepath.Join(*data, "quire.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	needed, err := store.NeedsSetup()
	if err != nil {
		return err
	}
	setupCode := ""
	if needed {
		var bytes [24]byte
		if _, err = rand.Read(bytes[:]); err != nil {
			return err
		}
		setupCode = hex.EncodeToString(bytes[:])
		log.Printf("First-run setup code: %s", setupCode)
	}
	srv := &http.Server{Addr: *listen, Handler: server.WithCORS(server.WebUI(server.NewConfiguredHandler(store, *publicURL, *name, setupCode), *webDir), append(strings.Split(*origins, ","), strings.TrimRight(*publicURL, "/"))), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Minute, WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		next := time.Now()
		previousDelay := store.ScanDelay(*scanInterval)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				delay := store.ScanDelay(*scanInterval)
				if delay != previousDelay {
					next = time.Now()
					previousDelay = delay
				}
				if delay > 0 && !time.Now().Before(next) {
					if err := store.ScanAll(); err != nil {
						log.Printf("Watched folder scan failed: %v", err)
					}
					next = time.Now().Add(delay)
				}
			}
		}
	}()

	done := make(chan error, 1)
	go func() { log.Printf("Quire listening on %s", *listen); done <- srv.ListenAndServe() }()
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}
func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
