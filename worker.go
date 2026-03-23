package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
)

// runWorker connects to a single coordinator and runs the full agent loop.
// Multiple workers can run concurrently for multi-delegate support.
func runWorker(
	ctx context.Context,
	coordinator, agentID, apiKey, level, region, wallet string,
	tlsCert string, useTLS bool,
	dataDir, snapshot string,
	workerIdx int,
	logger *zap.Logger,
) {
	// Auto-enable TLS when connecting to port 443
	if !useTLS && tlsCert == "" && strings.HasSuffix(coordinator, ":443") {
		useTLS = true
	}

	conn, err := dialCoordinator(coordinator, agentID, apiKey, tlsCert, useTLS)
	if err != nil {
		logger.Error("failed to connect to coordinator", zap.Error(err))
		return
	}
	defer conn.Close()

	// Register
	resp, err := register(ctx, conn, agentID, level, region, wallet, logger)
	if err != nil {
		logger.Error("registration failed", zap.Error(err))
		return
	}

	// Heartbeat
	hbInterval := time.Duration(resp.HeartbeatIntervalSec) * time.Second
	if hbInterval < time.Second {
		hbInterval = 10 * time.Second
	}
	go heartbeatLoop(ctx, conn, agentID, hbInterval, logger)

	// L4: state store (per-worker directory to avoid conflicts)
	// NOTE: multi-delegate L4 is not yet safe — activeStateStore is global.
	// For now, only the first worker (idx=0) initializes L4. Others fall back to L3 task processing.
	if strings.ToUpper(level) == "L4" && dataDir != "" && workerIdx == 0 {
		workerDataDir := dataDir
		if workerIdx > 0 {
			workerDataDir = fmt.Sprintf("%s/worker-%d", dataDir, workerIdx)
		}

		dbPath := workerDataDir + "/state.db"
		stateStore, err := OpenStateStore(dbPath, logger)
		if err != nil {
			logger.Error("failed to open state store", zap.Error(err))
			return
		}
		defer stateStore.Close()

		if snapshot != "" && stateStore.Height() == 0 {
			h, n, err := LoadSnapshot(snapshot, stateStore, logger)
			if err != nil {
				logger.Error("failed to load snapshot", zap.Error(err))
				return
			}
			logger.Info("loaded snapshot", zap.Uint64("height", h), zap.Int("entries", n))
		}

		sync := NewStateSync(stateStore, conn, agentID, logger)
		sync.Start(ctx)
		defer sync.Stop()

		logger.Info("waiting for state sync...")
		readyCtx, readyCancel := context.WithTimeout(ctx, 60*time.Second)
		if err := sync.WaitReady(readyCtx); err != nil {
			readyCancel()
			logger.Error("state sync not ready", zap.Error(err))
			return
		}
		readyCancel()
		logger.Info("state sync ready", zap.Uint64("height", stateStore.Height()))
		activeStateStore.Store(stateStore)
	}

	// Stream tasks (blocks until context cancelled)
	streamTasks(ctx, conn, agentID, level, region, wallet, logger)
}
