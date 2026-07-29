package chain

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"onchain-orderbook/orderbook"
)

// The claim this whole repository makes is that the state transition is
// deterministic. A claim like that is worth nothing asserted and everything
// demonstrated, so these tests try to break it the ways it actually breaks.

func acct(n byte) orderbook.AccountID {
	var a orderbook.AccountID
	a[0] = n
	return a
}

func funded(mode Mode, accounts int) *Engine {
	e := NewEngine(mode)
	for i := 1; i <= accounts; i++ {
		e.State.Credit(acct(byte(i)), 1_000_000, 1_000_000_000)
	}
	return e
}

// A block with enough shape to exercise crossing, resting, partial fills and
// rejections all at once.
func sampleBlock() []Tx {
	return []Tx{
		{Kind: TxPlace, Account: acct(1), Side: orderbook.Sell, Price: 101, Qty: 50},
		{Kind: TxPlace, Account: acct(2), Side: orderbook.Sell, Price: 100, Qty: 30},
		{Kind: TxPlace, Account: acct(3), Side: orderbook.Buy, Price: 102, Qty: 60},
		{Kind: TxPlace, Account: acct(4), Side: orderbook.Buy, Price: 99, Qty: 40},
		{Kind: TxPlace, Account: acct(5), Side: orderbook.Sell, Price: 103, Qty: 25},
		{Kind: TxPlace, Account: acct(2), Side: orderbook.Buy, Price: 104, Qty: 10},
	}
}

// Two validators, same input, same answer. If this ever fails the chain halts.
func TestIndependentNodesAgree(t *testing.T) {
	for _, mode := range []Mode{Continuous, BatchAuction} {
		t.Run(mode.String(), func(t *testing.T) {
			const nodes = 8
			var first BlockResult
			for n := 0; n < nodes; n++ {
				e := funded(mode, 6)
				got := e.ApplyBlock(7, sampleBlock())
				if n == 0 {
					first = got
					continue
				}
				if got.Root != first.Root {
					t.Fatalf("node %d root %x != node 0 root %x", n, got.Root, first.Root)
				}
				if len(got.Fills) != len(first.Fills) {
					t.Fatalf("node %d produced %d fills, node 0 produced %d",
						n, len(got.Fills), len(first.Fills))
				}
				for i := range got.Fills {
					if got.Fills[i] != first.Fills[i] {
						t.Fatalf("node %d fill %d differs:\n got %+v\nwant %+v",
							n, i, got.Fills[i], first.Fills[i])
					}
				}
			}
			if len(first.Fills) == 0 {
				t.Fatal("sample block produced no fills; the test proves nothing")
			}
		})
	}
}

// One node, same input, many runs. This is the check that catches map-order
// dependence: Go randomises map iteration per run, so a root computed from a
// map walk drifts here even though the input never changed.
func TestSameNodeIsStableAcrossRuns(t *testing.T) {
	for _, mode := range []Mode{Continuous, BatchAuction} {
		t.Run(mode.String(), func(t *testing.T) {
			var want [32]byte
			for run := 0; run < 200; run++ {
				e := funded(mode, 6)
				got := e.ApplyBlock(7, sampleBlock()).Root
				if run == 0 {
					want = got
					continue
				}
				if got != want {
					t.Fatalf("run %d root %x != run 0 root %x", run, got, want)
				}
			}
		})
	}
}

// Replaying a chain from genesis must land on the same state as having been
// online the whole time -- otherwise a new node can never sync.
func TestReplayFromGenesisMatchesLiveNode(t *testing.T) {
	blocks := [][]Tx{
		sampleBlock(),
		{
			{Kind: TxPlace, Account: acct(4), Side: orderbook.Buy, Price: 105, Qty: 35},
			{Kind: TxPlace, Account: acct(1), Side: orderbook.Sell, Price: 98, Qty: 20},
		},
		{
			{Kind: TxPlace, Account: acct(6), Side: orderbook.Buy, Price: 97, Qty: 15},
			{Kind: TxPlace, Account: acct(5), Side: orderbook.Sell, Price: 106, Qty: 12},
		},
	}

	live := funded(Continuous, 6)
	var liveRoot [32]byte
	for h, b := range blocks {
		liveRoot = live.ApplyBlock(uint64(h+1), b).Root
	}

	replay := funded(Continuous, 6)
	var replayRoot [32]byte
	for h, b := range blocks {
		replayRoot = replay.ApplyBlock(uint64(h+1), b).Root
	}

	if liveRoot != replayRoot {
		t.Fatalf("replay root %x != live root %x", replayRoot, liveRoot)
	}
}

// Demonstrates *why* State.Root sorts before hashing, rather than asserting it.
// The naive version walks the account map directly; the real one sorts. Both
// commit to identical data.
func TestMapIterationWouldBreakConsensus(t *testing.T) {
	e := funded(Continuous, 8)
	e.ApplyBlock(1, sampleBlock())

	naiveRoot := func() [32]byte {
		h := sha256.New()
		var buf [8]byte
		for id, b := range e.State.accounts { // deliberately unsorted
			h.Write(id[:])
			for _, v := range []int64{b.Base, b.Quote, b.LockedBase, b.LockedQuote} {
				binary.BigEndian.PutUint64(buf[:], uint64(v))
				h.Write(buf[:])
			}
		}
		var out [32]byte
		copy(out[:], h.Sum(nil))
		return out
	}

	naive := make(map[[32]byte]int)
	sorted := make(map[[32]byte]int)
	for i := 0; i < 200; i++ {
		naive[naiveRoot()]++
		sorted[e.State.Root(e.Book)]++
	}

	if len(sorted) != 1 {
		t.Fatalf("the sorted root is supposed to be stable, saw %d distinct values", len(sorted))
	}
	if len(naive) == 1 {
		// Astronomically unlikely with 8 accounts over 200 iterations, but the
		// language only promises randomisation, it does not guarantee a
		// collision-free sample. Reporting beats a flaky failure.
		t.Skip("map iteration happened to be stable this run; the hazard is unproven here")
	}
	t.Logf("map-order root produced %d distinct values over 200 iterations; "+
		"sorted root produced 1", len(naive))
}

// A batch auction's defining property: where an order sits in the block must
// not change what it gets. Rotating the block is the cheapest way to try to
// break that, and it is exactly what a block producer can do for free.
func TestBatchAuctionIgnoresPositionInBlock(t *testing.T) {
	base := sampleBlock()

	// The property is that the *economics* do not depend on position: the
	// clearing price, the volume, and what each account ends up with. The state
	// root deliberately is NOT compared here -- OrderID encodes an order's index
	// within the block, so rotating the block legitimately renames every resting
	// order and moves the root. Asserting root equality would be asserting that
	// identifiers are position-independent, which they are not and should not be.
	var wantPrice orderbook.Price
	var wantVolume orderbook.Qty
	perAccount := map[orderbook.AccountID]orderbook.Qty{}

	for rot := 0; rot < len(base); rot++ {
		rotated := make([]Tx, 0, len(base))
		rotated = append(rotated, base[rot:]...)
		rotated = append(rotated, base[:rot]...)

		e := funded(BatchAuction, 6)
		got := e.ApplyBlock(7, rotated)

		filled := map[orderbook.AccountID]orderbook.Qty{}
		for _, f := range got.Fills {
			filled[f.TakerAccount] += f.Qty
			filled[f.MakerAccount] += f.Qty
		}

		if rot == 0 {
			wantPrice, wantVolume = got.ClearingPrice, got.Volume
			perAccount = filled
			if wantVolume == 0 {
				t.Fatal("auction cleared no volume; the test proves nothing")
			}
			continue
		}
		if got.ClearingPrice != wantPrice {
			t.Fatalf("rotation %d cleared at %d, want %d", rot, got.ClearingPrice, wantPrice)
		}
		if got.Volume != wantVolume {
			t.Fatalf("rotation %d volume %d, want %d", rot, got.Volume, wantVolume)
		}
		for a, q := range perAccount {
			if filled[a] != q {
				t.Fatalf("rotation %d: account %d filled %d, want %d — the block "+
					"producer changed who got filled by reordering, which is the "+
					"advantage a uniform price is supposed to remove",
					rot, a[0], filled[a], q)
			}
		}
		if len(filled) != len(perAccount) {
			t.Fatalf("rotation %d filled %d accounts, want %d", rot, len(filled), len(perAccount))
		}
	}
}

// The contrast that justifies the batch mode existing at all: under continuous
// matching, moving one order to the front of the block changes who gets filled.
// That difference is the block producer's to sell, and it is what the auction
// removes.
func TestContinuousMatchingIsPositionSensitive(t *testing.T) {
	book := []Tx{
		{Kind: TxPlace, Account: acct(1), Side: orderbook.Sell, Price: 100, Qty: 10},
	}
	racer := Tx{Kind: TxPlace, Account: acct(2), Side: orderbook.Buy, Price: 100, Qty: 10}
	rival := Tx{Kind: TxPlace, Account: acct(3), Side: orderbook.Buy, Price: 100, Qty: 10}

	fillsFor := func(order []Tx) map[byte]orderbook.Qty {
		e := funded(Continuous, 4)
		e.ApplyBlock(1, book)
		got := e.ApplyBlock(2, order)
		out := map[byte]orderbook.Qty{}
		for _, f := range got.Fills {
			out[f.TakerAccount[0]] += f.Qty
		}
		return out
	}

	first := fillsFor([]Tx{racer, rival})
	second := fillsFor([]Tx{rival, racer})

	if first[2] == second[2] {
		t.Fatalf("expected position to decide the fill under continuous matching, "+
			"got identical outcomes: %v vs %v", first, second)
	}
	if first[2] != 10 || second[3] != 10 {
		t.Fatalf("whoever is first should take the whole resting order: %v / %v", first, second)
	}
}
