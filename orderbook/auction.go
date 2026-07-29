package orderbook

import "sort"

// Frequent batch auction: every order in a block clears at one uniform price.
//
// Why a chain wants this. In continuous matching, the order that arrives first
// takes the best resting price, so value accrues to whoever can get bytes in
// front of everyone else's. On a chain "first" is decided by whoever orders the
// block -- so continuous matching hands the block producer a free option on
// every trade, which is what MEV extraction is. A batch auction removes the
// prize rather than policing it: within a batch, arrival order changes nothing,
// because everyone clears at the same price.
//
// It is not free. A batch auction gives up continuous execution, and the
// rationing rule becomes consensus-critical: when one side is oversubscribed at
// the clearing price, *who* gets filled has to be decided by a rule every
// validator computes identically. That rule is the interesting part, and it is
// where integer arithmetic stops being a preference and becomes the design.

// Allocation is one participant's fill in a batch.
type Allocation struct {
	ID      OrderID
	Account AccountID
	Side    Side
	Qty     Qty
}

// AuctionResult is the outcome of clearing one batch.
type AuctionResult struct {
	Cleared     bool
	Price       Price
	Volume      Qty // matched quantity on each side
	Allocations []Allocation
}

// cumulative demand at p: every buy willing to pay at least p.
func demandAt(orders []Order, p Price) Qty {
	var q Qty
	for _, o := range orders {
		if o.Side == Buy && o.Price >= p {
			q += o.Qty
		}
	}
	return q
}

// cumulative supply at p: every sell willing to accept at most p.
func supplyAt(orders []Order, p Price) Qty {
	var q Qty
	for _, o := range orders {
		if o.Side == Sell && o.Price <= p {
			q += o.Qty
		}
	}
	return q
}

func abs64(v Qty) Qty {
	if v < 0 {
		return -v
	}
	return v
}

// Clear runs a uniform-price auction over `orders` and returns the allocations.
//
// The input slice is not mutated and its order does not affect the result --
// that is asserted by the determinism tests, because "the answer must not depend
// on the order the transactions happened to be listed in" is the entire point.
func Clear(orders []Order) AuctionResult {
	// Candidate prices are the limit prices present. The clearing price is
	// always one of them: volume only changes where the curves step.
	seen := make(map[Price]struct{}, len(orders))
	candidates := make([]Price, 0, len(orders))
	for _, o := range orders {
		if o.Qty <= 0 {
			continue
		}
		if _, ok := seen[o.Price]; !ok {
			seen[o.Price] = struct{}{}
			candidates = append(candidates, o.Price)
		}
	}
	// Sorting is what makes the candidate set order-independent. It is built
	// from a map, so without this the sweep below would inherit Go's randomised
	// map order and pick a different price on ties, on every run.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i] < candidates[j] })

	var (
		best      Price
		bestVol   Qty
		bestImbal Qty
		found     bool
	)
	for _, p := range candidates {
		d, s := demandAt(orders, p), supplyAt(orders, p)
		vol := d
		if s < vol {
			vol = s
		}
		if vol <= 0 {
			continue
		}
		imbal := abs64(d - s)
		// Tie-breaks, in order and all deterministic: most volume, then least
		// imbalance, then the lowest price. The last one is arbitrary but it
		// must exist -- without a total order two validators can legitimately
		// compute different prices from identical input.
		switch {
		case !found, vol > bestVol,
			vol == bestVol && imbal < bestImbal,
			vol == bestVol && imbal == bestImbal && p < best:
			best, bestVol, bestImbal, found = p, vol, imbal, true
		}
	}
	if !found {
		return AuctionResult{}
	}

	res := AuctionResult{Cleared: true, Price: best, Volume: bestVol}
	res.Allocations = append(res.Allocations, ration(orders, Buy, best, bestVol)...)
	res.Allocations = append(res.Allocations, ration(orders, Sell, best, bestVol)...)
	return res
}

// ration distributes `volume` across the eligible orders on one side.
//
// Pro-rata by size, floored, with the remainder handed out one unit at a time
// in ascending OrderID. The floor-then-distribute shape is deliberate: the
// obvious `qty * volume / total` rounding loses units, and an auction that
// allocates 99 of 100 units has silently created a unit of imbalance that the
// settlement layer will later discover as a missing balance. Every unit is
// accounted for here or the function is wrong.
func ration(orders []Order, side Side, price Price, volume Qty) []Allocation {
	eligible := make([]Order, 0, len(orders))
	var total Qty
	for _, o := range orders {
		if o.Qty <= 0 || o.Side != side {
			continue
		}
		if side == Buy && o.Price < price {
			continue
		}
		if side == Sell && o.Price > price {
			continue
		}
		eligible = append(eligible, o)
		total += o.Qty
	}
	if total == 0 || volume <= 0 {
		return nil
	}
	// Canonical order for both the pro-rata pass and the remainder pass.
	//
	// Account first, and deliberately NOT OrderID first. OrderID encodes the
	// order's index within the block, so ranking by it would hand the remainder
	// units to whoever the block producer chose to list earliest -- reintroducing,
	// in the rationing rule, exactly the positional advantage the uniform price
	// was meant to abolish. It is only a unit or two per auction, which is
	// precisely why it would have survived review: too small to notice, and free
	// to the producer, forever. Account, price and quantity are all fixed before
	// the block is assembled; OrderID remains only as a final tie-break between
	// two economically identical orders from the same account, where the choice
	// changes nobody's position.
	sort.Slice(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		for k := 0; k < len(a.Account); k++ {
			if a.Account[k] != b.Account[k] {
				return a.Account[k] < b.Account[k]
			}
		}
		if a.Price != b.Price {
			return a.Price < b.Price
		}
		if a.Qty != b.Qty {
			return a.Qty < b.Qty
		}
		return a.ID.Less(b.ID)
	})

	out := make([]Allocation, len(eligible))
	var handed Qty
	for i, o := range eligible {
		q := o.Qty * volume / total // integer floor
		if q > o.Qty {
			q = o.Qty
		}
		out[i] = Allocation{ID: o.ID, Account: o.Account, Side: side, Qty: q}
		handed += q
	}
	// Hand out what flooring dropped, one unit per order, ascending ID, looping
	// until exhausted. Bounded: each pass gives away at least one unit while any
	// order still has headroom, and `handed` only rises.
	for handed < volume {
		progressed := false
		for i := range out {
			if handed == volume {
				break
			}
			if out[i].Qty < eligible[i].Qty {
				out[i].Qty++
				handed++
				progressed = true
			}
		}
		if !progressed {
			break // every eligible order is full; cannot happen if volume <= total
		}
	}

	// Drop zero allocations so the result contains only real fills.
	trimmed := out[:0]
	for _, a := range out {
		if a.Qty > 0 {
			trimmed = append(trimmed, a)
		}
	}
	return trimmed
}
