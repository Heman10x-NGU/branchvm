//go:build !linux

package ptyclient

import "context"

func termWinsize(uintptr) (uint16, uint16, bool) { return 0, 0, false }

func makeRaw(uintptr) (func(), bool) { return nil, false }

func watchResize(context.Context, uintptr, func(uint16, uint16)) {}
