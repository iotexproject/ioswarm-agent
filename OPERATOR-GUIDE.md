# IOSwarm L4 Agent — Operator Guide

Run an IOSwarm agent on your Mac mini, MacBook, or any machine to validate IoTeX mainnet transactions and earn IOTX rewards.

## Requirements

- Go 1.22+ (for building from source)
- ~2 GB RAM (for L4 state store)
- ~2 GB disk (state store grows over time)
- Stable internet connection to delegate

## Quick Start

### 1. Build the agent

```bash
git clone https://github.com/iotexproject/ioswarm-agent.git
cd ioswarm-agent
go build -o ioswarm-agent .
```

### 2. Generate a wallet

Each agent needs its own wallet for reward payouts:

```bash
./ioswarm-agent keygen -out my-agent.key
# Output: Address: 0x1234...abcd
```

Save the key file securely. The address is your reward wallet.

### 3. Get your API key

Contact the delegate operator to get:
- **Coordinator address** (e.g., `delegate.goodwillclaw.com:443`)
- **API key** (`iosw_...` format, derived from your agent ID)

Or if you have the delegate's master secret, derive it yourself:
```bash
echo -n "my-agent-id" | openssl dgst -sha256 -hmac "<master_secret>" -hex
# Prepend "iosw_" to the hex output
```

### 4. Download the bootstrap snapshot

L4 agents need a state snapshot for initial bootstrap. Download from the delegate or CDN:

```bash
# Account + Code snapshot (~209 MB, sufficient for L4)
curl -O https://<snapshot-server>/acctcode.snap.gz
```

You only need this once. After first boot, the agent stores state locally and syncs incrementally.

### 5. Start the agent

```bash
./ioswarm-agent \
  --coordinator=delegate.goodwillclaw.com:443 \
  --agent-id=my-agent-id \
  --api-key=iosw_<your_key> \
  --level=L4 \
  --snapshot=./acctcode.snap.gz \
  --datadir=./l4state \
  --wallet=0x1234...abcd
```

First boot takes ~10 seconds to load the snapshot. After that, the agent syncs state diffs from the coordinator in real time.

### 6. Run as a background service

```bash
nohup ./ioswarm-agent \
  --coordinator=delegate.goodwillclaw.com:443 \
  --agent-id=my-agent-id \
  --api-key=iosw_<your_key> \
  --level=L4 \
  --snapshot=./acctcode.snap.gz \
  --datadir=./l4state \
  --wallet=0x1234...abcd > agent.log 2>&1 &
```

Or use `launchd` (macOS) / `systemd` (Linux) for auto-restart.

## CLI Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--coordinator` | `127.0.0.1:14689` | Coordinator gRPC address |
| `--agent-id` | _(required)_ | Unique agent identifier |
| `--api-key` | | HMAC API key (`iosw_...`) |
| `--level` | `L3` | Task level: L1, L2, L3, L4 |
| `--region` | `default` | Region label |
| `--wallet` | | IOTX wallet address for rewards |
| `--datadir` | | Data directory for L4 state store |
| `--snapshot` | | Path to IOSWSNAP file for bootstrap |
| `--tls-cert` | | Path to TLS certificate (optional) |

Environment variables are also supported: `IOSWARM_API_KEY`, `IOSWARM_AGENT_ID`, `IOSWARM_WALLET`, `IOSWARM_COORDINATOR`, `IOSWARM_DATADIR`.

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

## Task Levels

| Level | What It Does | State Needed |
|-------|-------------|--------------|
| L1 | Signature verification | None |
| L2 | + Nonce/balance checks | Coordinator-provided snapshots |
| L3 | + Full EVM execution | Coordinator-provided state |
| **L4** | **Independent EVM execution** | **Local full state via snapshot + diffs** |

L4 agents maintain their own copy of the blockchain state and don't depend on coordinator-provided snapshots for each transaction. This gives the highest accuracy (99.5%+).

## Rewards

Rewards are distributed per epoch:
- Delegate keeps a configurable cut (e.g., 10%)
- Remaining pool is split proportionally by task weight
- Agents receive payouts via heartbeat responses
- Claim rewards on-chain from the AgentRewardPool contract

Check your earnings in the agent log:
```
{"msg":"payout received","epoch":245,"amount_iotx":0.082}
```

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
curl -s "http://<coordinator>:14690/swarm/agents" \
  -H "X-Ioswarm-Agent-Id: my-agent-id" \
  -H "X-Ioswarm-Token: iosw_<your_key>"
```

## Troubleshooting

### "failed to load snapshot: read value: unexpected EOF"
Snapshot file may be corrupted or incomplete. Re-download and verify the file size matches the source.

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
