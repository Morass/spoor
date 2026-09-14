//go:build !linux

package try

import (
	"errors"
	"runtime"
)

var errUnsupported = errors.New("spoor try needs Linux overlayfs; on " + runtime.GOOS + " use `spoor run` and `spoor revert` instead")

func Supported() error                     { return errUnsupported }
func Run(Spec) (int, error)                { return 0, errUnsupported }
func Child(string)                         {}
func Changes(Spec) ([]Change, error)       { return nil, errUnsupported }
func Apply(Spec, []Change) error           { return errUnsupported }
func OpaqueRemovals(Change) []string       { return nil }
