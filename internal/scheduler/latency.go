package scheduler

import (
	"context"
	"log"
	"time"

	"netpatrol/internal/store"
)

func RunLatencyPruning(ctx context.Context, db *store.Store) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if err := db.PruneLatency(ctx, time.Now()); err != nil && ctx.Err() == nil {
			log.Printf("清理延迟历史: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
