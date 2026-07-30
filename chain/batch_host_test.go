package chain

import (
	"testing"

	"github.com/ITJHIT/onchain-orderbook/orderbook"
)

// A host stages every placement in a block, then clears once. These check
// that the staged path produces exactly what the all-at-once ApplyBlock batch
// path produces for the same orders -- StagePlace/ClearBatch is a
// decomposition of applyBatch, not a different algorithm, and it needs to stay
// that way or a host and the library disagree about what a block did.

func stageAll(e *Engine, height uint64, txs []Tx) []orderbook.Order {
	var staged []orderbook.Order
	for i, tx := range txs {
		if tx.Kind != TxPlace {
			continue
		}
		res, o := e.StagePlace(height, uint32(i), tx)
		if res.Accepted && o != nil {
			staged = append(staged, *o)
		}
	}
	return staged
}

func TestStageThenClearMatchesTheAllAtOnceBatchAPI(t *testing.T) {
	block := sampleBlock()

	whole := funded(BatchAuction, 6)
	want := whole.ApplyBlock(9, block)

	staged := funded(BatchAuction, 6)
	orders := stageAll(staged, 9, block)
	summary, err := staged.ClearBatch(orders)
	if err != nil {
		t.Fatalf("ClearBatch: %v", err)
	}

	if !summary.Cleared || summary.Price != want.ClearingPrice || summary.Volume != want.Volume {
		t.Fatalf("staged summary %+v, want price=%d volume=%d", summary, want.ClearingPrice, want.Volume)
	}
	if len(summary.Fills) != len(want.Fills) {
		t.Fatalf("staged produced %d fills, block API produced %d", len(summary.Fills), len(want.Fills))
	}
	for i := range summary.Fills {
		if summary.Fills[i] != want.Fills[i] {
			t.Fatalf("fill %d differs:\n got %+v\nwant %+v", i, summary.Fills[i], want.Fills[i])
		}
	}
	if got := staged.State.Root(staged.Book); got != want.Root {
		t.Fatalf("staged root %x != block API root %x", got, want.Root)
	}
}

// The property the whole feature exists for: staging in a different order
// must not change who gets filled or how much.
func TestStagedClearingIgnoresStagingOrder(t *testing.T) {
	block := sampleBlock()

	var wantPrice orderbook.Price
	var wantVolume orderbook.Qty
	perAccount := map[orderbook.AccountID]orderbook.Qty{}

	for rot := 0; rot < len(block); rot++ {
		rotated := append(append([]Tx{}, block[rot:]...), block[:rot]...)

		e := funded(BatchAuction, 6)
		orders := stageAll(e, 7, rotated)
		summary, err := e.ClearBatch(orders)
		if err != nil {
			t.Fatalf("rotation %d: ClearBatch: %v", rot, err)
		}

		filled := map[orderbook.AccountID]orderbook.Qty{}
		for _, f := range summary.Fills {
			filled[f.TakerAccount] += f.Qty
			filled[f.MakerAccount] += f.Qty
		}

		if rot == 0 {
			wantPrice, wantVolume = summary.Price, summary.Volume
			perAccount = filled
			if wantVolume == 0 {
				t.Fatal("auction cleared no volume; the test proves nothing")
			}
			continue
		}
		if summary.Price != wantPrice || summary.Volume != wantVolume {
			t.Fatalf("rotation %d: price=%d volume=%d, want price=%d volume=%d",
				rot, summary.Price, summary.Volume, wantPrice, wantVolume)
		}
		for a, q := range perAccount {
			if filled[a] != q {
				t.Fatalf("rotation %d: account %d filled %d, want %d", rot, a[0], filled[a], q)
			}
		}
	}
}

func TestClearBatchOnEmptyStageClearsNothing(t *testing.T) {
	e := funded(BatchAuction, 3)
	summary, err := e.ClearBatch(nil)
	if err != nil {
		t.Fatalf("ClearBatch(nil): %v", err)
	}
	if summary.Cleared {
		t.Fatalf("expected an uncleared summary for an empty stage, got %+v", summary)
	}
}

func TestStagePlaceRejectsAnythingThatIsNotAPlacement(t *testing.T) {
	e := funded(BatchAuction, 3)
	res, o := e.StagePlace(1, 0, Tx{Kind: TxCancel, Account: acct(1)})
	if res.Accepted || o != nil {
		t.Fatalf("StagePlace accepted a cancel: %+v, %v", res, o)
	}
}

func TestStagePlaceLocksFundsImmediatelyEvenBeforeClearing(t *testing.T) {
	e := funded(BatchAuction, 3)
	res, o := e.StagePlace(1, 0, Tx{Kind: TxPlace, Account: acct(1), Side: orderbook.Sell, Price: 100, Qty: 30})
	if !res.Accepted || o == nil {
		t.Fatalf("stage rejected: %+v", res)
	}
	if b := e.State.Balance(acct(1)); b.LockedBase != 30 {
		t.Fatalf("locked base = %d, want 30 -- funds must be committed at stage time, "+
			"not at clear time, or a trader could stage more than they can cover", b.LockedBase)
	}
}
