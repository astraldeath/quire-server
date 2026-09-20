package main

import "testing"

func TestPublicURL(t *testing.T) {
	for _, url := range []string{"https://books.example", "https://books.example:8443", "http://127.0.0.1:8080", "http://localhost:8080"} {
		if err := validateURL(url); err != nil {
			t.Errorf("%s: %v", url, err)
		}
	}
	for _, url := range []string{"http://books.example", "https://user:password@books.example", "https://books.example/subpath", "https://books.example?x=1", "not a url"} {
		if validateURL(url) == nil {
			t.Errorf("accepted %s", url)
		}
	}
}

func TestUploadLimitStartupConfiguration(t *testing.T) {
	for _, value := range []string{"", "0", "-1", "8589934593", "9223372036854775808", "nope"} {
		t.Run("env_"+value, func(t *testing.T) {
			t.Setenv("QUIRE_MAX_UPLOAD_BYTES", value)
			if err := run([]string{"watch-list", "-data", t.TempDir()}); err == nil {
				t.Fatalf("accepted invalid upload limit %q", value)
			}
		})
	}
}

func TestUploadLimitFlagOverridesEnvironment(t *testing.T) {
	t.Setenv("QUIRE_MAX_UPLOAD_BYTES", "invalid")
	for _, value := range []string{"1", "8589934592"} {
		if err := run([]string{"watch-list", "-data", t.TempDir(), "-max-upload-bytes", value}); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range []string{"0", "-1", "8589934593", "9223372036854775808"} {
		if err := run([]string{"serve", "-data", t.TempDir(), "-max-upload-bytes", value}); err == nil {
			t.Fatalf("accepted invalid flag %q", value)
		}
	}
}
