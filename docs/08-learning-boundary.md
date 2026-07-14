# 08 — The Learning Boundary

> "In consuming intelligence, you are creating intelligence. And what you create
> should belong to you."

## The problem: the Reverse Information Paradox

Arrow's Information Paradox is about the **seller's** risk: to sell information you
must reveal it, and once revealed it has been given away. Patents solve this — an
inventor can disclose an idea without surrendering it.

AI inverts the risk onto the **buyer**. To get value from a model you must feed it
your proprietary context: your prompts, your documents, your workflows, and above
all your *corrections* — the places where the model was wrong and you knew better.
You pay twice: once in money, once in knowledge. And the second payment is
invisible, unpriced, and unbounded. The better you want the model to be, the more
of yourself you must hand over.

This asymmetry compounds. The seller learns about you every time you use what you
bought; you learn almost nothing about what they learned. Value converges toward
whoever owns the learning infrastructure rather than whoever created the knowledge.

**The Reverse Information Paradox needs the equivalent of a patent: an instrument
that lets you *use* a model without surrendering the knowledge that makes you
unique.**

## Why Tollgate is the right place to solve it

Tollgate already sits on the exact wire where the problem occurs. Every paid call
crosses the facilitator: the buyer's request goes out, the seller's answer comes
back. The rail already meters **money** flowing buyer → seller.

It has been blind to the **knowledge** flowing seller-ward on the very same wire.

That flow — the *intelligence exhaust* — is today free, invisible and
non-consensual. Tollgate is the one component positioned to price it, gate it, and
prove what happened.

## The primitives

### 1. Exhaust rights are protocol, not terms-of-service

A seller declares what it wants to do with the exhaust of a call. This travels
inside the **facilitator-signed quote**, so a seller cannot quietly change the ask
after the fact:

| Right | Meaning |
|---|---|
| `retain` | store the request/response beyond serving it |
| `train` | train or fine-tune the seller's models on it |
| `distill` | use the outputs to distill another model |
| `share_third_party` | pass it to anyone else |
| `human_review` | show it to a human reviewer |
| `improve_memory` | fold it into cross-customer memory |

Rights are **`required`** (the seller will not serve without them) or **`optional`**
(the seller would like them, and offers a **rebate** in exchange).

The buyer answers with a **grant**, carried inside the **agent-signed payment**.
Consent is now a cryptographic artifact, not a checkbox in a contract.

### 2. Deny by default — a hard boundary

The effective grant is the intersection of what was *asked* and what was *granted*:

```
effective = (required ∪ optional) ∩ grant
```

Nothing crosses that isn't explicitly granted. The buyer's policy (the same engine
that gates spend, `docs/05-policy-engine.md`) gains an `exhaust_rights` rule listing
what an agent may **ever** grant. **No rule ⇒ nothing is grantable.** Silence is
refusal.

If a seller *requires* a right the policy will not grant, the payment is **denied**:
no funds move and no data crosses. The service is simply unusable under that policy,
and marketplace search routes the buyer to a rights-respecting competitor. The
terms-of-service decision that used to need a lawyer is now automatic, machine-
enforced, and made *before* the knowledge leaves the building.

### 3. The exhaust gets a price — the data dividend

If learning has value, it should be bought, not extracted. A seller that wants
`train` rights offers a rebate; a buyer who grants it is **paid** for the knowledge.
This settles as real legs in the double-entry ledger, gross and auditable:

```
debit  buyer    1000    price
credit seller   1000    revenue
debit  seller    150    data dividend paid for the exhaust
credit buyer     150    dividend earned
                        → net: buyer -850, seller +850
```

Revenue and data-cost stay separately visible, so a seller's books show exactly what
they paid for knowledge — and a buyer's show exactly what their exhaust earned. A
buyer who refuses simply pays list price. **Learning becomes a trade instead of an
extraction.**

### 4. Rights-bearing receipts — the patent equivalent

The effective grant and the dividend are bound into the facilitator-signed receipt
issued to both parties (`docs/03-data-model.md` `receipts`). That receipt is the
instrument Arrow's paradox needed, pointed the other way: a non-repudiable,
independently verifiable record of **exactly what was disclosed, what was granted,
what was refused, and what was paid for it**.

If a seller later trains on a trace it was never granted, the buyer holds a signed
artifact proving the boundary — and so does the seller, proving they honored it.

## What this delivers against the five requirements

| Requirement | How Tollgate delivers it |
|---|---|
| **Control** | Rights are deny-by-default and policy-enforced; every grant is signed and receipted. The buyer owns their traces, evals and corrections by construction — nothing leaves without consent. |
| **Choice** | The marketplace is model-agnostic and searchable *by rights*: `GET /v1/services?excludeRequired=train` finds services that will not train on you. If a model is taken away, the orchestration layer still stands. |
| **Cost** | Analytics + dynamic pricing (Milestone 5) already optimize spend across models. The dividend now offsets it: your exhaust is an asset on the balance sheet, not a leak. |
| **Compound** | Grants, refusals, dividends and receipts accumulate on the buyer's side of the boundary as a durable, auditable record of the firm's own learning. |
| **Capability** | Out of scope for a payments rail: training inside the tenant is the enterprise's own infrastructure. Tollgate guarantees the boundary that makes it *safe* to do so. |

## Invariants

- **Deny by default.** An absent policy, an absent rule, or an absent grant all mean
  *no rights*. Silence never grants.
- **Signed both ways.** The ask is facilitator-signed inside the quote; the grant is
  agent-signed inside the payment. Neither side can rewrite history.
- **No rights without settlement.** Rights attach only to a settled transaction; a
  rejected or refunded call grants nothing.
- **The dividend never exceeds the price.** A call can be free, never negative.
- **Idempotent.** Replaying a paid request re-grants nothing and re-pays no dividend.
