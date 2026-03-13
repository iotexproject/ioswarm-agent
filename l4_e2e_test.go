//go:build e2e

package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestL4E2E is a full end-to-end test for the L4 agent pipeline:
//
//  1. Mock coordinator gRPC server serves Register, GetTasks, SubmitResults, Heartbeat, StreamStateDiffs
//  2. Agent connects, registers, starts state sync, receives diffs
//  3. Verify: BoltDB height tracks tip, entries persisted, L4 validation works
//
// Run: go test -v -run TestL4E2E -timeout 60s -count=1
func TestL4E2E(t *testing.T) {
	// 1. Start mock coordinator
	coord := newMockCoordinator(t)
	port := coord.start(t)
	defer coord.stop()

	t.Logf("mock coordinator listening on port %d", port)

	// 2. Open BoltDB state store
	dbPath := filepath.Join(t.TempDir(), "state.db")
	logger, _ := zap.NewDevelopment()

	store, err := OpenStateStore(dbPath, logger)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer store.Close()

	if store.Height() != 0 {
		t.Fatalf("expected initial height 0, got %d", store.Height())
	}

	// 3. Connect to coordinator
	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 4. Register
	agentID := "test-l4-e2e"
	resp, err := register(ctx, conn, agentID, "L4", "test", "", logger)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("registration rejected: %s", resp.Reason)
	}
	t.Logf("registered: heartbeat_interval=%ds", resp.HeartbeatIntervalSec)

	// 5. Start state sync
	ss := NewStateSync(store, conn, agentID, logger)
	ss.Start(ctx)
	defer ss.Stop()

	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := ss.WaitReady(readyCtx); err != nil {
		t.Fatalf("state sync did not become ready: %v", err)
	}

	activeStateStore.Store(store)

	// 6. Wait for all 50 diffs
	deadline := time.After(10 * time.Second)
	for store.Height() < 50 {
		select {
		case <-deadline:
			t.Fatalf("timed out at height %d, want 50", store.Height())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Logf("state synced to height %d", store.Height())

	// 7. Verify BoltDB contents
	stats := store.Stats()
	t.Logf("store stats: %+v", stats)
	if stats[nsAccount] == 0 {
		t.Error("expected Account entries, got 0")
	}

	val, _ := store.Get(nsAccount, []byte("sender-addr-001"))
	if val == nil {
		t.Error("expected sender-addr-001 in store")
	}

	// 8. Verify persistence (reopen)
	h := store.Height()
	store.Close()

	store2, err := OpenStateStore(dbPath, logger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	if store2.Height() != h {
		t.Errorf("persistence: want %d, got %d", h, store2.Height())
	}
	t.Logf("persistence verified: height=%d", h)

	// 9. Heartbeat
	sendHeartbeat(ctx, conn, agentID, logger)
	t.Log("heartbeat OK")

	// 10. Task stream + L4 validation
	activeStateStore.Store(store2)
	streamDesc := &grpc.StreamDesc{StreamName: "GetTasks", ServerStreams: true}
	stream, err := conn.NewStream(ctx, streamDesc, methodGetTasks)
	if err != nil {
		t.Fatalf("open task stream: %v", err)
	}
	if err := stream.SendMsg(&getTasksRequest{AgentID: agentID, MaxLevel: 3, MaxBatchSize: 10}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stream.CloseSend()

	batch := &taskBatch{}
	if err := stream.RecvMsg(batch); err != nil {
		t.Fatalf("recv batch: %v", err)
	}
	t.Logf("received batch %s: %d tasks", batch.BatchID, len(batch.Tasks))

	results := processBatch(batch, "L4")
	for _, r := range results {
		t.Logf("  task=%d valid=%v reject=%q note=%q gas=%d",
			r.TaskID, r.Valid, r.RejectReason, r.Note, r.GasEstimate)
		if r.Note == "" {
			t.Errorf("task %d: missing L4-stateful note", r.TaskID)
		}
	}

	submitResults(ctx, conn, agentID, batch.BatchID, results, logger)
	if coord.resultsReceived.Load() == 0 {
		t.Error("coordinator received 0 results")
	}

	t.Logf("=== L4 E2E PASSED: height=%d accounts=%d tasks=%d results=%d ===",
		store2.Height(), stats[nsAccount], len(results), coord.resultsReceived.Load())
}

// TestStateStoreRestart verifies BoltDB persistence across close/reopen.
func TestStateStoreRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	logger, _ := zap.NewDevelopment()

	store, err := OpenStateStore(dbPath, logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	for h := uint64(1); h <= 100; h++ {
		err := store.ApplyDiff(h, []*stateDiffEntry{
			{WriteType: WriteTypePut, Namespace: nsAccount, Key: []byte(fmt.Sprintf("addr-%d", h)), Value: []byte("bal")},
		})
		if err != nil {
			t.Fatalf("apply h=%d: %v", h, err)
		}
	}
	if store.Height() != 100 {
		t.Fatalf("want 100, got %d", store.Height())
	}
	store.Close()

	store2, err := OpenStateStore(dbPath, logger)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()

	if store2.Height() != 100 {
		t.Fatalf("persisted height: want 100, got %d", store2.Height())
	}
	for h := uint64(1); h <= 100; h++ {
		val, _ := store2.Get(nsAccount, []byte(fmt.Sprintf("addr-%d", h)))
		if val == nil {
			t.Fatalf("addr-%d missing after restart", h)
		}
	}
	t.Log("restart test passed: 100 blocks persisted and recovered")
}

// TestStateStoreDelete verifies delete operations.
func TestStateStoreDelete(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	logger, _ := zap.NewDevelopment()

	store, err := OpenStateStore(dbPath, logger)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	store.ApplyDiff(1, []*stateDiffEntry{
		{WriteType: WriteTypePut, Namespace: nsAccount, Key: []byte("to-delete"), Value: []byte("exists")},
	})
	val, _ := store.Get(nsAccount, []byte("to-delete"))
	if val == nil {
		t.Fatal("should exist after put")
	}

	store.ApplyDiff(2, []*stateDiffEntry{
		{WriteType: WriteTypeDelete, Namespace: nsAccount, Key: []byte("to-delete")},
	})
	val, _ = store.Get(nsAccount, []byte("to-delete"))
	if val != nil {
		t.Fatal("should be nil after delete")
	}
	if store.Height() != 2 {
		t.Fatalf("want height 2, got %d", store.Height())
	}
}

// TestL4E2EBinary builds and runs the actual ioswarm-agent binary.
//
// Run: go test -v -run TestL4E2EBinary -timeout 90s -count=1
func TestL4E2EBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary E2E in short mode")
	}

	// 1. Build binary
	binPath := filepath.Join(t.TempDir(), "ioswarm-agent")
	t.Log("building ioswarm-agent binary...")
	out, err := exec.CommandContext(
		context.Background(), "go", "build", "-o", binPath, ".",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Log("build OK")

	// 2. Start mock coordinator with registration tracking
	var (
		registered      atomic.Bool
		registeredMu    sync.Mutex
		registeredAgent string
	)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := lis.Addr().(*net.TCPAddr).Port

	srv := grpc.NewServer()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "ioswarm.IOSwarm",
		HandlerType: (*interface{})(nil),
		Methods: []grpc.MethodDesc{
			{
				MethodName: "Register",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
					req := &registerRequest{}
					if err := dec(req); err != nil {
						return nil, err
					}
					registeredMu.Lock()
					registeredAgent = req.AgentID
					registeredMu.Unlock()
					registered.Store(true)
					return &registerResponse{Accepted: true, HeartbeatIntervalSec: 60}, nil
				},
			},
			{
				MethodName: "SubmitResults",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
					req := &batchResult{}
					dec(req)
					return &submitResponse{Accepted: true}, nil
				},
			},
			{
				MethodName: "Heartbeat",
				Handler: func(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
					req := &heartbeatRequest{}
					dec(req)
					return &heartbeatResponse{Alive: true, Directive: "continue"}, nil
				},
			},
		},
		Streams: []grpc.StreamDesc{
			{
				StreamName:    "GetTasks",
				ServerStreams:  true,
				Handler: func(srv interface{}, stream grpc.ServerStream) error {
					req := &getTasksRequest{}
					stream.RecvMsg(req)
					<-stream.Context().Done()
					return nil
				},
			},
			{
				StreamName:    "StreamStateDiffs",
				ServerStreams:  true,
				Handler: func(srv interface{}, stream grpc.ServerStream) error {
					req := &streamStateDiffsRequest{}
					if err := stream.RecvMsg(req); err != nil {
						return err
					}
					from := req.FromHeight
					if from == 0 {
						from = 1
					}
					for h := from; h <= 50; h++ {
						diff := &stateDiffResponse{
							Height: h,
							Entries: []*stateDiffEntry{
								{WriteType: WriteTypePut, Namespace: nsAccount, Key: []byte(fmt.Sprintf("addr-%d", h)), Value: []byte("bal")},
							},
						}
						if err := stream.SendMsg(diff); err != nil {
							return err
						}
					}
					<-stream.Context().Done()
					return nil
				},
			},
		},
	}, &struct{}{})

	go srv.Serve(lis)
	defer srv.GracefulStop()

	t.Logf("mock coordinator on port %d", port)

	// 3. Start agent binary
	dataDir := t.TempDir()
	agentCtx, agentCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer agentCancel()

	agentCmd := exec.CommandContext(agentCtx,
		binPath,
		"--coordinator", fmt.Sprintf("127.0.0.1:%d", port),
		"--agent-id", "binary-test-l4",
		"--level", "L4",
		"--datadir", dataDir,
	)
	agentCmd.Stdout = os.Stdout
	agentCmd.Stderr = os.Stderr

	if err := agentCmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}

	// 4. Wait for registration
	deadline := time.After(15 * time.Second)
	for !registered.Load() {
		select {
		case <-deadline:
			agentCmd.Process.Kill()
			t.Fatal("agent did not register within 15s")
		case <-time.After(200 * time.Millisecond):
		}
	}
	registeredMu.Lock()
	t.Logf("agent registered: %s", registeredAgent)
	registeredMu.Unlock()

	// 5. Wait for state sync
	time.Sleep(5 * time.Second)

	// Kill agent gracefully
	agentCmd.Process.Signal(os.Interrupt)
	agentCmd.Wait()

	// 6. Verify BoltDB the agent wrote
	dbPath := filepath.Join(dataDir, "state.db")
	logger, _ := zap.NewDevelopment()
	store, err := OpenStateStore(dbPath, logger)
	if err != nil {
		t.Fatalf("open agent's store: %v", err)
	}
	defer store.Close()

	finalHeight := store.Height()
	t.Logf("agent BoltDB height: %d", finalHeight)

	if finalHeight == 0 {
		t.Fatal("height=0, state sync failed")
	}
	if finalHeight < 10 {
		t.Errorf("want >=10, got %d", finalHeight)
	}

	stats := store.Stats()
	t.Logf("=== BINARY E2E PASSED: height=%d accounts=%d ===", finalHeight, stats[nsAccount])
}

// --- Mock coordinator ---

type mockCoordinator struct {
	grpcServer      *grpc.Server
	lis             net.Listener
	resultsReceived atomic.Int32
}

func newMockCoordinator(t *testing.T) *mockCoordinator {
	return &mockCoordinator{}
}

func (mc *mockCoordinator) start(t *testing.T) int {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mc.lis = lis
	port := lis.Addr().(*net.TCPAddr).Port

	mc.grpcServer = grpc.NewServer()
	mc.grpcServer.RegisterService(&grpc.ServiceDesc{
		ServiceName: "ioswarm.IOSwarm",
		HandlerType: (*interface{})(nil),
		Methods: []grpc.MethodDesc{
			{MethodName: "Register", Handler: mc.handleRegister},
			{MethodName: "SubmitResults", Handler: mc.handleSubmitResults},
			{MethodName: "Heartbeat", Handler: mc.handleHeartbeat},
		},
		Streams: []grpc.StreamDesc{
			{StreamName: "GetTasks", Handler: mc.handleGetTasks, ServerStreams: true},
			{StreamName: "StreamStateDiffs", Handler: mc.handleStreamStateDiffs, ServerStreams: true},
		},
	}, &struct{}{})

	go mc.grpcServer.Serve(lis)
	return port
}

func (mc *mockCoordinator) stop() {
	if mc.grpcServer != nil {
		mc.grpcServer.GracefulStop()
	}
}

func (mc *mockCoordinator) handleRegister(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
	req := &registerRequest{}
	if err := dec(req); err != nil {
		return nil, err
	}
	return &registerResponse{Accepted: true, HeartbeatIntervalSec: 10}, nil
}

func (mc *mockCoordinator) handleSubmitResults(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
	req := &batchResult{}
	if err := dec(req); err != nil {
		return nil, err
	}
	mc.resultsReceived.Add(int32(len(req.Results)))
	return &submitResponse{Accepted: true}, nil
}

func (mc *mockCoordinator) handleHeartbeat(srv interface{}, ctx context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
	req := &heartbeatRequest{}
	dec(req)
	return &heartbeatResponse{Alive: true, Directive: "continue"}, nil
}

func (mc *mockCoordinator) handleGetTasks(srv interface{}, stream grpc.ServerStream) error {
	req := &getTasksRequest{}
	if err := stream.RecvMsg(req); err != nil {
		return err
	}

	// Realistic task with valid signature
	txRaw := make([]byte, 73)
	binary.BigEndian.PutUint64(txRaw[:8], 0) // nonce=0
	txRaw[8] = 0x01                           // r non-zero
	txRaw[40] = 0x01                          // s non-zero
	txRaw[72] = 0x1b                          // v=27

	// Use 0x addresses that match mock state diff entries so L4 local lookup works
	return stream.SendMsg(&taskBatch{
		BatchID:   "e2e-batch-001",
		Timestamp: uint64(time.Now().UnixMilli()),
		Tasks: []*taskPackage{
			{
				TaskID: 1, TxRaw: txRaw, Level: 3, BlockHeight: 50,
				Sender:   &accountSnapshot{Address: "0xAA00000000000000000000000000000000000001", Balance: "1000000000000000000", Nonce: 0},
				Receiver: &accountSnapshot{Address: "0xBB00000000000000000000000000000000000001", Balance: "500000000000000000", Nonce: 0},
			},
			{
				TaskID: 2, TxRaw: txRaw, Level: 3, BlockHeight: 50,
				Sender:   &accountSnapshot{Address: "0xAA00000000000000000000000000000000000002", Balance: "2000000000000000000", Nonce: 0},
				Receiver: nil, // contract deploy
			},
		},
	})
}

func (mc *mockCoordinator) handleStreamStateDiffs(srv interface{}, stream grpc.ServerStream) error {
	req := &streamStateDiffsRequest{}
	if err := stream.RecvMsg(req); err != nil {
		return err
	}

	from := req.FromHeight
	if from == 0 {
		from = 1
	}

	// 20-byte address keys matching the 0x addresses used in handleGetTasks
	senderKey1 := make([]byte, 20)
	senderKey1[0] = 0xAA
	senderKey1[19] = 0x01 // matches 0xAA00...0001

	senderKey2 := make([]byte, 20)
	senderKey2[0] = 0xAA
	senderKey2[19] = 0x02 // matches 0xAA00...0002

	receiverKey := make([]byte, 20)
	receiverKey[0] = 0xBB
	receiverKey[19] = 0x01 // matches 0xBB00...0001

	// Also keep legacy keys for backward compatibility with older test assertions
	legacySenderKey := []byte("sender-addr-001")

	for h := from; h <= 50; h++ {
		// Protobuf-encoded accounts for the new L4 local lookup path
		senderAcct := encodeTestAccount(h, fmt.Sprintf("%d", 1000000-h*100), nil, nil)
		receiverAcct := encodeTestAccount(0, fmt.Sprintf("%d", h*100), nil, nil)

		if err := stream.SendMsg(&stateDiffResponse{
			Height: h,
			Entries: []*stateDiffEntry{
				{WriteType: WriteTypePut, Namespace: nsAccount, Key: senderKey1, Value: senderAcct},
				{WriteType: WriteTypePut, Namespace: nsAccount, Key: senderKey2, Value: senderAcct},
				{WriteType: WriteTypePut, Namespace: nsAccount, Key: receiverKey, Value: receiverAcct},
				{WriteType: WriteTypePut, Namespace: nsAccount, Key: legacySenderKey, Value: []byte("bal")},
			},
			DigestBytes: []byte(fmt.Sprintf("digest-h%d", h)),
		}); err != nil {
			return err
		}
	}

	<-stream.Context().Done()
	return nil
}

// TestSnapshotDiffCatchUp tests the full L4 pipeline (TEST_PLAN 11.11):
//  1. Load baseline snapshot at height H
//  2. Start StateSync from H+1
//  3. Verify agent catches up and local state is queryable
func TestSnapshotDiffCatchUp(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	dir := t.TempDir()

	// 1. Create a snapshot at height 25 with protobuf-encoded accounts
	senderKey := make([]byte, 20)
	senderKey[0] = 0xAA
	senderKey[19] = 0x01

	receiverKey := make([]byte, 20)
	receiverKey[0] = 0xBB
	receiverKey[19] = 0x01

	snapEntries := []testSnapEntry{
		{ns: nsAccount, key: senderKey, val: encodeTestAccount(25, "997500", nil, nil)},
		{ns: nsAccount, key: receiverKey, val: encodeTestAccount(0, "2500", nil, nil)},
	}
	snapPath := filepath.Join(dir, "baseline.snap")
	writeTestSnapshot(t, snapPath, 25, snapEntries)

	// 2. Open store and load snapshot
	dbPath := filepath.Join(dir, "state.db")
	store, err := OpenStateStore(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	h, n, err := LoadSnapshot(snapPath, store, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot loaded: height=%d entries=%d", h, n)

	if store.Height() != 25 {
		t.Fatalf("store height after snapshot = %d, want 25", store.Height())
	}

	// Verify snapshot accounts
	acct, err := store.GetAccount(senderKey)
	if err != nil {
		t.Fatal(err)
	}
	if acct == nil || acct.Nonce != 25 {
		t.Fatalf("snapshot sender nonce = %v, want 25", acct)
	}

	// 3. Start mock coordinator that serves diffs from height 26-50
	coord := newMockCoordinator(t)
	port := coord.start(t)
	defer coord.stop()

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register first
	_, err = register(ctx, conn, "snap-test", "L4", "test", "", logger)
	if err != nil {
		t.Fatal(err)
	}

	// 4. StateSync catches up from height 26
	ss := NewStateSync(store, conn, "snap-test", logger)
	ss.Start(ctx)
	defer ss.Stop()

	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := ss.WaitReady(readyCtx); err != nil {
		t.Fatalf("sync not ready: %v", err)
	}

	// Wait for all diffs
	deadline := time.After(10 * time.Second)
	for store.Height() < 50 {
		select {
		case <-deadline:
			t.Fatalf("timed out at height %d", store.Height())
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.Logf("synced to height %d (snapshot=25 + 25 diffs)", store.Height())

	// 5. Verify: latest account state reflects diff updates (not just snapshot)
	acct, err = store.GetAccount(senderKey)
	if err != nil {
		t.Fatal(err)
	}
	if acct == nil {
		t.Fatal("sender account not found after sync")
	}
	// At height 50, nonce should be 50 (set by mock diff handler)
	if acct.Nonce != 50 {
		t.Errorf("sender nonce = %d, want 50 (latest diff)", acct.Nonce)
	}

	// 6. L4 validation with local state
	activeStateStore.Store(store)
	txRaw := make([]byte, 73)
	binary.BigEndian.PutUint64(txRaw[:8], 50) // nonce=50 (matches latest)
	txRaw[8] = 0x01
	txRaw[40] = 0x01
	txRaw[72] = 0x1b

	task := &taskPackage{
		TaskID: 99, TxRaw: txRaw, Level: 3, BlockHeight: 50,
		Sender:   &accountSnapshot{Address: "0xAA00000000000000000000000000000000000001", Balance: "1000000", Nonce: 0},
		Receiver: &accountSnapshot{Address: "0xBB00000000000000000000000000000000000001", Balance: "500", Nonce: 0},
	}
	res := validateL4(task)
	t.Logf("L4 result: valid=%v note=%q reject=%q", res.Valid, res.Note, res.RejectReason)

	if !res.Valid {
		t.Errorf("expected valid, got reject=%q", res.RejectReason)
	}
	if res.Note == "" {
		t.Error("missing L4 note")
	}

	t.Logf("=== SNAPSHOT + DIFF CATCH-UP E2E PASSED (test 11.11) ===")
}

// TestL4LocalStateLookup verifies L4 validator uses local BoltDB state for nonce/balance checks.
func TestL4LocalStateLookup(t *testing.T) {
	logger := zap.NewNop()
	dbPath := filepath.Join(t.TempDir(), "state.db")

	store, err := OpenStateStore(dbPath, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Insert sender account: nonce=10, balance=5000000
	senderAddr := "0xAA00000000000000000000000000000000000099"
	senderKey, _ := addressToHash160(senderAddr)
	acctData := encodeTestAccount(10, "5000000", nil, nil)
	store.ApplyDiff(100, []*stateDiffEntry{
		{WriteType: WriteTypePut, Namespace: nsAccount, Key: senderKey, Value: acctData},
	})
	activeStateStore.Store(store)

	t.Run("valid_tx_local_state", func(t *testing.T) {
		txRaw := make([]byte, 73)
		binary.BigEndian.PutUint64(txRaw[:8], 10) // nonce=10
		txRaw[8] = 0x01
		txRaw[40] = 0x01
		txRaw[72] = 0x1b

		task := &taskPackage{
			TaskID: 1, TxRaw: txRaw, Level: 3, BlockHeight: 100,
			Sender:   &accountSnapshot{Address: senderAddr, Balance: "999", Nonce: 0},
			Receiver: &accountSnapshot{Address: "0xBB00000000000000000000000000000000000001", Balance: "100", Nonce: 0},
		}
		res := validateL4(task)
		if !res.Valid {
			t.Errorf("expected valid, got reject=%q", res.RejectReason)
		}
		if res.Note == "" || !contains(res.Note, "local") {
			t.Errorf("expected 'local' in note, got %q", res.Note)
		}
		t.Logf("valid tx: note=%q", res.Note)
	})

	t.Run("nonce_too_low_local_state", func(t *testing.T) {
		txRaw := make([]byte, 73)
		binary.BigEndian.PutUint64(txRaw[:8], 5) // nonce=5 < account nonce=10
		txRaw[8] = 0x01
		txRaw[40] = 0x01
		txRaw[72] = 0x1b

		task := &taskPackage{
			TaskID: 2, TxRaw: txRaw, Level: 3, BlockHeight: 100,
			Sender:   &accountSnapshot{Address: senderAddr, Balance: "999", Nonce: 0},
			Receiver: nil,
		}
		res := validateL4(task)
		if res.Valid {
			t.Error("expected rejection for low nonce")
		}
		if !contains(res.RejectReason, "nonce too low") {
			t.Errorf("unexpected reject: %q", res.RejectReason)
		}
		if !contains(res.RejectReason, "L4-local") {
			t.Errorf("expected 'L4-local' in reject, got %q", res.RejectReason)
		}
		t.Logf("nonce reject: %q", res.RejectReason)
	})

	t.Run("zero_balance_local_state", func(t *testing.T) {
		// Insert zero-balance account
		zeroAddr := "0xCC00000000000000000000000000000000000001"
		zeroKey, _ := addressToHash160(zeroAddr)
		zeroAcct := encodeTestAccount(0, "0", nil, nil)
		store.ApplyDiff(101, []*stateDiffEntry{
			{WriteType: WriteTypePut, Namespace: nsAccount, Key: zeroKey, Value: zeroAcct},
		})

		txRaw := make([]byte, 73)
		binary.BigEndian.PutUint64(txRaw[:8], 0)
		txRaw[8] = 0x01
		txRaw[40] = 0x01
		txRaw[72] = 0x1b

		task := &taskPackage{
			TaskID: 3, TxRaw: txRaw, Level: 3, BlockHeight: 101,
			Sender:   &accountSnapshot{Address: zeroAddr, Balance: "999", Nonce: 0},
			Receiver: nil,
		}
		res := validateL4(task)
		if res.Valid {
			t.Error("expected rejection for zero balance")
		}
		if !contains(res.RejectReason, "zero balance") {
			t.Errorf("unexpected reject: %q", res.RejectReason)
		}
		t.Logf("zero balance reject: %q", res.RejectReason)
	})

	t.Run("fallback_to_coordinator_state", func(t *testing.T) {
		// Use io1 address that addressToHash160 doesn't support → falls back to coordinator
		txRaw := make([]byte, 73)
		binary.BigEndian.PutUint64(txRaw[:8], 0)
		txRaw[8] = 0x01
		txRaw[40] = 0x01
		txRaw[72] = 0x1b

		task := &taskPackage{
			TaskID: 4, TxRaw: txRaw, Level: 3, BlockHeight: 100,
			Sender:   &accountSnapshot{Address: "io1unknown", Balance: "1000000", Nonce: 0},
			Receiver: nil,
		}
		res := validateL4(task)
		if !res.Valid {
			t.Errorf("expected valid (coordinator fallback), got reject=%q", res.RejectReason)
		}
		if contains(res.Note, "local") {
			t.Errorf("should use coord fallback, got note=%q", res.Note)
		}
		t.Logf("fallback: note=%q", res.Note)
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstr(s, substr)
}

func searchSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
