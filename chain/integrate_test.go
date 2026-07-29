package chain

import (
	"testing"

	"github.com/ITJHIT/onchain-orderbook/orderbook"
)

// A host chain persists the book and reloads it every transaction. The reload
// has to land on exactly the state it saved, or balances drift a little on every
// block until someone cannot withdraw.

func TestRestoreReproducesTheBookAndItsLocks(t *testing.T) {
	live := funded(Continuous, 6)
	live.ApplyBlock(1, sampleBlock())

	restored := funded(Continuous, 6)
	if err := restored.Restore(live.Book.Snapshot()); err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	if got, want := restored.Book.Len(), live.Book.Len(); got != want {
		t.Fatalf("restored %d resting orders, want %d", got, want)
	}
	a, b := restored.Book.Snapshot(), live.Book.Snapshot()
	for i := range b {
		if a[i] != b[i] {
			t.Fatalf("order %d differs after restore:\n got %+v\nwant %+v", i, a[i], b[i])
		}
	}

	// Locks are recomputed from the orders, never stored. They must come back
	// identical -- a lock that drifts from the book is money nothing releases.
	for id, want := range live.State.Snapshot() {
		got := restored.State.Balance(id)
		if got.LockedBase != want.LockedBase || got.LockedQuote != want.LockedQuote {
			t.Fatalf("account %d locks after restore: got base=%d quote=%d, want base=%d quote=%d",
				id[0], got.LockedBase, got.LockedQuote, want.LockedBase, want.LockedQuote)
		}
	}
}

// Restoring must not re-match: the stored orders are the *result* of matching.
func TestRestoreDoesNotInventFills(t *testing.T) {
	e := funded(Continuous, 4)
	// A crossed pair, as if a host had stored them (which a correct host never
	// would -- which is exactly why restore must not quietly "fix" it by trading).
	crossed := []orderbook.Order{
		{ID: orderbook.OrderID{Height: 1, Index: 0}, Account: acct(1), Side: orderbook.Buy, Price: 110, Qty: 5},
		{ID: orderbook.OrderID{Height: 1, Index: 1}, Account: acct(2), Side: orderbook.Sell, Price: 90, Qty: 5},
	}
	if err := e.Restore(crossed); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if e.Book.Len() != 2 {
		t.Fatalf("restore matched the orders against each other: %d left resting", e.Book.Len())
	}
	if b := e.State.Balance(acct(1)); b.Base != 1_000_000 {
		t.Fatalf("restore moved balances: %+v", b)
	}
}

func TestRestoreRefusesABookItCannotFund(t *testing.T) {
	e := NewEngine(Continuous)
	e.State.Credit(acct(1), 1, 0) // one unit of base, nowhere near enough
	err := e.Restore([]orderbook.Order{
		{ID: orderbook.OrderID{Height: 1}, Account: acct(1), Side: orderbook.Sell, Price: 10, Qty: 500},
	})
	if err != ErrRestoreLock {
		t.Fatalf("expected ErrRestoreLock for an unfundable book, got %v", err)
	}
}

// Applying one transaction at a host-chosen position must match what the
// block-level API does for that same position.
func TestApplyPositionedMatchesTheBlockAPI(t *testing.T) {
	block := sampleBlock()

	whole := funded(Continuous, 6)
	want := whole.ApplyBlock(9, block)

	piecemeal := funded(Continuous, 6)
	var fills []orderbook.Fill
	for i, tx := range block {
		r := piecemeal.ApplyPositioned(9, uint32(i), tx)
		fills = append(fills, r.Fills...)
	}

	if len(fills) != len(want.Fills) {
		t.Fatalf("piecemeal produced %d fills, block API produced %d", len(fills), len(want.Fills))
	}
	for i := range fills {
		if fills[i] != want.Fills[i] {
			t.Fatalf("fill %d differs:\n got %+v\nwant %+v", i, fills[i], want.Fills[i])
		}
	}
	if got := piecemeal.State.Root(piecemeal.Book); got != want.Root {
		t.Fatalf("piecemeal root %x != block root %x", got, want.Root)
	}
}

// The persistence cycle a host runs every transaction: save, reload, continue.
// Doing it between every transaction must land where never reloading would.
func TestSaveReloadCycleIsInvisible(t *testing.T) {
	block := sampleBlock()

	straight := funded(Continuous, 6)
	want := straight.ApplyBlock(9, block)

	cycled := funded(Continuous, 6)
	for i, tx := range block {
		cycled.ApplyPositioned(9, uint32(i), tx)

		// Persist and reload, the way a chain does between transactions.
		orders := cycled.Book.Snapshot()
		balances := cycled.State.Snapshot()
		next := NewEngine(Continuous)
		for id, b := range balances {
			next.State.Credit(id, b.Base, b.Quote)
		}
		if err := next.Restore(orders); err != nil {
			t.Fatalf("tx %d: reload failed: %v", i, err)
		}
		cycled = next
	}

	if got := cycled.State.Root(cycled.Book); got != want.Root {
		t.Fatalf("root after save/reload every tx is %x, want %x", got, want.Root)
	}
}
