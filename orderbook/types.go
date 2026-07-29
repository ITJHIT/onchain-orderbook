// Package orderbook is a limit order book that can run inside consensus.
//
// A book that lives off-chain only has to be fast. A book that lives *in
// consensus* has to be fast and produce byte-identical results on every
// validator from the same ordered input, forever, including on a node that
// replays the chain from genesis three years from now on different hardware.
// That second requirement is the whole engineering problem, and it rules out
// several things a normal matching engine does freely:
//
//   - **No floating point.** IEEE-754 is deterministic per operation, but
//     compilers are free to contract a*b+c into an FMA, reassociate, or keep
//     intermediates in wider registers. Two validators built with different
//     compiler flags then disagree about who got filled. Prices and quantities
//     here are integers and nothing else.
//   - **No map iteration on any path that affects output.** Go randomises map
//     iteration order deliberately, so a book that walked a map of price levels
//     would match in a different order on every node -- and, worse, on every
//     run of the same node. Levels live in sorted slices.
//   - **No wall clock and no randomness.** Time is whatever the block header
//     says it is. Sequence comes from position in the block.
//
// Everything here is written so that those constraints are checkable, not just
// claimed: see the determinism harness in package chain.
package orderbook

// Price and Qty are integers in ticks and base units. See the package comment
// for why this is not a stylistic preference.
type Price = int64
type Qty = int64

type Side uint8

const (
	Buy Side = iota
	Sell
)

func (s Side) String() string {
	if s == Buy {
		return "buy"
	}
	return "sell"
}

func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// AccountID is an opaque account identifier. Fixed-width so that canonical
// serialisation for the state root cannot depend on a variable-length encoding.
type AccountID [20]byte

// OrderID is assigned from position in the chain, never from a local counter:
// (block height, index within block). Two validators applying the same block
// therefore assign the same identifiers without having to agree on anything
// else, and a replay from genesis reproduces them exactly.
type OrderID struct {
	Height uint64
	Index  uint32
}

func (o OrderID) Less(other OrderID) bool {
	if o.Height != other.Height {
		return o.Height < other.Height
	}
	return o.Index < other.Index
}

type Order struct {
	ID      OrderID
	Account AccountID
	Side    Side
	Price   Price // limit price in ticks
	Qty     Qty   // remaining quantity in base units
}

// Fill is one match. Price is always the resting order's price: the taker gets
// the improvement, which is the standard rule and, more importantly here, one
// that does not depend on anything outside the two orders.
type Fill struct {
	TakerID      OrderID
	MakerID      OrderID
	TakerAccount AccountID
	MakerAccount AccountID
	TakerSide    Side
	Price        Price
	Qty          Qty
}
