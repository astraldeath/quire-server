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
