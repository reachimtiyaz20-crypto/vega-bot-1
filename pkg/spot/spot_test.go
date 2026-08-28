package spot

import (
	"path/filepath"
	"testing"
	"time"
)

var now = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// THE DISTINCTION THIS PACKAGE EXISTS TO PROTECT
//
// "We did not check" and "there is no market" are different answers, and
// collapsing them routes a tradeable coin into the worse structure while
// looking like a decision. Every test below is really about that.
func TestUnknownIsNotAbsent(t *testing.T) {
	tbl := NewTable(now)

	// Never scanned.
	if got := tbl.Spot("binance", "BTC"); got != Unknown {
		t.Fatalf("unscanned venue = %v, want unknown", got)
	}
	if tbl.CanCashAndCarry("binance", "BTC") {
		t.Fatal("unscanned venue claimed cash-and-carry is possible")
	}

	// Scanned and failed: still unknown, even though we hold no markets.
	tbl.SetResult(VenueResult{Venue: "binance", OK: false, Err: "status 451"})
	if got := tbl.Spot("binance", "BTC"); got != Unknown {
		t.Fatalf("failed scan = %v, want unknown -- a failed fetch is not an absent market", got)
	}

	// Scanned successfully, coin genuinely not listed: now Absent is a fact.
	tbl.SetResult(VenueResult{Venue: "okx", OK: true, Count: 1})
	tbl.Put(Market{Venue: "okx", Coin: "BTC", Symbol: "BTC-USDT", Quote: "USDT"})
	if got := tbl.Spot("okx", "SOMEALT"); got != Absent {
		t.Fatalf("successful scan, unlisted coin = %v, want absent", got)
	}
	if got := tbl.Spot("okx", "BTC"); got != Present {
		t.Fatalf("listed coin = %v, want present", got)
	}
}

// A nil table must answer Unknown rather than panic or claim absence: it is
// what every caller holds before the first scan has ever run.
func TestNilTableIsUnknownNotAbsent(t *testing.T) {
	var tbl *Table
	if got := tbl.Spot("binance", "BTC"); got != Unknown {
		t.Fatalf("nil table = %v, want unknown", got)
	}
	if tbl.CanCashAndCarry("binance", "BTC") {
		t.Fatal("nil table claimed cash-and-carry is possible")
	}
	if tbl.CanReverseCarry("binance", "BTC") {
		t.Fatal("nil table claimed reverse carry is possible")
	}
	if v := tbl.VenuesWithSpot("BTC"); v != nil {
		t.Fatalf("nil table listed venues: %v", v)
	}
}

// Cash-and-carry needs spot only. Reverse carry needs borrow, and errs CLOSED
// when borrow was never checked: an unborrowable short-spot leg fails at
// execution, not at assessment.
func TestReverseCarryNeedsCheckedBorrow(t *testing.T) {
	tbl := NewTable(now)
	tbl.SetResult(VenueResult{Venue: "bybit", OK: true, Count: 1})
	tbl.Put(Market{Venue: "bybit", Coin: "ETH", Symbol: "ETHUSDT", Quote: "USDT"})

	if !tbl.CanCashAndCarry("bybit", "ETH") {
		t.Fatal("spot present but cash-and-carry refused")
	}
	if tbl.CanReverseCarry("bybit", "ETH") {
		t.Fatal("reverse carry allowed with borrow unchecked")
	}

	if !tbl.ApplyBorrow("bybit", "ETH", true, 1.98, 2000) {
		t.Fatal("ApplyBorrow rejected a coin that has a spot market")
	}
	if !tbl.CanReverseCarry("bybit", "ETH") {
		t.Fatal("reverse carry refused after borrow confirmed")
	}

	// Explicitly not borrowable is a refusal, not a gap.
	tbl.ApplyBorrow("bybit", "ETH", false, 0, 0)
	if tbl.CanReverseCarry("bybit", "ETH") {
		t.Fatal("reverse carry allowed for an explicitly unborrowable coin")
	}

	// Borrow without a spot market is not actionable.
	if tbl.ApplyBorrow("bybit", "NOTLISTED", true, 5, 100) {
		t.Fatal("ApplyBorrow accepted a coin with no spot market")
	}
}

func TestVenuesWithSpotSkipsFailedScans(t *testing.T) {
	tbl := NewTable(now)
	tbl.SetResult(VenueResult{Venue: "binance", OK: true, Count: 1})
	tbl.Put(Market{Venue: "binance", Coin: "SOL", Symbol: "SOLUSDT", Quote: "USDT"})

	// okx holds a stale market from an earlier run but its scan failed now.
	tbl.Put(Market{Venue: "okx", Coin: "SOL", Symbol: "SOL-USDT", Quote: "USDT"})
	tbl.SetResult(VenueResult{Venue: "okx", OK: false, Err: "timeout"})

	got := tbl.VenuesWithSpot("SOL")
	if len(got) != 1 || got[0] != "binance" {
		t.Fatalf("venues = %v, want [binance] -- a failed scan must not be reported as confirmed", got)
	}
}

func TestCoinLookupIsCaseInsensitive(t *testing.T) {
	tbl := NewTable(now)
	tbl.SetResult(VenueResult{Venue: "binance", OK: true, Count: 1})
	tbl.Put(Market{Venue: "binance", Coin: "btc", Symbol: "BTCUSDT", Quote: "USDT"})
	if tbl.Spot("binance", "BTC") != Present {
		t.Fatal("lowercase-stored coin not found by uppercase lookup")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spot", "markets.json")

	tbl := NewTable(now)
	tbl.SetResult(VenueResult{Venue: "binance", OK: true, Count: 2})
	tbl.SetResult(VenueResult{Venue: "okx", OK: false, Err: "status 451"})
	tbl.Put(Market{Venue: "binance", Coin: "BTC", Symbol: "BTCUSDT", Quote: "USDT", MinNotionalUSD: 5})
	tbl.Put(Market{Venue: "binance", Coin: "ETH", Symbol: "ETHUSDT", Quote: "USDT"})
	tbl.ApplyBorrow("binance", "BTC", true, 0.44, 300)

	if err := tbl.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil || got == nil {
		t.Fatalf("Load: %v (nil=%v)", err, got == nil)
	}

	if got.Spot("binance", "BTC") != Present {
		t.Error("BTC lost across save/load")
	}
	// The FAILURE must survive too, or okx silently becomes Absent on reload.
	if got.Spot("okx", "BTC") != Unknown {
		t.Error("failed-scan marker lost across save/load; okx would read as absent")
	}
	if !got.CanReverseCarry("binance", "BTC") {
		t.Error("borrow data lost across save/load")
	}
	m, _ := got.Market("binance", "BTC")
	if m.MinNotionalUSD != 5 {
		t.Errorf("min notional = %v, want 5", m.MinNotionalUSD)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file errored: %v", err)
	}
	if got != nil {
		t.Fatal("missing file returned a table")
	}
	// And the nil result answers Unknown, so no scan yet cannot become "absent".
	if got.Spot("binance", "BTC") != Unknown {
		t.Fatal("nil table from missing file did not answer unknown")
	}
}

// ----------------------------------------------------------------- parsers

func TestBinanceParse(t *testing.T) {
	raw := []byte(`{"symbols":[
	 {"symbol":"BTCUSDT","status":"TRADING","baseAsset":"BTC","quoteAsset":"USDT",
	  "filters":[{"filterType":"NOTIONAL","minNotional":"5.00000000"}]},
	 {"symbol":"ETHBTC","status":"TRADING","baseAsset":"ETH","quoteAsset":"BTC","filters":[]},
	 {"symbol":"DEADUSDT","status":"BREAK","baseAsset":"DEAD","quoteAsset":"USDT","filters":[]}
	]}`)
	got, err := binanceSpot{}.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d markets, want 1 (non-USDT and halted must be dropped): %+v", len(got), got)
	}
	if got[0].Coin != "BTC" || got[0].MinNotionalUSD != 5 {
		t.Fatalf("got %+v", got[0])
	}
}

func TestBybitParse(t *testing.T) {
	raw := []byte(`{"retCode":0,"result":{"list":[
	 {"symbol":"SOLUSDT","baseCoin":"SOL","quoteCoin":"USDT","status":"Trading",
	  "lotSizeFilter":{"minOrderAmt":"1"}},
	 {"symbol":"OLDUSDT","baseCoin":"OLD","quoteCoin":"USDT","status":"Delivering",
	  "lotSizeFilter":{"minOrderAmt":"1"}}
	]}}`)
	got, err := bybitSpot{}.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 || got[0].Coin != "SOL" {
		t.Fatalf("got %+v, want only SOL -- 'Delivering' is not 'Trading'", got)
	}
}

func TestOKXParse(t *testing.T) {
	raw := []byte(`{"code":"0","data":[
	 {"instId":"SOL-USDT","baseCcy":"SOL","quoteCcy":"USDT","state":"live","minSz":"0.1"},
	 {"instId":"OLD-USDT","baseCcy":"OLD","quoteCcy":"USDT","state":"suspend","minSz":"1"}
	]}`)
	got, err := okxSpot{}.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 || got[0].Coin != "SOL" || got[0].Symbol != "SOL-USDT" {
		t.Fatalf("got %+v, want only live SOL", got)
	}
	// minSz is in BASE units and must NOT be recorded as a USD minimum.
	if got[0].MinNotionalUSD != 0 {
		t.Fatalf("okx minSz (base units) leaked into MinNotionalUSD as %v", got[0].MinNotionalUSD)
	}
}

// An empty or error payload must ERROR, never return an empty universe. An
// empty universe reads downstream as "this venue lists nothing", which is how
// a bad afternoon becomes a permanent refusal.
func TestEmptyPayloadsError(t *testing.T) {
	cases := []struct {
		name string
		fn   func([]byte) ([]Market, error)
		raw  string
	}{
		{"binance empty", binanceSpot{}.Parse, `{"symbols":[]}`},
		{"bybit empty", bybitSpot{}.Parse, `{"retCode":0,"result":{"list":[]}}`},
		{"bybit retcode", bybitSpot{}.Parse, `{"retCode":10001,"retMsg":"bad"}`},
		{"okx empty", okxSpot{}.Parse, `{"code":"0","data":[]}`},
		{"okx code", okxSpot{}.Parse, `{"code":"50011","msg":"rate limit"}`},
		{"binance garbage", binanceSpot{}.Parse, `<html>451</html>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.fn([]byte(c.raw)); err == nil {
				t.Fatal("returned no error -- an empty universe would be written as fact")
			}
		})
	}
}
