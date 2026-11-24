//go:build !linux && !darwin

package store

import "errors"

func cloneFile(_, _ string) error {
	return errors.ErrUnsupported
}
