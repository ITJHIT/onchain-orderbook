// Package chain applies ordered blocks of orders to replicated state.
//
// The book decides who trades. This decides what that *means* for balances, and
// commits the result to a hash every validator can compare. If two nodes ever
// disagree about a single unit here, the chain halts -- so every operation is
// integer, every iteration is over a sorted slice, and the commitment covers
// everything that could differ.
package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"

	"onchain-orderbook/orderbook"
)

var (
	ErrInsufficientFunds = errors.New("insufficient unlocked balance")
	ErrBadOrder          = errors.New("non-positive price or quantity")
	ErrOverflow          = errors.New("notional overflows int64")
)

// Balance holds one account's holdings in a base/quote pair.
//
// Locked funds are the part committed to resting orders. Keeping them as a
// separate field rather than deducting at placement means a cancel is exact and
// the invariant "free + locked never changes except through a fill" is directly
// checkable -- and it is checked, in the tests.
type Balance struct {
	Base        int64
	Quote       int64
	LockedBase  int64
	LockedQuote int64
}

type State struct {
	accounts map[orderbook.AccountID]*Balance
}

func NewState() *State {
	return &State{accounts: make(map[orderbook.AccountID]*Balance)}
}

func (s *State) Balance(a orderbook.AccountID) Balance {
	if b, ok := s.accounts[a]; ok {
		return *b
	}
	return Balance{}
}

func (s *State) Credit(a orderbook.AccountID, base, quote int64) {
	b := s.accounts[a]
	if b == nil {
		b = &Balance{}
		s.accounts[a] = b
	}
	b.Base += base
	b.Quote += quote
}

// notional returns price*qty, refusing to wrap. A silent overflow here would
// mint value out of nothing, so it is an error rather than a saturating clamp:
// there is no sensible "close enough" for a balance.
func notional(price orderbook.Price, qty orderbook.Qty) (int64, error) {
	if price == 0 || qty == 0 {
		return 0, nil
	}
	n := price * qty
	if n/price != qty {
		return 0, ErrOverflow
	}
	return n, nil
}

// lockFor reserves the funds a new order commits: a buy locks quote at its
// limit price, a sell locks base.
func (s *State) lockFor(o orderbook.Order) error {
	if o.Price <= 0 || o.Qty <= 0 {
		return ErrBadOrder
	}
	b := s.accounts[o.Account]
	if b == nil {
		return ErrInsufficientFunds
	}
	if o.Side == orderbook.Sell {
		if b.Base-b.LockedBase < o.Qty {
			return ErrInsufficientFunds
		}
		b.LockedBase += o.Qty
		return nil
	}
	need, err := notional(o.Price, o.Qty)
	if err != nil {
		return err
	}
	if b.Quote-b.LockedQuote < need {
		return ErrInsufficientFunds
	}
	b.LockedQuote += need
	return nil
}

func (s *State) unlock(o orderbook.Order, qty orderbook.Qty) error {
	b := s.accounts[o.Account]
	if b == nil || qty <= 0 {
		return nil
	}
	if o.Side == orderbook.Sell {
		b.LockedBase -= qty
		return nil
	}
	amt, err := notional(o.Price, qty)
	if err != nil {
		return err
	}
	b.LockedQuote -= amt
	return nil
}

// settle moves value for one fill.
//
// The buyer locked quote at *its own* limit price but pays the maker's price,
// which for a crossing buy is never worse. The difference is released back --
// forgetting that refund is a classic way for an exchange to slowly strand user
// funds in a lock that nothing ever clears.
func (s *State) settle(f orderbook.Fill, takerLimit orderbook.Price) error {
	paid, err := notional(f.Price, f.Qty)
	if err != nil {
		return err
	}

	var buyer, seller orderbook.AccountID
	var buyerLimit orderbook.Price
	if f.TakerSide == orderbook.Buy {
		buyer, seller = f.TakerAccount, f.MakerAccount
		buyerLimit = takerLimit
	} else {
		buyer, seller = f.MakerAccount, f.TakerAccount
		buyerLimit = f.Price // the maker's own resting price is what it locked
	}

	bb := s.accounts[buyer]
	sb := s.accounts[seller]
	if bb == nil || sb == nil {
		return ErrInsufficientFunds
	}

	locked, err := notional(buyerLimit, f.Qty)
	if err != nil {
		return err
	}
	bb.LockedQuote -= locked // release the whole reservation...
	bb.Quote -= paid         // ...and charge only what was actually paid
	bb.Base += f.Qty

	sb.LockedBase -= f.Qty
	sb.Base -= f.Qty
	sb.Quote += paid
	return nil
}

// Root commits to everything a validator could disagree about: every balance
// and every resting order, in a canonical order, at fixed width.
//
// Accounts come out of a map, so they are sorted before hashing. Skipping that
// would produce a root that changed on every run of the same node -- the single
// most common way a chain built on Go maps fails to reach consensus with itself.
func (s *State) Root(book *orderbook.Book) [32]byte {
	ids := make([]orderbook.AccountID, 0, len(s.accounts))
	for id := range s.accounts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		for k := 0; k < len(ids[i]); k++ {
			if ids[i][k] != ids[j][k] {
				return ids[i][k] < ids[j][k]
			}
		}
		return false
	})

	h := sha256.New()
	var buf [8]byte
	putI64 := func(v int64) {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		h.Write(buf[:])
	}

	h.Write([]byte("accounts"))
	for _, id := range ids {
		b := s.accounts[id]
		h.Write(id[:])
		putI64(b.Base)
		putI64(b.Quote)
		putI64(b.LockedBase)
		putI64(b.LockedQuote)
	}

	h.Write([]byte("book"))
	if book != nil {
		for _, o := range book.Snapshot() {
			h.Write(o.Account[:])
			putI64(int64(o.ID.Height))
			putI64(int64(o.ID.Index))
			putI64(int64(o.Side))
			putI64(o.Price)
			putI64(o.Qty)
		}
	}

	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
