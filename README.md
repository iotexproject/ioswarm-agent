# ioswarm-agent

IOSwarm agent node for the IoTeX network. Connects to a delegate's coordinator, receives pending transactions, validates them at configurable levels (L1-L4), and earns IOTX rewards.

**Production status** (March 2026): L4 agents running on IoTeX mainnet with **100% shadow accuracy**, on-chain reward settlement verified end-to-end.

## Architecture

```
┌──────────────────────────┐         ┌───────────────────────────────┐
│   IoTeX Delegate Node    │  gRPC   │        ioswarm-agent          │
│   (iotex-core + IOSwarm) │◄───────►│                               │
│                          │         │  1. Register with coordinator │
│  Coordinator:            │         │  2. Stream task batches       │
│  • dispatches tx batches │         │  3. Validate L1/L2/L3/L4     │
│  • tracks agent work     │         │  4. Submit results            │
│  • distributes rewards   │         │  5. Sync state diffs (L4)     │
│  • streams state diffs   │         │  6. Receive payout via HB     │
└──────────┬───────────────┘         │  7. Claim rewards on-chain    │
           │                         └───────────────────────────────┘
           │ depositAndSettle()
           ▼
┌──────────────────────────┐         ┌───────────────────────────────┐
│  AgentRewardPool Contract│         │  Snapshot Server              │
│  (IoTeX mainnet)         │         │  https://ts.iotex.me          │
│  • F1 cumulative rewards │         │  • acctcode.snap.gz (209 MB)  │
│  • claim() by agents     │         │  • baseline.snap.gz (1.4 GB)  │
└──────────────────────────┘         │  • Updated daily              │
                                     └───────────────────────────────┘
```

## Supported Delegates

| Delegate | Coordinator | Snapshot | Status |
|----------|------------|----------|--------|
| **goodwell** | `swarm.iotex.me:14689` | [ts.iotex.me](https://ts.iotex.me) | Active |

Want to add your delegate? See the [Coordinator README](https://github.com/iotexproject/iotex-core/tree/ioswarm-v2.3.5/ioswarm) for setup instructions.

## Quick Start

### Prerequisites

- Docker **or** Go 1.23+
- ~2 GB RAM, ~2 GB disk

### 1. Download the state snapshot

```bash
curl -O https://ts.iotex.me/acctcode.snap.gz    # ~209 MB, one-time download
```

This is a compressed copy of the IoTeX mainnet state. You only need it for the first boot — after that the agent syncs incrementally.

> **Snapshot server:** https://ts.iotex.me — updated daily, served via Cloudflare CDN.

### 2. Get your credentials

Contact the delegate operator to get:
- **Coordinator address** (e.g., `178.62.196.98:14689`)
- **Agent ID** (e.g., `agent-01`)
- **API key** (`iosw_...` format)

### 3. Start the agent

**Option A: Docker (recommended)**

Works on Linux (amd64), macOS (Apple Silicon & Intel), and Windows (WSL2). No Go toolchain needed.

```bash
docker run -d --name ioswarm-agent --restart=always \
  -v $(pwd)/acctcode.snap.gz:/data/acctcode.snap.gz \
  -v $(pwd)/l4state:/data/l4state \
  raullen/ioswarm-agent:latest \
  --coordinator=<delegate-ip>:14689 \
  --agent-id=<your-id> \
  --api-key=iosw_<your-key> \
  --level=L4 \
  --snapshot=/data/acctcode.snap.gz \
  --datadir=/data/l4state \
  --wallet=<your-iotx-address>
```

Check logs:
```bash
docker logs -f ioswarm-agent
```

**Option B: Build from source (requires Go 1.23+)**

```bash
git clone https://github.com/iotexproject/ioswarm-agent.git
cd ioswarm-agent
go build -o ioswarm-agent .

./ioswarm-agent \
  --coordinator=<delegate-ip>:14689 \
  --agent-id=<your-id> \
  --api-key=iosw_<your-key> \
  --level=L4 \
  --snapshot=./acctcode.snap.gz \
  --datadir=./l4state \
  --wallet=<your-iotx-address>
```

First boot loads the snapshot (~10s). Subsequent restarts recover from local state in <200ms.

### 4. Generate a wallet (optional)

If you need a new wallet for reward payouts:

```bash
# Docker
docker run --rm raullen/ioswarm-agent:latest keygen

# Or from source
./ioswarm-agent keygen -out my-agent.key
```

### 5. Run as a background service (non-Docker)

```bash
nohup ./ioswarm-agent \
  --coordinator=<delegate-ip>:14689 \
  --agent-id=<your-id> \
  --api-key=iosw_<your-key> \
  --level=L4 \
  --snapshot=./acctcode.snap.gz \
  --datadir=./l4state \
  --wallet=<your-iotx-address> > agent.log 2>&1 &
```

Or use `systemd` (Linux) / `launchd` (macOS) for auto-restart.

## CLI Flags

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--coordinator` | `IOSWARM_COORDINATOR` | `127.0.0.1:14689` | Coordinator gRPC address |
| `--agent-id` | `IOSWARM_AGENT_ID` | *(required)* | Unique agent identifier |
| `--api-key` | `IOSWARM_API_KEY` | | HMAC authentication key |
| `--level` | | `L2` | Validation level: `L1`, `L2`, `L3`, `L4` |
| `--snapshot` | | | Path to IOSWSNAP file for L4 bootstrap |
| `--datadir` | `IOSWARM_DATADIR` | `/tmp/ioswarm` | Directory for L4 BoltDB state |
| `--region` | | `default` | Region label for task routing |
| `--wallet` | `IOSWARM_WALLET` | | IOTX wallet address for rewards |
| `--tls-cert` | | | Path to TLS certificate (optional) |

## Validation Levels

| Level | What It Does | State Needed | Accuracy |
|-------|-------------|--------------|----------|
| L1 | Signature verification | None | — |
| L2 | + Nonce/balance checks | Coordinator-provided snapshots | — |
| L3 | + Full EVM execution | Coordinator-provided state (via SimulateAccessList) | 100% |
| **L4** | **Independent EVM execution** | **Local full state via snapshot + diffs** | **100%** |

### L1 — Signature Verification
- Checks transaction raw bytes >= 65 bytes
- Verifies ECDSA signature components (r, s) are non-zero and within secp256k1 curve order

### L2 — State Verification (includes L1)
- Validates sender account has non-zero balance
- Checks transaction nonce >= sender account nonce (replay protection)
- Estimates gas: 21,000 for transfers, 100,000 for contract calls

### L3 — Full EVM Execution (includes L1 + L2)
- Executes the transaction in a local EVM sandbox
- Reports gas used, state changes, logs, and execution errors
- Handles contract creation, calls, and plain transfers
- Uses coordinator-provided state (accounts, code, storage slots prefetched via SimulateAccessList)
- Mainnet result: 230+ transactions, 100% shadow accuracy

### L4 — Fully Independent Validation (includes L1 + L2 + L3)
- Maintains a local copy of the full IoTeX state in BoltDB (~931 MB after sync)
- L2 checks use local account data (nonce/balance) instead of coordinator-provided state
- EVM execution uses local MPT trie for storage slots, contract code, and account state
- Does not depend on coordinator's storage prefetch — independently reads any contract storage via trie traversal
- Cold start: load an IOSWSNAP snapshot file, then catch up via gRPC state diff streaming
- Steady state: real-time state diffs keep BoltDB in sync with the delegate
- Kill/restart recovery: <200ms from BoltDB, no snapshot reload needed

## How It Works

```
First boot:
  1. Load IOSWSNAP snapshot → local BoltDB state store (~10s)
  2. Register with coordinator
  3. Open StreamStateDiffs from snapshot height + 1
  4. Catch up to current block height
  5. Start processing transaction validation tasks

Subsequent boots:
  1. Open existing BoltDB state store (instant)
  2. Register with coordinator
  3. Resume StreamStateDiffs from last synced height
  4. Start processing tasks (<1s to ready)
```

## Snapshot Server

State snapshots for L4 bootstrap are served via CDN at **https://ts.iotex.me**.

| File | Size | Contents | Description |
|------|------|----------|-------------|
| `acctcode.snap.gz` | ~209 MB | Account + Code | Sufficient for L4 validation |
| `baseline.snap.gz` | ~1.4 GB | Account + Code + Contract | Full state including contract storage trie |
| `snapshot-meta.json` | — | Metadata | Height, timestamps, file sizes |

Snapshots are exported daily from the delegate's `trie.db` and updated automatically.

### IOSWSNAP Format

```
header:  magic("IOSWSNAP") + version(uint32) + height(uint64)
entries: [marker(0x01) + ns_len(uint8) + ns + key_len(uint32) + key + val_len(uint32) + val]*
end:     marker(0x00)
trailer: count(uint64) + sha256(32 bytes) + end_magic("SNAPEND\0")
```

Gzip-compressed binary with SHA-256 integrity check. The export tool (`l4baseline`) is in the [iotex-core](https://github.com/iotexproject/iotex-core/tree/ioswarm-v2.3.5/ioswarm/cmd/l4baseline) repo.

## Subcommands

### `claim` — Claim Rewards

Check and withdraw accumulated IOTX rewards from the AgentRewardPool contract.

```bash
# Check claimable amount (dry run)
./ioswarm-agent claim \
  --contract=0x96F475F87911615dD710f9cB425Af8ed0e167C89 \
  --private-key=<agent-wallet-private-key> \
  --dry-run

# Execute claim
./ioswarm-agent claim \
  --contract=0x96F475F87911615dD710f9cB425Af8ed0e167C89 \
  --private-key=<agent-wallet-private-key>
```

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--contract` | `IOSWARM_REWARD_CONTRACT` | *(required)* | AgentRewardPool contract address |
| `--private-key` | `IOSWARM_PRIVATE_KEY` | *(required)* | Agent wallet private key (hex) |
| `--rpc` | | `https://babel-api.mainnet.iotex.io` | IoTeX RPC endpoint |
| `--chain-id` | | `4689` | Chain ID (4689=mainnet, 4690=testnet) |
| `--dry-run` | | `false` | Only show claimable amount |

### `deploy` — Deploy Reward Contract

Deploy a new AgentRewardPool contract to IoTeX.

```bash
./ioswarm-agent deploy \
  --private-key=<deployer-key> \
  --coordinator=0x<coordinator-hot-wallet>
```

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--private-key` | `IOSWARM_PRIVATE_KEY` | *(required)* | Deployer private key |
| `--coordinator` | | *(required)* | Coordinator hot wallet address |
| `--rpc` | | `https://babel-api.mainnet.iotex.io` | IoTeX RPC endpoint |
| `--chain-id` | | `4689` | Chain ID |

After deployment, configure the delegate with the new contract address.

### `fund` — Fund Agent Wallets

Batch-send IOTX to multiple agent wallets (for claim gas fees).

```bash
./ioswarm-agent fund \
  --private-key=<funder-key> \
  --amount=0.1 \
  0xWallet1 0xWallet2 0xWallet3
```

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--private-key` | `IOSWARM_PRIVATE_KEY` | *(required)* | Funder wallet private key |
| `--amount` | | `0.1` | IOTX to send per wallet |
| `--rpc` | | `https://babel-api.mainnet.iotex.io` | IoTeX RPC endpoint |
| `--chain-id` | | `4689` | Chain ID |
| `--dry-run` | | `false` | Show plan without sending |

## Reward System

### How Rewards Work

1. **Epoch cycle** (default 30s): The coordinator tracks how many tasks each agent completes per epoch
2. **Weight calculation**: `weight = tasks_completed × 1000` (with optional accuracy bonus at 99.5%+)
3. **On-chain settlement**: Coordinator calls `depositAndSettle()` on the AgentRewardPool contract, sending `epochReward × (1 - delegateCut)` as IOTX
4. **Cumulative distribution**: The contract uses F1 (cumulative reward-per-weight) algorithm for O(1) proportional distribution
5. **Agent claim**: Agents call `claim()` at any time to withdraw accumulated rewards

### Reward Flow

```
Delegate Operator
│
│  Funds the coordinator hot wallet with IOTX
│  (this is the reward budget for agents)
│
▼
Coordinator Hot Wallet (EOA)
│
│  Every epoch (30s):
│  1. Calculate each agent's weight (tasks × accuracy bonus)
│  2. Call depositAndSettle(agents[], weights[])
│     with msg.value = epochReward × (1 - delegateCut)
│
▼
AgentRewardPool Contract (on-chain)
│
│  1. Receives IOTX from coordinator
│  2. Updates cumulativeRewardPerWeight (F1 algorithm)
│  3. Each agent's pending reward accumulates proportionally
│
▼
Agents call claim() at any time
│
│  Agent-A wallet → receives proportional share
│  Agent-B wallet → receives proportional share
│  ...
│
▼
IOTX in agent wallets ✅
```

**Fund flow summary:** Delegate operator loads IOTX into the coordinator hot wallet → coordinator drips it into the on-chain contract each epoch based on work done → agents withdraw from the contract whenever they want.

The coordinator hot wallet must maintain sufficient IOTX balance to cover epoch rewards + gas fees. If the wallet runs dry, on-chain settlement pauses (agents keep working and accumulating internal credits, which are settled once the wallet is refunded).

### Key Parameters (delegate config)

| Parameter | Description | Example |
|-----------|-------------|---------|
| `epochRewardIOTX` | IOTX reward per epoch | `0.5` |
| `delegateCutPct` | Delegate's percentage cut | `10` |
| `epochBlocks` | Blocks per epoch (x 10s) | `3` (= 30s) |
| `minTasksForReward` | Minimum tasks to qualify | `1` |
| `bonusAccuracyPct` | Accuracy threshold for bonus | `99.5` |
| `bonusMultiplier` | Weight multiplier for bonus | `1.2` |

### AgentRewardPool Contract

| Function | Access | Description |
|----------|--------|-------------|
| `depositAndSettle(address[], uint256[])` | Coordinator only | Deposit IOTX and update agent weights |
| `claim()` | Any agent | Withdraw accumulated rewards |
| `claimable(address)` | View | Check pending reward amount |
| `setCoordinator(address)` | Coordinator only | Transfer coordinator role |

**Mainnet contract**: `0x96F475F87911615dD710f9cB425Af8ed0e167C89`

### E2E Test Results (mainnet, March 2026)

| Test | Result |
|------|--------|
| Single agent payout | PASS — 0.9 IOTX claimed |
| Multi-agent proportional split | PASS — 50/50 split, 0.45 each |
| 10 agents x 5 epochs | PASS — 0.225 IOTX each, all claimed |
| Dynamic join/leave | PASS — contract balance = 0 after all claims |
| MinTasks threshold | PASS — weight=0 gets 0, weight=1 gets 0.45 |
| Delegate cut | PASS — 10% retained, 90% to agents |

## API Key Generation

API keys are HMAC-SHA256 based. The delegate operator generates keys using the master secret:

```
key = "iosw_" + hex(HMAC-SHA256(masterSecret, agentID))
```

Where `masterSecret` is the delegate's configured secret string, and `agentID` is the agent's unique identifier.

## Monitoring

### Agent log

```bash
tail -f agent.log
```

Key log lines:
- `"state store opened"` — BoltDB loaded, shows current height
- `"snapshot loaded"` — IOSWSNAP bootstrap complete
- `"state sync ready"` — caught up to coordinator, processing tasks
- `"received batch"` — processing transactions
- `"payout received"` — earned rewards

### Coordinator API

Check your agent status (requires auth):
```bash
# Swarm status
curl -s "http://<coordinator>:14690/swarm/status"

# Your agent info
curl -s "http://<coordinator>:14690/swarm/agents" \
  -H "X-Ioswarm-Agent-Id: my-agent-id" \
  -H "X-Ioswarm-Token: iosw_<your_key>"

# Shadow accuracy
curl -s "http://<coordinator>:14690/swarm/shadow"

# Leaderboard
curl -s "http://<coordinator>:14690/swarm/leaderboard"
```

## Troubleshooting

### "failed to load snapshot: read value: unexpected EOF"
Snapshot file may be corrupted or incomplete. Re-download from `https://ts.iotex.me/acctcode.snap.gz` and verify the file size matches `snapshot-meta.json`.

### Agent disconnects frequently
Check network stability. The agent auto-reconnects on disconnect. If the coordinator restarts, all agents reconnect automatically.

### State store grows too large
The BoltDB state store grows as it accumulates state diffs. To reset:
```bash
rm -rf ./l4state
# Restart with --snapshot to re-bootstrap
```

### "error: --datadir is required for L4 mode"
L4 mode requires a data directory for the state store. Add `--datadir=./l4state`.

## Project Structure

```
ioswarm-agent/
├── main.go          # Entry point, gRPC client, task streaming
├── validator.go     # L1/L2/L3/L4 transaction validation
├── evm.go           # EVM execution engine (L3/L4)
├── statedb.go       # In-memory state database for EVM (with L4 local store fallback)
├── statestore.go    # BoltDB persistent state store (L4)
├── statesync.go     # gRPC state diff streaming (L4)
├── snapshot.go      # IOSWSNAP snapshot loader (L4)
├── mpt.go           # MPT trie node deserialization and traversal (L4)
├── account.go       # IoTeX account protobuf decoder
├── types.go         # gRPC message types (protobuf-compatible)
├── codec.go         # Custom gRPC codec (raw protobuf)
├── client.go        # gRPC dialer with auth interceptor
├── claim.go         # `claim` subcommand
├── deploy.go        # `deploy` subcommand
├── fund.go          # `fund` subcommand
├── contracts/       # AgentRewardPool Solidity source + ABI
├── Dockerfile       # Multi-stage Docker build
└── scripts/         # Deployment and test scripts
```

## EVM Fork Compatibility

The agent uses an **all-forks-at-genesis** chain config — all Ethereum hardforks (Homestead through Cancun) are activated from block/time 0. This works because the agent only validates transactions at the current block height, where all forks are already active on IoTeX mainnet.

Currently enabled forks: Homestead, EIP-150/155/158, Byzantium, Constantinople, Petersburg, Istanbul, MuirGlacier, Berlin, London, ArrowGlacier, GrayGlacier, **Shanghai** (time 0), **Cancun** (time 0).

Key EVM compatibility notes:
- **`BlockContext.Random`** must be set (even to `common.Hash{}`) — go-ethereum uses `Random != nil` to detect post-merge, which gates `IsShanghai` and `IsCancun` in `ChainConfig.Rules()`. Without it, PUSH0 (0x5f) and other Shanghai/Cancun opcodes are invalid.
- **`BlobBaseFee`** must be set (e.g., `big.NewInt(0)`) for Cancun EIP-7516 BLOBBASEFEE opcode.
- **Address format**: All addresses from the coordinator arrive in 0x hex format (converted from io1 bech32 on the coordinator side). The agent uses `common.HexToAddress()` directly.

When IoTeX activates a new EVM hardfork:
1. Update the go-ethereum dependency to match the delegate's fork version
2. Add the new fork to `iotexChainConfig()` in `evm.go` (one line)
3. Rebuild and redeploy

## Docker Image

Multi-platform image on Docker Hub — supports `linux/amd64` (Linux servers, Windows WSL2) and `linux/arm64` (Apple Silicon Mac mini/MacBook).

```bash
docker pull raullen/ioswarm-agent:latest
```

| Tag | Description |
|-----|-------------|
| `latest` | Latest stable build |
| `v1.0` | First production release |

## Related Repositories

| Repository | Description |
|------------|-------------|
| [`raullen/ioswarm-agent`](https://hub.docker.com/r/raullen/ioswarm-agent) | Docker image (multi-platform: amd64 + arm64) |
| [iotex-core](https://github.com/iotexproject/iotex-core) (branch: `ioswarm-v2.3.5`) | Delegate node with IOSwarm coordinator |
| [ioswarm-portal](https://github.com/iotexproject/ioswarm-portal) | Dashboard and monitoring UI |
| [IIP-58](https://github.com/iotexproject/iips/pull/64) | IOSwarm protocol specification |

## License

Apache 2.0
