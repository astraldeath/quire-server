// SPDX-License-Identifier: AGPL-3.0-only
package server

import "errors"

const DefaultMaxUploadBytes int64 = 2 << 30
const MaxStoredBookBytes int64 = 8 << 30

// Upload policy is independent of the persistent book/backup format ceiling.
type StoreOptions struct{ MaxUploadBytes int64 }

var ErrUploadTooLarge = errors.New("book exceeds configured upload limit")

func (options StoreOptions) validate() error {
	if options.MaxUploadBytes < 1 || options.MaxUploadBytes > MaxStoredBookBytes {
		return errors.New("max-upload-bytes must be between 1 and 8589934592")
	}
	return nil
}
