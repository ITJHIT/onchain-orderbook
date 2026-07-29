package orderbook

import "sort"

// level is one price point. Orders within it are in arrival order, which is
// insertion order into a slice -- not a map, and not a heap whose sift order
// could depend on capacity growth.
type level struct {
	price  Price
	orders []Order
}

// Book is a price-time-priority limit book.
//
// Levels are held in slices sorted so that index 0 is always the best price:
// bids descending, asks ascending. A map keyed by price would be the obvious
// structure and is the classic way an on-chain book goes non-deterministic --
// Go randomises map iteration on purpose, so the sweep order would differ
// between validators and between runs.
//
// The index from OrderID to location is a map, and that is safe: it is only
// ever read by key, never iterated. The rule is not "no maps", it is "nothing
// that affects output may depend on map order".
type Book struct {
	bids []level
	asks []level
	idx  map[OrderID]struct{}
}

func NewBook() *Book {
	return &Book{idx: make(map[OrderID]struct{})}
}

func (b *Book) sideLevels(s Side) *[]level {
	if s == Buy {
		return &b.bids
	}
	return &b.asks
}

// better reports whether price a comes before price b on the given side.
func better(s Side, a, b Price) bool {
	if s == Buy {
		return a > b
	}
	return a < b
}

// BestBid returns the highest bid price and whether one exists.
func (b *Book) BestBid() (Price, bool) {
	if len(b.bids) == 0 {
		return 0, false
	}
	return b.bids[0].price, true
}

// BestAsk returns the lowest ask price and whether one exists.
func (b *Book) BestAsk() (Price, bool) {
	if len(b.asks) == 0 {
		return 0, false
	}
	return b.asks[0].price, true
}

// Depth returns the resting quantity at a price on one side.
func (b *Book) Depth(s Side, p Price) Qty {
	levels := *b.sideLevels(s)
	for i := range levels {
		if levels[i].price == p {
			var total Qty
			for _, o := range levels[i].orders {
				total += o.Qty
			}
			return total
		}
	}
	return 0
}

// Len is the number of resting orders.
func (b *Book) Len() int { return len(b.idx) }

// rest inserts an order, keeping the level slice sorted best-first.
func (b *Book) rest(o Order) {
	if o.Qty <= 0 {
		return
	}
	levels := b.sideLevels(o.Side)
	pos := sort.Search(len(*levels), func(i int) bool {
		return !better(o.Side, (*levels)[i].price, o.Price)
	})
	if pos < len(*levels) && (*levels)[pos].price == o.Price {
		(*levels)[pos].orders = append((*levels)[pos].orders, o)
		b.idx[o.ID] = struct{}{}
		return
	}
	*levels = append(*levels, level{})
	copy((*levels)[pos+1:], (*levels)[pos:])
	(*levels)[pos] = level{price: o.Price, orders: []Order{o}}
	b.idx[o.ID] = struct{}{}
}

// Cancel removes a resting order. Returns the quantity removed and whether the
// order was found.
func (b *Book) Cancel(id OrderID) (Qty, bool) {
	if _, ok := b.idx[id]; !ok {
		return 0, false
	}
	for _, side := range []Side{Buy, Sell} {
		levels := b.sideLevels(side)
		for li := range *levels {
			orders := (*levels)[li].orders
			for oi := range orders {
				if orders[oi].ID != id {
					continue
				}
				q := orders[oi].Qty
				(*levels)[li].orders = append(orders[:oi], orders[oi+1:]...)
				if len((*levels)[li].orders) == 0 {
					*levels = append((*levels)[:li], (*levels)[li+1:]...)
				}
				delete(b.idx, id)
				return q, true
			}
		}
	}
	return 0, false
}

// crosses reports whether a taker at takerPx would trade against restPx.
func crosses(takerSide Side, takerPx, restPx Price) bool {
	if takerSide == Buy {
		return takerPx >= restPx
	}
	return takerPx <= restPx
}

// Submit matches a taker against the book and rests any remainder, appending
// fills to out. Returns the total quantity filled.
//
// maxLevels bounds how many price levels one order may sweep. On-chain this is
// not a tuning knob -- an unbounded sweep is a denial-of-service vector, since
// one order against a book of ten thousand dust levels would blow the block's
// compute budget. Hitting the bound stops matching and rests the remainder;
// the order is not rejected, because rejecting would let an attacker who seeds
// dust levels also decide whose orders fail.
func (b *Book) Submit(taker Order, maxLevels int, out *[]Fill) Qty {
	if taker.Qty <= 0 {
		return 0
	}
	var filled Qty
	opposite := b.sideLevels(taker.Side.Opposite())
	swept := 0

	for taker.Qty > 0 && len(*opposite) > 0 {
		if maxLevels > 0 && swept >= maxLevels {
			break
		}
		lv := &(*opposite)[0]
		if !crosses(taker.Side, taker.Price, lv.price) {
			break
		}
		for taker.Qty > 0 && len(lv.orders) > 0 {
			maker := &lv.orders[0]
			q := maker.Qty
			if taker.Qty < q {
				q = taker.Qty
			}
			*out = append(*out, Fill{
				TakerID:      taker.ID,
				MakerID:      maker.ID,
				TakerAccount: taker.Account,
				MakerAccount: maker.Account,
				TakerSide:    taker.Side,
				Price:        lv.price, // always the maker's price
				Qty:          q,
			})
			maker.Qty -= q
			taker.Qty -= q
			filled += q
			if maker.Qty == 0 {
				delete(b.idx, maker.ID)
				lv.orders = lv.orders[1:]
			}
		}
		if len(lv.orders) == 0 {
			*opposite = (*opposite)[1:]
			swept++
		}
	}

	b.rest(taker)
	return filled
}

// Snapshot returns the book in canonical order: bids best-first then asks
// best-first, orders within a level in arrival order. This is what the state
// root commits to, so it must not depend on anything but the book's contents.
func (b *Book) Snapshot() []Order {
	out := make([]Order, 0, len(b.idx))
	for _, levels := range [][]level{b.bids, b.asks} {
		for _, lv := range levels {
			out = append(out, lv.orders...)
		}
	}
	return out
}
