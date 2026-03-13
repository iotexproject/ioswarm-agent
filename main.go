package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// gRPC service/method constants (match coordinator's service descriptor).
const (
	methodRegister      = "/ioswarm.IOSwarm/Register"
	methodGetTasks      = "/ioswarm.IOSwarm/GetTasks"
	methodSubmitResults = "/ioswarm.IOSwarm/SubmitResults"
	methodHeartbeat     = "/ioswarm.IOSwarm/Heartbeat"
)

// tasksProcessed tracks the total number of tasks processed by this agent.
var tasksProcessed atomic.Uint32

// activeStateStore is set when running in L4 mode, protected by the
// initialization-before-use pattern (set before streamTasks starts).
var activeStateStore atomic.Pointer[StateStore]

func main() {
	// Subcommands
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "claim":
			runClaim(os.Args[2:])
			return
		case "deploy":
			runDeploy(os.Args[2:])
			return
		case "fund":
			runFund(os.Args[2:])
			return
		case "keygen":
			runKeygen(os.Args[2:])
			return
		case "addr":
			runAddr(os.Args[2:])
			return
		case "status":
			runStatus(os.Args[2:])
			return
		case "service":
			runService(os.Args[2:])
			return
		}
	}

	coordinator := flag.String("coordinator", "127.0.0.1:14689", "coordinator gRPC address")
	apiKey := flag.String("api-key", "", "HMAC API key (iosw_...)")
	agentID := flag.String("agent-id", "", "agent ID (extracted from api-key context, or set manually)")
	level := flag.String("level", "L3", "task level: L1, L2, L3, L4")
	region := flag.String("region", "default", "region label")
	wallet := flag.String("wallet", "", "IOTX wallet address for rewards")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate (optional)")
	useTLS := flag.Bool("tls", false, "use system TLS (for Cloudflare Tunnel / port 443)")
	dataDir := flag.String("datadir", "", "data directory for L4 state store (required for L4)")
	snapshot := flag.String("snapshot", "", "path to IOSWSNAP file for bootstrap (L4 only)")
	flag.Parse()

	// Also check env vars as fallback
	if *apiKey == "" {
		*apiKey = os.Getenv("IOSWARM_API_KEY")
	}
	if *agentID == "" {
		*agentID = os.Getenv("IOSWARM_AGENT_ID")
	}
	if *wallet == "" {
		*wallet = os.Getenv("IOSWARM_WALLET")
	}
	if *coordinator == "127.0.0.1:14689" {
		if env := os.Getenv("IOSWARM_COORDINATOR"); env != "" {
			*coordinator = env
		}
	}

	if *dataDir == "" {
		*dataDir = os.Getenv("IOSWARM_DATADIR")
	}

	if *agentID == "" {
		fmt.Fprintf(os.Stderr, "error: --agent-id is required\n")
		os.Exit(1)
	}

	if strings.ToUpper(*level) == "L4" && *dataDir == "" {
		fmt.Fprintf(os.Stderr, "error: --datadir is required for L4 mode\n")
		os.Exit(1)
	}

	logger, _ := zap.NewProduction()
	defer logger.Sync()

	logger.Info("starting ioswarm-agent",
		zap.String("coordinator", *coordinator),
		zap.String("agent_id", *agentID),
		zap.String("level", *level),
		zap.String("region", *region),
		zap.Bool("auth", *apiKey != ""))

	// Auto-enable TLS when connecting to port 443
	if !*useTLS && *tlsCert == "" && strings.HasSuffix(*coordinator, ":443") {
		*useTLS = true
	}

	conn, err := dialCoordinator(*coordinator, *agentID, *apiKey, *tlsCert, *useTLS)
	if err != nil {
		logger.Fatal("failed to connect to coordinator", zap.Error(err))
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("shutting down...")
		cancel()
	}()

	// Register with coordinator
	resp, err := register(ctx, conn, *agentID, *level, *region, *wallet, logger)
	if err != nil {
		logger.Fatal("registration failed", zap.Error(err))
	}

	// Start heartbeat loop in background
	hbInterval := time.Duration(resp.HeartbeatIntervalSec) * time.Second
	if hbInterval < time.Second {
		hbInterval = 10 * time.Second
	}
	go heartbeatLoop(ctx, conn, *agentID, hbInterval, logger)

	// L4: initialize state store and sync
	var stateStore *StateStore
	if strings.ToUpper(*level) == "L4" {
		dbPath := *dataDir + "/state.db"
		var err error
		stateStore, err = OpenStateStore(dbPath, logger)
		if err != nil {
			logger.Fatal("failed to open state store", zap.Error(err))
		}
		defer stateStore.Close()

		// Load snapshot if provided and store is empty
		if *snapshot != "" && stateStore.Height() == 0 {
			h, n, err := LoadSnapshot(*snapshot, stateStore, logger)
			if err != nil {
				logger.Fatal("failed to load snapshot", zap.Error(err))
			}
			logger.Info("loaded snapshot",
				zap.Uint64("height", h),
				zap.Int("entries", n))
		}

		sync := NewStateSync(stateStore, conn, *agentID, logger)
		sync.Start(ctx)
		defer sync.Stop()

		// Wait for first diff before processing tasks (with 60s timeout)
		logger.Info("waiting for state sync to become ready...")
		readyCtx, readyCancel := context.WithTimeout(ctx, 60*time.Second)
		if err := sync.WaitReady(readyCtx); err != nil {
			readyCancel()
			logger.Fatal("state sync did not become ready", zap.Error(err))
		}
		readyCancel()
		logger.Info("state sync ready, starting task processing",
			zap.Uint64("height", stateStore.Height()))

		// Store reference for L4 validator (atomic store before streamTasks)
		activeStateStore.Store(stateStore)
	}

	// Stream and process tasks
	streamTasks(ctx, conn, *agentID, *level, *region, *wallet, logger)
}

func register(ctx context.Context, conn *grpc.ClientConn, agentID, level, region, wallet string, logger *zap.Logger) (*registerResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req := &registerRequest{
		AgentID:       agentID,
		Capability:    parseLevel(level),
		Region:        region,
		Version:       "0.2.0",
		WalletAddress: wallet,
	}
	resp := &registerResponse{}

	if err := conn.Invoke(ctx, methodRegister, req, resp); err != nil {
		return nil, fmt.Errorf("register RPC: %w", err)
	}
	if !resp.Accepted {
		return nil, fmt.Errorf("rejected: %s", resp.Reason)
	}

	logger.Info("registered with coordinator",
		zap.Uint32("heartbeat_interval", resp.HeartbeatIntervalSec))
	return resp, nil
}

func heartbeatLoop(ctx context.Context, conn *grpc.ClientConn, agentID string, interval time.Duration, logger *zap.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sendHeartbeat(ctx, conn, agentID, logger)
		}
	}
}

func sendHeartbeat(ctx context.Context, conn *grpc.ClientConn, agentID string, logger *zap.Logger) {
	hbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req := &heartbeatRequest{
		AgentID:        agentID,
		TasksProcessed: tasksProcessed.Load(),
	}
	resp := &heartbeatResponse{}
	if err := conn.Invoke(hbCtx, methodHeartbeat, req, resp); err != nil {
		logger.Warn("heartbeat failed", zap.Error(err))
	} else if !resp.Alive {
		logger.Warn("coordinator says not alive", zap.String("directive", resp.Directive))
	} else if resp.Payout != nil {
		logger.Info("payout received",
			zap.Uint64("epoch", resp.Payout.Epoch),
			zap.Float64("amount_iotx", resp.Payout.AmountIOTX))
	}
}

func streamTasks(ctx context.Context, conn *grpc.ClientConn, agentID, level, region, wallet string, logger *zap.Logger) {
	firstRun := true
	for {
		if ctx.Err() != nil {
			return
		}

		// Re-register before opening stream (handles eviction recovery).
		// Skip on first iteration since main() already registered.
		if !firstRun {
			if _, err := register(ctx, conn, agentID, level, region, wallet, logger); err != nil {
				logger.Warn("re-register failed, retrying", zap.Error(err))
				time.Sleep(5 * time.Second)
				continue
			}
		}
		firstRun = false

		logger.Info("opening task stream")

		streamDesc := &grpc.StreamDesc{StreamName: "GetTasks", ServerStreams: true}
		stream, err := conn.NewStream(ctx, streamDesc, methodGetTasks)
		if err != nil {
			logger.Error("failed to open stream", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}

		req := &getTasksRequest{
			AgentID:      agentID,
			MaxLevel:     parseLevel(level),
			MaxBatchSize: 10,
		}
		if err := stream.SendMsg(req); err != nil {
			logger.Error("failed to send GetTasksRequest", zap.Error(err))
			time.Sleep(5 * time.Second)
			continue
		}
		stream.CloseSend()

		for {
			batch := &taskBatch{}
			if err := stream.RecvMsg(batch); err != nil {
				logger.Warn("stream ended", zap.Error(err))
				break
			}

			logger.Info("received batch",
				zap.String("batch_id", batch.BatchID),
				zap.Int("tasks", len(batch.Tasks)))

			results := processBatch(batch, level)
			submitResults(ctx, conn, agentID, batch.BatchID, results, logger)
		}

		// Backoff before reconnecting
		time.Sleep(2 * time.Second)
	}
}

func processBatch(batch *taskBatch, level string) []*taskResult {
	results := make([]*taskResult, 0, len(batch.Tasks))
	for _, task := range batch.Tasks {
		r := validateTask(task, level)
		from := ""
		to := ""
		if task.Sender != nil {
			from = short(task.Sender.Address)
		}
		if task.Receiver != nil {
			to = short(task.Receiver.Address)
		}
		fmt.Printf("[%s] task=%d from=%s to=%s valid=%v reject=%s gasEst=%d gasUsed=%d latency=%dµs err=%s\n",
			batch.BatchID, task.TaskID, from, to,
			r.Valid, r.RejectReason, r.GasEstimate, r.GasUsed, r.LatencyUs, r.ExecError)
		if len(r.StateChanges) > 0 {
			fmt.Printf("  stateChanges=%d logs=%d\n", len(r.StateChanges), len(r.Logs))
		}
		results = append(results, r)
	}
	tasksProcessed.Add(uint32(len(results)))
	return results
}

func short(s string) string {
	if len(s) > 12 {
		return s[:6] + ".." + s[len(s)-4:]
	}
	return s
}

func submitResults(ctx context.Context, conn *grpc.ClientConn, agentID, batchID string, results []*taskResult, logger *zap.Logger) {
	submitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req := &batchResult{
		AgentID:   agentID,
		BatchID:   batchID,
		Results:   results,
		Timestamp: uint64(time.Now().UnixMilli()),
	}
	resp := &submitResponse{}

	if err := conn.Invoke(submitCtx, methodSubmitResults, req, resp); err != nil {
		logger.Error("submit failed", zap.Error(err))
		return
	}
	if !resp.Accepted {
		logger.Warn("results rejected", zap.String("reason", resp.Reason))
	}
}

func parseLevel(s string) int32 {
	switch strings.ToUpper(s) {
	case "L1":
		return 0
	case "L3":
		return 2
	case "L4":
		return 3
	default:
		return 1
	}
}
