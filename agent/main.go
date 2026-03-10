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

func main() {
	coordinator := flag.String("coordinator", "127.0.0.1:14689", "coordinator gRPC address")
	apiKey := flag.String("api-key", "", "HMAC API key (iosw_...)")
	agentID := flag.String("agent-id", "", "agent ID (extracted from api-key context, or set manually)")
	level := flag.String("level", "L2", "task level: L1, L2, L3")
	region := flag.String("region", "default", "region label")
	wallet := flag.String("wallet", "", "IOTX wallet address for rewards")
	tlsCert := flag.String("tls-cert", "", "path to TLS certificate (optional)")
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

	if *agentID == "" {
		fmt.Fprintf(os.Stderr, "error: --agent-id is required\n")
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

	conn, err := dialCoordinator(*coordinator, *agentID, *apiKey, *tlsCert)
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
	if err := register(ctx, conn, *agentID, *level, *region, *wallet, logger); err != nil {
		logger.Fatal("registration failed", zap.Error(err))
	}

	// Start heartbeat loop in background
	go heartbeatLoop(ctx, conn, *agentID, logger)

	// Stream and process tasks
	streamTasks(ctx, conn, *agentID, *level, logger)
}

func register(ctx context.Context, conn *grpc.ClientConn, agentID, level, region, wallet string, logger *zap.Logger) error {
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
		return fmt.Errorf("register RPC: %w", err)
	}
	if !resp.Accepted {
		return fmt.Errorf("rejected: %s", resp.Reason)
	}

	logger.Info("registered with coordinator",
		zap.Uint32("heartbeat_interval", resp.HeartbeatIntervalSec))
	return nil
}

func heartbeatLoop(ctx context.Context, conn *grpc.ClientConn, agentID string, logger *zap.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
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
			cancel()
		}
	}
}

func streamTasks(ctx context.Context, conn *grpc.ClientConn, agentID, level string, logger *zap.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}

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
		results = append(results, validateTask(task, level))
	}
	tasksProcessed.Add(uint32(len(results)))
	return results
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
	default:
		return 1
	}
}
