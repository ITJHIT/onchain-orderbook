package chain

import "github.com/ITJHIT/onchain-orderbook/orderbook"

// AuctionSummary is what a host needs after a batch clears: the price and
// volume that make a frequent batch auction worth having in the first place,
// plus the fills themselves so the host can emit whatever event or receipt
// shape it wants without re-deriving them.
type AuctionSummary struct {
	Cleared bool
	Price   orderbook.Price
	Volume  orderbook.Qty
	Fills   []orderbook.Fill
}

// StagePlace locks funds for a placement and hands it back for the caller to
// accumulate. It does NOT match.
//
// Pair with ClearBatch: staging every placement in a block before matching any
// of them is the entire mechanism. A host that staged some orders and matched
// others immediately would have reintroduced position-sensitivity through the
// side door -- whoever got staged last would clear against a book the earlier
// ones had already moved.
func (e *Engine) StagePlace(height uint64, index uint32, tx Tx) (TxResult, *orderbook.Order) {
	if tx.Kind != TxPlace {
		return TxResult{Index: index, Err: ErrBadOrder}, nil
	}
	o := orderbook.Order{
		ID:      orderbook.OrderID{Height: height, Index: index},
		Account: tx.Account,
		Side:    tx.Side,
		Price:   tx.Price,
		Qty:     tx.Qty,
	}
	if err := e.State.lockFor(o); err != nil {
		return TxResult{Index: index, Err: err}, nil
	}
	return TxResult{Index: index, Accepted: true, OrderID: o.ID}, &o
}

// ClearBatch runs the uniform-price auction over every staged order, settles
// the resulting fills, and rests whatever did not fill. Safe to call with an
// empty slice -- a block that only cancelled orders clears nothing.
//
// The economic result does not depend on the order of `staged`: orderbook.Clear
// sorts its own candidate prices and ration() sorts its own allocation order
// internally, neither reads from the slice's position. That is not asserted
// here -- it is asserted at the library level by
// TestBatchAuctionIgnoresPositionInBlock -- but it is why a host can stage
// transactions in whatever order they arrived in a block without having to
// canonicalise them first.
//
// A settlement error here means the invariant that locked funds always suffice
// to settle has broken; matching applyContinuous's handling of the same
// impossible case, this stops rather than continuing over a corrupted balance.
// It is still possible for earlier fills in the same call to have already
// mutated e.State before the failing one is reached -- callers that need that
// to be atomic get it for free at the host level: a host applying this inside
// a copy-on-write overlay (as l1chain does) discards the whole block, staged
// mutations included, on any error.
func (e *Engine) ClearBatch(staged []orderbook.Order) (AuctionSummary, error) {
	auction := orderbook.Clear(staged)
	if !auction.Cleared {
		for _, o := range staged {
			e.restLeftover(o, 0)
		}
		return AuctionSummary{}, nil
	}

	limits := make(map[orderbook.OrderID]orderbook.Order, len(staged))
	for _, o := range staged {
		limits[o.ID] = o
	}

	var buys, sells []orderbook.Allocation
	for _, a := range auction.Allocations {
		if a.Side == orderbook.Buy {
			buys = append(buys, a)
		} else {
			sells = append(sells, a)
		}
	}

	filled := make(map[orderbook.OrderID]orderbook.Qty, len(staged))
	var fills []orderbook.Fill
	bi, si := 0, 0
	for bi < len(buys) && si < len(sells) {
		q := buys[bi].Qty
		if sells[si].Qty < q {
			q = sells[si].Qty
		}
		f := orderbook.Fill{
			TakerID:      buys[bi].ID,
			MakerID:      sells[si].ID,
			TakerAccount: buys[bi].Account,
			MakerAccount: sells[si].Account,
			TakerSide:    orderbook.Buy,
			Price:        auction.Price,
			Qty:          q,
		}
		if err := e.State.settle(f, limits[buys[bi].ID].Price); err != nil {
			return AuctionSummary{}, err
		}
		fills = append(fills, f)
		filled[buys[bi].ID] += q
		filled[sells[si].ID] += q
		buys[bi].Qty -= q
		sells[si].Qty -= q
		if buys[bi].Qty == 0 {
			bi++
		}
		if sells[si].Qty == 0 {
			si++
		}
	}

	for _, o := range staged {
		e.restLeftover(o, filled[o.ID])
	}
	return AuctionSummary{Cleared: true, Price: auction.Price, Volume: auction.Volume, Fills: fills}, nil
}
