# onchain-orderbook

[![CI](https://github.com/ITJHIT/onchain-orderbook/actions/workflows/ci.yml/badge.svg)](https://github.com/ITJHIT/onchain-orderbook/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

**A limit order book that can run inside consensus — and a harness that proves it.**

The matching/settlement engine at the center of [`l1chain_JIHO`](https://github.com/ITJHIT/l1chain_JIHO)'s
on-chain exchange, and the consensus-facing counterpart to
[`lowlat-oms-core`](https://github.com/ITJHIT/lowlat-oms-core)'s off-chain OMS: same
price-time-priority matching discipline, opposite constraint — this one has to
agree byte-for-byte across every validator, not just be fast on one machine.

An off-chain book only has to be fast. A book that lives *in consensus* has to be
fast **and** produce byte-identical results on every validator from the same
ordered input — forever, including on a node replaying from genesis three years
later on different hardware. That second requirement is the entire engineering
problem, and it is what this repository is about.

It sits where low-latency exchange infrastructure meets state-machine
replication: price-time priority, integer settlement, a state commitment, and a
frequent batch auction as the structural answer to block-producer MEV.

---

## The determinism budget

Determinism is not a coding style here, it is a constraint that removes tools:

| Removed | Why |
|---|---|
| Floating point | IEEE-754 is deterministic per operation, but compilers may contract `a*b+c` into an FMA, reassociate, or keep intermediates in wider registers. Two validators built with different flags then disagree about who got filled. Prices and quantities are integers. |
| Map iteration on any output path | Go randomises map iteration **on purpose**. A book that walked a map of price levels would match in a different order on every node — and on every run of the same node. Levels live in sorted slices. Maps are used only where they are read by key and never iterated. |
| Wall clock, randomness | Time is whatever the block header says. Sequence comes from position in the block. |
| Unbounded work | One order sweeping ten thousand dust levels is a denial-of-service vector, not a fast fill. Sweeps are bounded, and hitting the bound rests the remainder rather than rejecting — otherwise whoever seeds the dust also chooses whose orders fail. |

## Proof, not assertion

`chain/determinism_test.go` tries to break the claim the way it actually breaks:

- **Independent nodes agree.** Eight fresh engines, one block, identical roots and
  identical fills — in both matching modes.
- **The same node is stable across 200 runs.** This is the check that catches
  map-order dependence, since the input never changes but map order does.
- **Replay from genesis lands where the live node did.** Without this a new node
  can never sync.
- **A demonstration of the hazard, not just the defence.** One test computes the
  state root the naive way — walking the account map — alongside the real sorted
  one. On a recent run the naive root produced **8 distinct values over 200
  iterations while the sorted root produced 1**. The failure mode is shown, not
  described.

## Continuous matching vs. batch auction

Both modes are implemented, because the comparison is the point.

Under **continuous** matching each order matches as it is applied, so whoever is
earlier in the block takes the better resting price. On a chain, "earlier" is
decided by whoever orders the block — which hands the block producer a free
option on every trade. A test asserts this sensitivity exists rather than
pretending it does not: swap two identical orders and a different account gets
filled.

Under a **frequent batch auction** every order in the block clears at one uniform
price, so position within the block is worth nothing. The cost is real —
continuous execution is given up, and the rationing rule becomes
consensus-critical.

### A bug worth keeping in the README

The first implementation rationed oversubscribed volume pro-rata, breaking ties
by `OrderID`. `OrderID` encodes the order's index within the block — so the block
producer could still choose who received the remainder units simply by
reordering. The uniform price removed the large, visible advantage and left a
small invisible one: a unit or two per auction, free to the producer, forever.
Exactly the kind of thing that survives review.

`TestBatchAuctionIgnoresPositionInBlock` caught it by rotating the block and
checking per-account fills. Rationing now ranks by account, then price, then
quantity — all fixed before the block is assembled — with `OrderID` left only as
a final tie-break between economically identical orders from the same account,
where the choice changes nobody's position.

The same test also documents where position-independence legitimately *stops*:
the state root is not compared across rotations, because `OrderID` renames
resting orders and the root should follow. Identifiers are allowed to depend on
position; economics are not.

## Settlement

Spot base/quote with explicit locks. Placing an order reserves funds; cancelling
releases them; a fill moves them. Keeping locked funds as a separate field
instead of deducting at placement makes the invariant *free + locked changes only
through a fill* directly checkable — and it is checked.

Two settlement details that are quiet money bugs when missed, both tested:

- **Price improvement must refund the over-lock.** A buy locks quote at its own
  limit but pays the maker's price. Forgetting the difference strands user funds
  in a lock nothing will ever clear.
- **Notional overflow is an error, not a wrap.** A silent overflow mints value
  out of nothing, and there is no sensible "close enough" for a balance.

`TestValueIsConservedAcrossTrading` asserts total supply of both assets is
unchanged after every block: trading moves value between accounts and must never
create or destroy any.

## Build & test

```bash
go vet ./...
go test ./... -v
go test ./... -race -count=5     # determinism tests are worth repeating
```

No dependencies outside the standard library.

## Status

The matching, auction, settlement and commitment layers are complete and tested.
The engine is deliberately a pure `ApplyBlock(height, txs) -> (fills, root)`
function so it can be driven by any consensus layer — and it now is: it is
wired into [`l1chain_JIHO`](https://github.com/ITJHIT/l1chain_JIHO)'s state
transition as `exchange/`, reachable through ordinary signed transactions in
either matching mode, with its own storage folded into that chain's state
root. See `l1chain_JIHO`'s [On-chain exchange](https://github.com/ITJHIT/l1chain_JIHO#on-chain-exchange)
section for how the seam works, including a real bug the integration surfaced
(order-ID collisions across blocks) and the regression test that catches it.

## License

MIT.
