//go:build !linux

package agent

import "context"

func networkChanges(context.Context) <-chan struct{} { return nil }
