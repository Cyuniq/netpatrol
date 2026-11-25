//go:build !windows

package main

import "context"

func runApplication(ctx context.Context) error {
	return run(ctx, func() {})
}
