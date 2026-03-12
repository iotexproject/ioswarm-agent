# ioSwarm × OpenClaw — User Journey

> Your laptop validates IoTeX transactions while you sleep. You earn IOTX.

---

## How Users Discover ioSwarm

### Path A: Word of mouth

Someone posts on X / Discord / Telegram:

> "I'm earning ~2 IOTX/day just leaving my laptop on. Ask OpenClaw to install ioswarm."

The user opens OpenClaw and types: **"What is ioswarm?"**

OpenClaw explains:
> ioSwarm lets your machine validate IoTeX blockchain transactions in the background. It uses less than 64MB RAM and barely any CPU. IoTeX delegates pay you from their block rewards for helping validate. You can earn IOTX 24/7 while your computer sits idle.
>
> Want me to set it up? Takes about 30 seconds.

### Path B: ClawHub skill marketplace

User browses ClawHub, sees:

```
ioswarm
Earn IOTX by validating IoTeX transactions in the background.
Your machine joins the IoTeX execution layer as an autonomous agent.
```

Clicks install. OpenClaw loads the skill.

User: **"Start earning"**

### Path C: Direct link

A delegate shares their referral page or the install command:

> Earn IOTX with starfish delegate — tell OpenClaw: "install ioswarm and connect to starfish"

---

## First-Time Setup (30 seconds)

The entire setup is one conversation. The user never touches a config file, terminal, or private key.

### User says: "Set up ioswarm" (or "I want to earn crypto")

**Step 1 — Install** (OpenClaw runs silently):
```bash
curl -sSL https://raw.githubusercontent.com/iotexproject/ioswarm-agent/main/openclaw/install.sh | bash
```
Downloads the agent binary (~16 MB) and management script to `~/.ioswarm/agent/`.

**Step 2 — Setup** (OpenClaw runs silently):
```bash
~/.ioswarm/agent/ioswarm.sh setup
```
- Generates a secp256k1 wallet (private key stored locally, mode 600)
- Queries all known delegates for availability and payout rates
- Picks the best-paying delegate automatically
- Writes config

**Step 3 — OpenClaw reports back:**

> All set! Here's what I did:
>
> - Created your IOTX wallet: `0x7a3B...9f2E`
> - Connected to delegate **starfish** (best rate: ~2.4 IOTX per epoch)
> - Level: L2 — your machine will verify transaction signatures and account nonces
>
> It uses less than 64MB RAM and almost no CPU. Want me to start earning now?

### User says: "Yes"

```bash
~/.ioswarm/agent/ioswarm.sh start
```

> Your agent is running in the background (PID 42381). It's now validating transactions and accumulating IOTX rewards.
>
> You can close this chat — it keeps earning. Check back anytime: **"how's my ioswarm?"**

**Total elapsed: ~30 seconds. Zero configuration by the user.**

---

## Daily Usage

### "How's my ioswarm?" / "Check earnings"

OpenClaw runs `ioswarm.sh status`, reads the JSON, and says:

> Running for 3 days, validated 8,340 transactions for delegate starfish.
>
> **Claimable: 12.4 IOTX (~$2.50)**
>
> Say "claim" to withdraw to your wallet.

### "Claim my rewards"

> Claimed **12.4 IOTX** to `0x7a3B...9f2E`. [View transaction](https://iotexscan.io/tx/0xabc1...)
>
> Your agent is still running and earning more.

### "Is there a better delegate?"

> | Delegate | Agents | Your rate |
> |----------|--------|-----------|
> | iotex-dev | 8 | ~3.2 IOTX/epoch |
> | **starfish** | 12 | ~2.4 IOTX/epoch _(current)_ |
> | metanyx | 45 | ~1.8 IOTX/epoch |
>
> iotex-dev pays more because fewer agents are sharing the pool. Switch?

### "Switch to iotex-dev"

> Switched to **iotex-dev**. Agent restarted. Unclaimed rewards from starfish are still safe on-chain.

### "Stop" / "Pause ioswarm"

> Agent stopped. Your 3.1 IOTX in unclaimed rewards are safe — claim anytime. Say **"start ioswarm"** to resume.

### "Upgrade"

> Upgraded ioswarm-agent v0.2.0 → v0.3.0. Restarted automatically.

---

## Things That Can Go Wrong

### Delegate goes offline

The agent retries automatically. If it can't reconnect after a few minutes, OpenClaw (on next interaction) says:

> Your ioSwarm agent lost connection to starfish — looks like the delegate is down. Want me to switch to iotex-dev? Your unclaimed rewards are safe.

### Agent crashes

User asks "check ioswarm" and OpenClaw sees the process is gone:

> Your agent stopped unexpectedly. Last log entry:
> `connection reset by peer`
>
> Want me to restart it?

### "I already have an IOTX wallet"

> Sure — give me your wallet address (just the address, never your private key). I'll configure the agent to earn rewards to that address.
>
> Note: you'll claim rewards using your own wallet's private key, outside of ioSwarm.

### "Is this safe? What does it access?"

> The agent only validates transactions — it checks math (signatures, nonces, balances). It doesn't send transactions, access your files, or use the network beyond connecting to one IoTeX delegate. Your private key is stored locally at `~/.ioswarm/agent/wallet.key` (only used when you claim rewards). The binary is open source: [github.com/iotexproject/ioswarm-agent](https://github.com/iotexproject/ioswarm-agent).

---

## What's On the User's Machine

```
~/.ioswarm/agent/
  ioswarm-agent       # binary (~16 MB)
  ioswarm.sh          # management script
  wallet.key          # private key (mode 600, never leaves disk)
  wallet.addr         # 0x... address
  config.env          # delegate address, agent ID, level
  agent.pid           # PID file
  agent.log           # log output
  delegates.json      # known delegate registry
```

No cloud accounts. No sign-ups. No email. Everything local.

---

## The Economics (User's Perspective)

**What the user needs to know:**

- IoTeX delegates earn block rewards (~800 IOTX per epoch)
- Delegates share a portion (typically 90%) with ioSwarm agents
- More agents = smaller per-agent share (but also more delegates competing for agents)
- `ioswarm.sh discover` finds the best rate in real time
- Rewards accumulate on-chain — claim anytime, no minimum

**What the user does NOT need to know:**

- F1 distribution algorithm
- gRPC protocol details
- Coordinator architecture
- L1/L2/L3/L4 capability levels (just "L2" in status is fine)
- Contract addresses

**Rough earning estimates (depends on delegate pool):**

| Scenario | Your rate | Daily (~24 epochs) |
|----------|-----------|-------------------|
| 10 agents, 800 IOTX/epoch, 10% cut | 72 IOTX/epoch | ~1,728 IOTX |
| 50 agents, 800 IOTX/epoch, 10% cut | 14.4 IOTX/epoch | ~346 IOTX |
| 100 agents, 1200 IOTX/epoch, 15% cut | 10.2 IOTX/epoch | ~245 IOTX |

_(These are illustrative. Actual rates shown by `ioswarm.sh discover`.)_

---

## Conversation Cheatsheet

| User says | OpenClaw does |
|-----------|--------------|
| "What is ioswarm?" | Explains in 3 sentences |
| "Set it up" / "Install ioswarm" | `install.sh` + `ioswarm.sh setup` |
| "Start earning" | `ioswarm.sh start` |
| "How's my ioswarm?" / "Check earnings" | `ioswarm.sh status` → human summary |
| "Claim" / "Withdraw" | `ioswarm.sh claim` → reports amount + tx |
| "Which delegate is best?" | `ioswarm.sh discover` → table |
| "Switch to X" | `ioswarm.sh switch X` |
| "Stop" / "Pause" | `ioswarm.sh stop` |
| "Upgrade" | `ioswarm.sh upgrade` |
| "Show logs" | `ioswarm.sh logs` |
| "Is this safe?" | Explains security model |
| "How does this work?" | 3-sentence explainer |
| "Back up my wallet" | Points to `~/.ioswarm/agent/wallet.key` |
