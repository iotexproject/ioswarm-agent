# ioswarm-agent

IOSwarm agent node for the IoTeX network. Connects to a delegate's coordinator, receives pending transactions, validates them at configurable levels (L1/L2/L3), and earns IOTX rewards.

## Architecture

```
┌──────────────────────────┐         ┌───────────────────────────────┐
│   IoTeX Delegate Node    │  gRPC   │        ioswarm-agent          │
│   (iotex-core + IOSwarm) │◄───────►│                               │
│                          │         │  1. Register with coordinator │
│  Coordinator:            │         │  2. Stream task batches       │
│  • dispatches tx batches │         │  3. Validate L1/L2/L3        │
│  • tracks agent work     │         │  4. Submit results            │
│  • distributes rewards   │         │  5. Receive payout via HB     │
└──────────┬───────────────┘         │  6. Claim rewards on-chain    │
           │                         └───────────────────────────────┘
           │ depositAndSettle()
           ▼
┌──────────────────────────┐
│  AgentRewardPool Contract│
│  (IoTeX mainnet)         │
│  • F1 cumulative rewards │
│  • claim() by agents     │
└──────────────────────────┘
```

## Quick Start

### Prerequisites

- Go 1.21+
- Access to an IOSwarm-enabled delegate coordinator
- API key (HMAC-based, provided by delegate operator)

### Build

```bash
git clone https://github.com/iotexproject/ioswarm-agent.git
cd ioswarm-agent
go build -o ioswarm-agent .
```

### Run

```bash
./ioswarm-agent \
  --coordinator=<delegate-ip>:14689 \
  --agent-id=my-agent-01 \
  --api-key=iosw_<your-key> \
  --level=L3 \
  --wallet=0x<your-wallet-address>
```

The agent will:
1. Register with the coordinator via gRPC
2. Start heartbeat loop (default 10s interval)
3. Open a server-streaming connection to receive task batches
4. Validate each transaction and submit results
5. Log payout notifications from the coordinator at each epoch

### Docker

```bash
docker build -t ioswarm-agent .
docker run ioswarm-agent \
  --coordinator=<delegate-ip>:14689 \
  --agent-id=my-agent-01 \
  --api-key=iosw_<key> \
  --level=L3 \
  --wallet=0x<wallet>
```

## CLI Flags

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--coordinator` | `IOSWARM_COORDINATOR` | `127.0.0.1:14689` | Coordinator gRPC address |
| `--agent-id` | `IOSWARM_AGENT_ID` | *(required)* | Unique agent identifier |
| `--api-key` | `IOSWARM_API_KEY` | | HMAC authentication key |
| `--level` | | `L2` | Validation level: `L1`, `L2`, `L3`, `L4` |
| `--snapshot` | | | Path to IOSWSNAP file for L4 bootstrap |
| `--datadir` | | `/tmp/ioswarm` | Directory for L4 BoltDB state |
| `--region` | | `default` | Region label for task routing |
| `--wallet` | `IOSWARM_WALLET` | | IOTX wallet address for rewards |
| `--tls-cert` | | | Path to TLS certificate (optional) |

## Validation Levels

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
- Uses coordinator-provided state (accounts, code, storage slots)

### L4 — Fully Independent Validation (includes L1 + L2 + L3)
- Maintains a local copy of the full IoTeX state in BoltDB
- L2 checks use local account data (nonce/balance) instead of coordinator-provided state
- EVM execution falls back to local MPT trie for storage slots, contract code, and account state
- Does not depend on coordinator's storage prefetch — independently reads any contract storage via trie traversal
- Cold start: load an IOSWSNAP snapshot file, then catch up via gRPC state diff streaming
- Steady state: real-time state diffs keep BoltDB in sync with the delegate

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
2. **Weight calculation**: `weight = tasks_completed × 1000` (with optional accuracy bonus)
3. **On-chain settlement**: Coordinator calls `depositAndSettle()` on the AgentRewardPool contract, sending `epochReward × (1 - delegateCut)` as IOTX
4. **Cumulative distribution**: The contract uses F1 (cumulative reward-per-weight) algorithm for O(1) proportional distribution
5. **Agent claim**: Agents call `claim()` at any time to withdraw accumulated rewards

### Reward Flow

```
Epoch timer fires (every 30s)
    → Coordinator calculates agent weights
    → depositAndSettle(agents[], weights[]) + msg.value
    → Contract updates cumulativeRewardPerWeight
    → Agent heartbeat receives payout notification
    → Agent calls claim() when ready → IOTX transferred to wallet
```

### Key Parameters (delegate config)

| Parameter | Description | Example |
|-----------|-------------|---------|
| `epochRewardIOTX` | IOTX reward per epoch | `0.5` |
| `delegateCutPct` | Delegate's percentage cut | `10` |
| `epochBlocks` | Blocks per epoch (× 10s) | `3` (= 30s) |
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

## API Key Generation

API keys are HMAC-SHA256 based. The delegate operator generates keys using the master secret:

```
key = "iosw_" + hex(HMAC-SHA256(masterSecret, agentID))
```

Where `masterSecret` is the delegate's configured secret string, and `agentID` is the agent's unique identifier.

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
├── Dockerfile       # Multi-stage Docker build
└── scripts/         # Deployment and test scripts
```

## EVM Fork Compatibility

The agent uses an **all-forks-at-block-0** chain config — all Ethereum hardforks (Homestead through London) are activated from genesis. This works because the agent only validates transactions at the current block height, where all forks are already active on mainnet.

When IoTeX activates a new EVM hardfork on the delegate (e.g., a new EIP adding opcodes or changing gas rules):

1. **Update the go-ethereum dependency** — the agent's `go.mod` uses a `replace` directive pointing to IoTeX's go-ethereum fork. Update this to match the delegate's new fork version.
2. **Enable the new fork in chain config** — add the new fork to `iotexChainConfig()` in `evm.go` with block 0 activation (one line).
3. **Rebuild and redeploy** — the new binary automatically supports the updated EVM rules.

No fork-schedule synchronization is needed. The agent simply tracks the same go-ethereum version as the delegate.

## Related Repositories

| Repository | Description |
|------------|-------------|
| [iotex-core](https://github.com/iotexproject/iotex-core) (branch: `ioswarm-v2.3.5`) | Delegate node with IOSwarm coordinator |
| [ioswarm-portal](https://github.com/iotexproject/ioswarm-portal) | Dashboard and monitoring UI |

## License

Apache 2.0
