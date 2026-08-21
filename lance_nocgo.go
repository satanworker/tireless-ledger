//go:build !cgo

package main

import (
	"context"
	"errors"
)

func openLanceStore(context.Context, runtimeConfig) (lanceStore, error) {
	return nil, errors.New("Lance storage requires the Linux CGO build; use memory:// with CGO_ENABLED=0")
}
