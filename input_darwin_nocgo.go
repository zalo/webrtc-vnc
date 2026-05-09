//go:build darwin && !cgo

package main

import "log"

// Input stub used when cgo is disabled (e.g. cross-compiling darwin/amd64
// from a non-macOS host without an osxcross toolchain). Builds successfully
// but cannot post CGEvents. For a usable macOS server, build on a Mac with
// CGO_ENABLED=1.
type Input struct{}

func NewInput() (*Input, error) {
	log.Printf("Input: cgo disabled — input injection is a no-op (build with CGO_ENABLED=1 on macOS)")
	return &Input{}, nil
}

func (*Input) Close()                                  {}
func (*Input) EnsureGamepad(slot int)                  { _ = slot }
func (*Input) HandleKeyboard(data []byte)              { _ = data }
func (*Input) HandleMouseMove(data []byte)             { _ = data }
func (*Input) HandleMouseButton(data []byte)           { _ = data }
func (*Input) HandleMouseScroll(data []byte)           { _ = data }
func (*Input) HandleGamepad(slot int, data []byte)     { _ = slot; _ = data }
