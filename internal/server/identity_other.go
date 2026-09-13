//go:build !windows && !linux && !darwin

package server

import "errors"

func rootIdentity(path string) (string, error) {
	return "", errors.New("watched folders are unsupported on this server platform")
}
