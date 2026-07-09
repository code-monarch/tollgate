# 05 — Policy Engine

The policy engine is Tollgate's buyer-side moat. It is the last gate before a buyer's funds
move: given a signed quote and the agent's context, it returns **allow / deny / needs-approval**.
It must be deterministic, fast (`< 10ms`), and fully logged.

## Design goals

- **Deterministic** — same inputs, same decision. No network calls on the hot path except a
  balance read (cached) and an optional approval webhook (off the deny/allow path).
- **Explainable** — every decision returns the rule(s) that fired.
- **Composable** — rules stack; the most restrictive outcome wins.
- **Versioned** — a policy is immutable once active; changes create a new version.

## Rule schema (`policies.rules` jsonb)

```jsonc
{
  "version": 1,
  "currency": "USDC",
  "defaults": { "action": "deny" },        // deny-by-default is the safe posture
  "rules": [
    {
      "id": "per-call-ceiling",
      "type": "amount_ceiling",
      "scope": "call",
      "max": "50000"                         // 0.05 USDC, minor units
    },
    {
      "id": "task-budget",
      "type": "budget",
      "scope": "task",                       // window keyed by task_id
      "max": "5000000",                      // 5 USDC per task
      "window": "task"
    },
    {
      "id": "daily-budget",
      "type": "budget",
      "scope": "agent",
      "max": "20000000",                     // 20 USDC / day
      "window": "24h"
    },
    {
      "id": "domain-allowlist",
      "type": "allowlist",
      "field": "resource_host",
      "values": ["api.example.com", "*.trusted.dev"]
    },
    {
      "id": "category-blocklist",
      "type": "blocklist",
      "field": "service_category",
      "values": ["adult", "gambling"]
    },
    {
      "id": "rate-cap",
      "type": "velocity",
      "scope": "agent",
      "max_count": 100,
      "window": "1m"
    },
    {
      "id": "anomaly",
      "type": "anomaly",
      "signal": "price_spike",
      "factor": 5                            // >5x median price for this service → flag
    },
    {
      "id": "human-approval",
      "type": "approval",
      "threshold": "1000000",                // ≥1 USDC needs human sign-off
      "webhook": "https://buyer.example.com/approvals",
      "timeout": "300s",
      "on_timeout": "deny"
    }
  ]
}
```

## Rule types

| type | purpose |
|------|---------|
| `amount_ceiling` | hard max per call |
| `budget` | cumulative cap over a window (`task`, `24h`, `1h`, custom) |
| `allowlist` / `blocklist` | gate by host, category, seller, network, currency |
| `velocity` | max request count per window (rate limiting spend) |
| `anomaly` | statistical flags (price spike vs median, novel counterparty, burst) |
| `approval` | route to a human above a threshold; hold until resolved |

## Evaluation

```
Input:  { quote, agent, task_id, service, balances, history }
Output: { decision, firedRules[], approvalRequestId? }

decision ∈ { allow, deny, needs_approval }
```

Algorithm:
1. Start from `defaults.action` (recommend `deny`).
2. Evaluate all rules; collect verdicts. **Most restrictive wins**
   (`deny` > `needs_approval` > `allow`).
3. Check balance ≥ amount; insufficient funds → `deny` (distinct reason).
4. If any `approval` rule triggers and nothing denies → `needs_approval`.
5. Emit the decision + `firedRules` to the audit log **always**.

## Approval flow

On `needs_approval`, fire the webhook with the quote + fired rule, create an
`approval_request`, and hold the transaction. Resolution (approve/deny) or timeout
(`on_timeout`) finalizes it. Approvals never sit on the hot path — the agent's call is
parked, not blocking the engine.

## Anti-goals

- Not a general rules-DSL playground — a **fixed, audited set** of rule types. New types are
  added deliberately, not user-scripted (untrusted code near money is a non-starter).
- No ML on the hot path. Anomaly signals are precomputed; the hot path only reads them.
