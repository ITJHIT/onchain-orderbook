package chain

import "onchain-orderbook/orderbook"

// Mode selects how a block's orders are matched.
type Mode uint8

const (
	// Continuous matches each order against the book as it is applied. Lowest
	// latency to execution, and it makes position within the block worth money
	// -- which is the block producer's to sell.
	Continuous Mode = iota
	// BatchAuction collects the block's orders and clears them at one uniform
	// price. Position within the block is worth nothing.
	BatchAuction
)

func (m Mode) String() string {
	if m == BatchAuction {
		return "batch-auction"
	}
	return "continuous"
}

type TxKind uint8

const (
	TxPlace TxKind = iota
	TxCancel
)

// Tx is one transaction in a block. Deliberately a flat struct rather than an
// interface: an interface would let the concrete type influence behaviour in
// ways that are hard to prove identical across builds.
type Tx struct {
	Kind    TxKind
	Account orderbook.AccountID
	Side    orderbook.Side
	Price   orderbook.Price
	Qty     orderbook.Qty
	Cancel  orderbook.OrderID
}

// TxResult records what happened to one transaction, including why it failed.
// Rejections are part of the state transition and must be identical on every
// node, so they are returned rather than logged.
type TxResult struct {
	Index    uint32
	Accepted bool
	Err      error
	OrderID  orderbook.OrderID
	Filled   orderbook.Qty
}

// BlockResult is the full, deterministic outcome of applying one block.
type BlockResult struct {
	Height        uint64
	Mode          Mode
	Results       []TxResult
	Fills         []orderbook.Fill
	ClearingPrice orderbook.Price // BatchAuction only
	Volume        orderbook.Qty   // BatchAuction only
	Root          [32]byte
}

// Engine is one node's view: a book, balances, and the rules connecting them.
type Engine struct {
	Book  *orderbook.Book
	State *State
	Mode  Mode
	// MaxSweepLevels bounds a single order's sweep. Zero means unbounded, which
	// is only appropriate off-chain.
	MaxSweepLevels int
}

func NewEngine(mode Mode) *Engine {
	return &Engine{
		Book:           orderbook.NewBook(),
		State:          NewState(),
		Mode:           mode,
		MaxSweepLevels: 64,
	}
}

// ApplyBlock is the state transition function. Given the same starting state
// and the same ordered txs it must produce the same fills and the same root on
// every node, on every run, forever. Everything else in this repository exists
// to make that sentence true.
func (e *Engine) ApplyBlock(height uint64, txs []Tx) BlockResult {
	res := BlockResult{Height: height, Mode: e.Mode}
	res.Results = make([]TxResult, 0, len(txs))

	if e.Mode == BatchAuction {
		e.applyBatch(height, txs, &res)
	} else {
		e.applyContinuous(height, txs, &res)
	}

	res.Root = e.State.Root(e.Book)
	return res
}

func (e *Engine) applyContinuous(height uint64, txs []Tx, res *BlockResult) {
	for i, tx := range txs {
		idx := uint32(i)
		if tx.Kind == TxCancel {
			res.Results = append(res.Results, e.doCancel(idx, tx))
			continue
		}

		o := orderbook.Order{
			ID:      orderbook.OrderID{Height: height, Index: idx},
			Account: tx.Account,
			Side:    tx.Side,
			Price:   tx.Price,
			Qty:     tx.Qty,
		}
		if err := e.State.lockFor(o); err != nil {
			res.Results = append(res.Results, TxResult{Index: idx, Err: err})
			continue
		}

		before := len(res.Fills)
		filled := e.Book.Submit(o, e.MaxSweepLevels, &res.Fills)
		for _, f := range res.Fills[before:] {
			if err := e.State.settle(f, o.Price); err != nil {
				// Settlement cannot fail once funds are locked; if it does the
				// invariant is broken and continuing would corrupt every later
				// balance. Surface it rather than press on.
				res.Results = append(res.Results, TxResult{Index: idx, Err: err})
				return
			}
		}
		res.Results = append(res.Results, TxResult{
			Index: idx, Accepted: true, OrderID: o.ID, Filled: filled,
		})
	}
}

func (e *Engine) doCancel(idx uint32, tx Tx) TxResult {
	// Find the resting order first so the unlock uses its real side and price.
	var target orderbook.Order
	found := false
	for _, o := range e.Book.Snapshot() {
		if o.ID == tx.Cancel {
			target, found = o, true
			break
		}
	}
	if !found {
		return TxResult{Index: idx, Err: ErrBadOrder}
	}
	if target.Account != tx.Account {
		return TxResult{Index: idx, Err: ErrBadOrder} // not yours to cancel
	}
	q, ok := e.Book.Cancel(tx.Cancel)
	if !ok {
		return TxResult{Index: idx, Err: ErrBadOrder}
	}
	if err := e.State.unlock(target, q); err != nil {
		return TxResult{Index: idx, Err: err}
	}
	return TxResult{Index: idx, Accepted: true, OrderID: tx.Cancel}
}

// applyBatch clears every placement in the block at a single price.
//
// Cancels are processed first and in order: they refer to orders resting from
// earlier blocks, and letting them settle before the auction means a trader can
// actually withdraw a quote in the same block, which is the whole reason a
// batch design is tolerable to a market maker.
func (e *Engine) applyBatch(height uint64, txs []Tx, res *BlockResult) {
	for i, tx := range txs {
		if tx.Kind == TxCancel {
			res.Results = append(res.Results, e.doCancel(uint32(i), tx))
		}
	}

	batch := make([]orderbook.Order, 0, len(txs))
	limits := make(map[orderbook.OrderID]orderbook.Order, len(txs))
	for i, tx := range txs {
		if tx.Kind != TxPlace {
			continue
		}
		idx := uint32(i)
		o := orderbook.Order{
			ID:      orderbook.OrderID{Height: height, Index: idx},
			Account: tx.Account,
			Side:    tx.Side,
			Price:   tx.Price,
			Qty:     tx.Qty,
		}
		if err := e.State.lockFor(o); err != nil {
			res.Results = append(res.Results, TxResult{Index: idx, Err: err})
			continue
		}
		batch = append(batch, o)
		limits[o.ID] = o
		res.Results = append(res.Results, TxResult{Index: idx, Accepted: true, OrderID: o.ID})
	}

	auction := orderbook.Clear(batch)
	if !auction.Cleared {
		// Nothing crossed. Every accepted order rests, keeping its lock.
		for _, o := range batch {
			e.restLeftover(o, 0)
		}
		return
	}
	res.ClearingPrice = auction.Price
	res.Volume = auction.Volume

	// Pair buyers with sellers deterministically: both allocation lists are
	// already in ascending OrderID, so walking them together is reproducible.
	// Every unit clears at the auction price, so who is matched with whom does
	// not change anyone's economics -- only the bookkeeping.
	var buys, sells []orderbook.Allocation
	for _, a := range auction.Allocations {
		if a.Side == orderbook.Buy {
			buys = append(buys, a)
		} else {
			sells = append(sells, a)
		}
	}

	filled := make(map[orderbook.OrderID]orderbook.Qty, len(batch))
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
		res.Fills = append(res.Fills, f)
		if err := e.State.settle(f, limits[buys[bi].ID].Price); err != nil {
			return
		}
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

	for _, o := range batch {
		e.restLeftover(o, filled[o.ID])
	}
	for i := range res.Results {
		if res.Results[i].Accepted && res.Results[i].OrderID.Height == height {
			res.Results[i].Filled = filled[res.Results[i].OrderID]
		}
	}
}

// restLeftover puts an order's unfilled remainder on the book, keeping its lock.
func (e *Engine) restLeftover(o orderbook.Order, done orderbook.Qty) {
	rem := o.Qty - done
	if rem <= 0 {
		return
	}
	o.Qty = rem
	var ignored []orderbook.Fill
	e.Book.Submit(o, e.MaxSweepLevels, &ignored)
}
