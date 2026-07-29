package chain

import (
	"errors"

	"github.com/ITJHIT/onchain-orderbook/orderbook"
)

// The API a host chain needs, as opposed to the API a standalone simulation
// needs. A chain does not hand you a block of orders and ask for a result -- it
// applies one transaction at a time, at a position it chooses, against state it
// loaded from its own store. These three methods are that seam.

// ErrRestoreLock is returned when a restored order cannot be collateralised.
// Reaching it means stored balances and the stored book disagree, which is
// corruption rather than a user error: refuse rather than silently rebuild a
// book whose locks do not add up.
var ErrRestoreLock = errors.New("restored order cannot be funded from stored balances")

// Snapshot returns a copy of every account's balance.
//
// A copy, not the live map: a host that could mutate this would be able to move
// balances without going through settlement, and the invariants the tests check
// would stop meaning anything.
func (s *State) Snapshot() map[orderbook.AccountID]Balance {
	out := make(map[orderbook.AccountID]Balance, len(s.accounts))
	for id, b := range s.accounts {
		out[id] = *b
	}
	return out
}

// Restore rebuilds the book from a canonical snapshot, re-deriving every lock
// from the orders themselves.
//
// The orders are rested, never submitted: submitting would match them against
// each other, and they are already the *result* of matching. That distinction is
// the whole reason this is a separate method rather than a loop over Submit.
//
// Locks are recomputed rather than stored. Storing them would create a second
// source of truth that can drift from the book, and a drifted lock is invisible
// until the day a user cannot withdraw funds no order is holding.
func (e *Engine) Restore(orders []orderbook.Order) error {
	for _, o := range orders {
		if err := e.State.lockFor(o); err != nil {
			return ErrRestoreLock
		}
		e.Book.Rest(o)
	}
	return nil
}

// ApplyPositioned applies one transaction at an explicit position in the chain.
//
// The host supplies (height, index) because order identity must come from
// position in the chain and not from a counter this package keeps: a counter
// would diverge the moment two nodes processed a different number of rejected
// transactions.
func (e *Engine) ApplyPositioned(height uint64, index uint32, tx Tx) TxResult {
	if tx.Kind == TxCancel {
		return e.doCancel(index, tx)
	}

	o := orderbook.Order{
		ID:      orderbook.OrderID{Height: height, Index: index},
		Account: tx.Account,
		Side:    tx.Side,
		Price:   tx.Price,
		Qty:     tx.Qty,
	}
	if err := e.State.lockFor(o); err != nil {
		return TxResult{Index: index, Err: err}
	}

	var fills []orderbook.Fill
	filled := e.Book.Submit(o, e.MaxSweepLevels, &fills)
	for _, f := range fills {
		if err := e.State.settle(f, o.Price); err != nil {
			return TxResult{Index: index, Err: err}
		}
	}
	return TxResult{Index: index, Accepted: true, OrderID: o.ID, Filled: filled, Fills: fills}
}
