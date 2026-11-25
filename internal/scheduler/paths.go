package scheduler

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

type pathResult struct {
	task   store.PathTask
	sample store.PathSample
}

func RunPathChecks(ctx context.Context, db *store.Store, saturated *atomic.Bool) {
	const maxActive = 2
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var workers sync.WaitGroup
	defer workers.Wait()
	results := make(chan pathResult, maxActive)
	active := make(map[int64]struct{}, maxActive)
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-results:
			delete(active, result.task.ID)
			if err := db.RecordPathCheck(ctx, result.task, result.sample); err != nil && ctx.Err() == nil {
				log.Printf("记录路径 %d 的检测: %v", result.task.ID, err)
			}
		case now := <-ticker.C:
			if len(active) == maxActive || saturated.Load() {
				continue
			}
			tasks, err := db.DuePathChecks(ctx, now, maxActive)
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("读取待执行的路径检测: %v", err)
				}
				continue
			}
			for _, task := range tasks {
				if _, running := active[task.ID]; running {
					continue
				}
				if len(active) == maxActive {
					break
				}
				active[task.ID] = struct{}{}
				workers.Go(func() {
					sample := store.PathSample{Address: task.Address, Started: time.Now()}
					trace, err := monitor.Trace(ctx, task.Address)
					sample.Completed, sample.Result = time.Now(), trace
					if err != nil {
						sample.Message = err.Error()
					}
					select {
					case results <- pathResult{task, sample}:
					case <-ctx.Done():
					}
				})
			}
		}
	}
}
