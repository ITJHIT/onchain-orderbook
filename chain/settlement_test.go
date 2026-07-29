package chain

import (
	"testing"

	"github.com/ITJHIT/onchain-orderbook/orderbook"
)

// Settlement is where a rounding slip becomes a missing coin. These check the
// invariants that must hold after every block, not just that fills happened.

// total holdings across all accounts, free plus locked. Trading moves value
// between accounts; it must never create or destroy any.
func totals(e *Engine) (base, quote int64) {
	for _, b := range e.State.accounts {
		base += b.Base
		quote += b.Quote
	}
	return
}

func TestValueIsConservedAcrossTrading(t *testing.T) {
	e := funded(Continuous, 6)
	wantBase, wantQuote := totals(e)

	for h := uint64(1); h <= 5; h++ {
		e.ApplyBlock(h, sampleBlock())
		gotBase, gotQuote := totals(e)
		if gotBase != wantBase {
			t.Fatalf("block %d: base supply %d, want %d", h, gotBase, wantBase)
		}
		if gotQuote != wantQuote {
			t.Fatalf("block %d: quote supply %d, want %d", h, gotQuote, wantQuote)
		}
	}
}

func TestLockedFundsNeverExceedHoldings(t *testing.T) {
	e := funded(Continuous, 6)
	for h := uint64(1); h <= 5; h++ {
		e.ApplyBlock(h, sampleBlock())
		for id, b := range e.State.accounts {
			if b.LockedBase < 0 || b.LockedQuote < 0 {
				t.Fatalf("account %d has negative lock: %+v", id[0], *b)
			}
			if b.LockedBase > b.Base {
				t.Fatalf("account %d locks %d base but holds %d", id[0], b.LockedBase, b.Base)
			}
			if b.LockedQuote > b.Quote {
				t.Fatalf("account %d locks %d quote but holds %d", id[0], b.LockedQuote, b.Quote)
			}
		}
	}
}

// A buy that crosses locks quote at its own limit but pays the maker's price.
// The difference has to come back, or every price improvement quietly strands a
// little of the buyer's money in a lock nothing will ever release.
func TestPriceImprovementRefundsTheOverLock(t *testing.T) {
	e := NewEngine(Continuous)
	e.State.Credit(acct(1), 100, 0)     // seller: base only
	e.State.Credit(acct(2), 0, 100_000) // buyer: quote only

	e.ApplyBlock(1, []Tx{
		{Kind: TxPlace, Account: acct(1), Side: orderbook.Sell, Price: 100, Qty: 10},
	})
	// Buyer is willing to pay 150 but the book offers 100.
	got := e.ApplyBlock(2, []Tx{
		{Kind: TxPlace, Account: acct(2), Side: orderbook.Buy, Price: 150, Qty: 10},
	})

	if len(got.Fills) != 1 || got.Fills[0].Price != 100 {
		t.Fatalf("expected one fill at the maker price of 100, got %+v", got.Fills)
	}
	buyer := e.State.Balance(acct(2))
	if buyer.Quote != 100_000-1000 {
		t.Fatalf("buyer paid %d, expected 1000 (10 @ 100)", 100_000-buyer.Quote)
	}
	if buyer.LockedQuote != 0 {
		t.Fatalf("buyer still has %d quote locked after a fully filled order; "+
			"the 500 over-lock (10 @ 150 vs 10 @ 100) was not released", buyer.LockedQuote)
	}
	if buyer.Base != 10 {
		t.Fatalf("buyer received %d base, want 10", buyer.Base)
	}
	seller := e.State.Balance(acct(1))
	if seller.Quote != 1000 || seller.Base != 90 || seller.LockedBase != 0 {
		t.Fatalf("seller settled wrong: %+v", seller)
	}
}

func TestAnOrderYouCannotFundIsRejected(t *testing.T) {
	e := NewEngine(Continuous)
	e.State.Credit(acct(1), 5, 100)

	got := e.ApplyBlock(1, []Tx{
		{Kind: TxPlace, Account: acct(1), Side: orderbook.Sell, Price: 10, Qty: 50}, // only 5 base
		{Kind: TxPlace, Account: acct(1), Side: orderbook.Buy, Price: 10, Qty: 50},  // needs 500 quote
		{Kind: TxPlace, Account: acct(9), Side: orderbook.Buy, Price: 10, Qty: 1},   // no account
		{Kind: TxPlace, Account: acct(1), Side: orderbook.Sell, Price: 10, Qty: 5},  // fits exactly
	})

	for i := 0; i < 3; i++ {
		if got.Results[i].Accepted {
			t.Fatalf("tx %d should have been rejected: %+v", i, got.Results[i])
		}
		if got.Results[i].Err != ErrInsufficientFunds {
			t.Fatalf("tx %d rejected for %v, want insufficient funds", i, got.Results[i].Err)
		}
	}
	if !got.Results[3].Accepted {
		t.Fatalf("the affordable order should have been accepted: %+v", got.Results[3])
	}
	if b := e.State.Balance(acct(1)); b.LockedBase != 5 {
		t.Fatalf("expected exactly the affordable order to be locked, got %+v", b)
	}
}

func TestCancelReleasesTheLockAndOnlyTheOwnerMayCancel(t *testing.T) {
	e := funded(Continuous, 3)
	placed := e.ApplyBlock(1, []Tx{
		{Kind: TxPlace, Account: acct(1), Side: orderbook.Sell, Price: 500, Qty: 40},
	})
	id := placed.Results[0].OrderID
	if b := e.State.Balance(acct(1)); b.LockedBase != 40 {
		t.Fatalf("expected 40 base locked, got %+v", b)
	}

	stolen := e.ApplyBlock(2, []Tx{{Kind: TxCancel, Account: acct(2), Cancel: id}})
	if stolen.Results[0].Accepted {
		t.Fatal("an account cancelled an order it does not own")
	}
	if e.Book.Len() != 1 {
		t.Fatal("the order was removed by a cancel that should have failed")
	}

	own := e.ApplyBlock(3, []Tx{{Kind: TxCancel, Account: acct(1), Cancel: id}})
	if !own.Results[0].Accepted {
		t.Fatalf("owner could not cancel: %+v", own.Results[0])
	}
	if b := e.State.Balance(acct(1)); b.LockedBase != 0 {
		t.Fatalf("cancel left %d base locked", b.LockedBase)
	}
	if e.Book.Len() != 0 {
		t.Fatal("order still resting after cancel")
	}
}

// An unbounded sweep is a denial-of-service vector: seed a thousand dust levels
// and one order costs the whole block's compute budget.
func TestSweepIsBounded(t *testing.T) {
	e := funded(Continuous, 3)
	e.MaxSweepLevels = 4

	var seed []Tx
	for i := 0; i < 20; i++ {
		seed = append(seed, Tx{
			Kind: TxPlace, Account: acct(1), Side: orderbook.Sell,
			Price: orderbook.Price(100 + i), Qty: 1,
		})
	}
	e.ApplyBlock(1, seed)

	got := e.ApplyBlock(2, []Tx{
		{Kind: TxPlace, Account: acct(2), Side: orderbook.Buy, Price: 200, Qty: 20},
	})
	if len(got.Fills) > 5 {
		t.Fatalf("swept %d levels with a bound of 4", len(got.Fills))
	}
	if len(got.Fills) == 0 {
		t.Fatal("bounded should not mean blocked; the order matched nothing")
	}
	// The remainder rests rather than being rejected: rejecting would let
	// whoever seeded the dust also decide whose orders fail.
	if !got.Results[0].Accepted {
		t.Fatal("the bounded order should still be accepted and rest its remainder")
	}
}
