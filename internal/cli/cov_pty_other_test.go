//go:build !darwin && !linux

package cli_test

import (
	"errors"
	"os"
)

func openTerminal() (*os.File, func(), error) {
	return nil, nil, errors.New("no pseudo-terminal helper for this platform")
}
