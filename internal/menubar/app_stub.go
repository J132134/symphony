//go:build !darwin

package menubar

import "fmt"

func Run(opts Options) error {
	return fmt.Errorf("symphony menubar is only supported on macOS")
}
