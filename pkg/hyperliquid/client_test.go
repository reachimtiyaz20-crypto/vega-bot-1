package hyperliquid

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testBook is arithmetic anyone can check by hand.
//
//	mid = 100
//	asks: 101 x 10 = $1010,  102 x 10 = $1020   (total $2030)
//	bids:  99 x 10 = $990,    98 x 10 = $980    (total $1970)
func testBook() Book {
	return Book{
		Symbol:   "TEST",
		Venue:    "test",
		Measured: true,
		Asks:     []Level{{Px: 101, Sz: 10, N: 1}, {Px: 102, Sz: 10, N: 1}},
		Bids:     []Level{{Px: 99, Sz: 10, N: 1}, {Px: 98, Sz: 10, N: 1}},
	}
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// --- the interval trap -------------------------------------------------------

// TestHourlyIsNotEightHourly.
//
// This is the 8x error the whole cross-venue effort turns on. Hyperliquid
// quotes per hour; Binance and Bybit quote per 8 hours. If this test ever goes
// green while wrong, every dispersion figure in the project is off by 8x in
// whichever direction happens to look profitable.
func TestHourlyIsNotEightHourly(t *testing.T) {
	if FundingIntervalHours != 1 {
		t.Fatalf("FundingIntervalHours is %v; hyperliquid settles hourly", FundingIntervalHours)
	}

	a := Asset{FundingHourly: 0.0001} // 1 bp per hour
	if got := a.FundingBpsPerHour(); !near(got, 1, 1e-9) {
		t.Fatalf("bps/hour %v, want 1", got)
	}
	if got := a.FundingBpsPerDay(); !near(got, 24, 1e-9) {
		t.Fatalf("bps/day %v, want 24 -- a per-8h reading would give 3", got)
	}
}

// --- sweeps ------------------------------------------------------------------

// TestSweepFillsOneLevelExactly. $1010 is precisely the best ask.
func TestSweepFillsOneLevelExactly(t *testing.T) {
	s := testBook().SweepCost(1010, true)
	if !s.Ok {
		t.Fatal("sweep refused a fillable size")
	}
	if s.Exhausted {
		t.Fatal("book had $2030 on the ask and reported exhausted at $1010")
	}
	if !near(s.VWAP, 101, 1e-9) {
		t.Fatalf("VWAP %v, want 101", s.VWAP)
	}
	if !near(s.SlippageBps, 100, 1e-6) {
		t.Fatalf("slippage %v bps, want 100 (1 on a mid of 100)", s.SlippageBps)
	}
	if s.LevelsUsed != 1 {
		t.Fatalf("used %d levels, want 1", s.LevelsUsed)
	}
}

// TestSweepWalksIntoTheSecondLevel. $2030 takes both asks: VWAP 101.5.
func TestSweepWalksIntoTheSecondLevel(t *testing.T) {
	s := testBook().SweepCost(2030, true)
	if !s.Ok || s.Exhausted {
		t.Fatalf("ok=%v exhausted=%v on a size the book exactly holds", s.Ok, s.Exhausted)
	}
	if !near(s.VWAP, 101.5, 1e-9) {
		t.Fatalf("VWAP %v, want 101.5", s.VWAP)
	}
	if !near(s.SlippageBps, 150, 1e-6) {
		t.Fatalf("slippage %v bps, want 150", s.SlippageBps)
	}
	if s.LevelsUsed != 2 {
		t.Fatalf("used %d levels, want 2", s.LevelsUsed)
	}
}

// TestSellSweepIsAlsoACost.
//
// Selling fills BELOW the mid. If the sign were wrong this would come back
// negative and read as free money.
func TestSellSweepIsAlsoACost(t *testing.T) {
	s := testBook().SweepCost(990, false)
	if !s.Ok {
		t.Fatal("sell sweep refused")
	}
	if !near(s.VWAP, 99, 1e-9) {
		t.Fatalf("VWAP %v, want 99", s.VWAP)
	}
	if s.SlippageBps < 0 {
		t.Fatalf("selling reported %v bps -- a negative cost is a sign error", s.SlippageBps)
	}
	if !near(s.SlippageBps, 100, 1e-6) {
		t.Fatalf("slippage %v bps, want 100", s.SlippageBps)
	}
}

// TestExhaustedBookIsFlagged.
//
// A partial fill on the SECOND leg of a hedge leaves a naked position. The
// caller has to be able to tell "filled" from "filled what it could".
func TestExhaustedBookIsFlagged(t *testing.T) {
	s := testBook().SweepCost(50_000, true)
	if !s.Exhausted {
		t.Fatal("asked for $50,000 against $2,030 of asks and was not told the book ran out")
	}
	if s.Filled > 2030.0000001 {
		t.Fatalf("filled %v from a book holding 2030", s.Filled)
	}
}

// TestRoundTripChargesBothSides.
//
// A short perp sells to open and buys to close. Charging one side is the bug
// that reported CASHCAT at 20 bps when the true cost was 119.5.
func TestRoundTripChargesBothSides(t *testing.T) {
	rt, ok := testBook().RoundTripSlippageBps(1010)
	if !ok {
		t.Fatal("round trip refused a fillable size")
	}
	// buy $1010 -> 100 bps; sell $1010 walks into the 98 bid -> more than 100.
	if rt <= 100 {
		t.Fatalf("round trip %v bps -- that is one side only", rt)
	}
	if rt < 190 || rt > 260 {
		t.Fatalf("round trip %v bps, expected roughly 200-210", rt)
	}
}

// TestRoundTripRefusesWhenTheExitCannotFill.
//
// An exit that cannot be filled is not an expensive exit. It is no exit.
func TestRoundTripRefusesWhenTheExitCannotFill(t *testing.T) {
	if _, ok := testBook().RoundTripSlippageBps(1_000_000); ok {
		t.Fatal("priced a round trip the book cannot support")
	}
}

// --- refusals ----------------------------------------------------------------

// TestUnmeasuredBookRefusesEverything.
//
// The most dangerous wrong answer this package can give is "cheap to trade".
func TestUnmeasuredBookRefusesEverything(t *testing.T) {
	var b Book

	if _, ok := b.Mid(); ok {
		t.Fatal("unmeasured book produced a mid")
	}
	if _, ok := b.SpreadBps(); ok {
		t.Fatal("unmeasured book produced a spread")
	}
	if _, _, _, ok := b.TopOfBookUSD(); ok {
		t.Fatal("unmeasured book produced a depth")
	}
	if s := b.SweepCost(100, true); s.Ok {
		t.Fatal("unmeasured book priced a sweep")
	}
	if _, ok := b.RoundTripSlippageBps(100); ok {
		t.Fatal("unmeasured book priced a round trip")
	}
	if _, _, ok := b.DepthWithinBps(50); ok {
		t.Fatal("unmeasured book reported depth within a band")
	}
}

// TestOneEmptySideIsNotAMarket.
func TestOneEmptySideIsNotAMarket(t *testing.T) {
	b := testBook()
	b.Asks = nil
	b.Measured = false // as L2Book would set it

	if _, ok := b.Mid(); ok {
		t.Fatal("a book with no asks produced a mid")
	}
}

// --- depth and top of book ---------------------------------------------------

// TestThinnerSideBounds. Both opening and closing must clear, so the smaller
// side is the constraint -- the same rule pkg/exchange applies across legs.
func TestThinnerSideBounds(t *testing.T) {
	bid, ask, min, ok := testBook().TopOfBookUSD()
	if !ok {
		t.Fatal("refused a measured book")
	}
	if !near(bid, 990, 1e-9) || !near(ask, 1010, 1e-9) {
		t.Fatalf("bid %v ask %v, want 990 and 1010", bid, ask)
	}
	if !near(min, 990, 1e-9) {
		t.Fatalf("min %v, want the thinner 990", min)
	}
}

// TestDepthWithinBandExcludesWhatIsOutside.
func TestDepthWithinBandExcludesWhatIsOutside(t *testing.T) {
	// mid 100, 150 bps -> [98.5, 101.5]. Only the 99 bid and 101 ask qualify.
	bid, ask, ok := testBook().DepthWithinBps(150)
	if !ok {
		t.Fatal("refused")
	}
	if !near(bid, 990, 1e-9) {
		t.Fatalf("bid depth %v, want 990 -- the 98 level is outside the band", bid)
	}
	if !near(ask, 1010, 1e-9) {
		t.Fatalf("ask depth %v, want 1010", ask)
	}
}

func TestSpreadBps(t *testing.T) {
	s, ok := testBook().SpreadBps()
	if !ok || !near(s, 200, 1e-6) {
		t.Fatalf("spread %v (ok=%v), want 200", s, ok)
	}
}

// --- parsing -----------------------------------------------------------------

// TestNumRefusesRatherThanZeroing. Zero is a legitimate funding rate and a
// catastrophic price.
func TestNumRefusesRatherThanZeroing(t *testing.T) {
	for _, s := range []string{"", "abc", "1.2.3", "null"} {
		if _, ok := num(s); ok {
			t.Fatalf("parsed %q as a number", s)
		}
	}
	if v, ok := num("0"); !ok || v != 0 {
		t.Fatal("a real zero must parse")
	}
}

// TestMismatchedArraysAreFatal.
//
// metaAndAssetCtxs returns [universe, contexts] as PARALLEL POSITIONAL arrays.
// If they ever differ in length, every coin is paired with another coin's
// funding rate -- which would not look like an error, it would look like a
// strategy.
func TestMismatchedArraysAreFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// two coins, one context
		_, _ = w.Write([]byte(`[
		  {"universe":[{"name":"BTC","szDecimals":5},{"name":"ETH","szDecimals":4}]},
		  [{"funding":"0.0000125","markPx":"60000","oraclePx":"60000","dayNtlVlm":"1000","openInterest":"10","impactPxs":["59990","60010"]}]
		]`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), URL: srv.URL}
	if _, err := c.Assets(context.Background()); err == nil {
		t.Fatal("accepted a universe and context array of different lengths")
	} else if !strings.Contains(err.Error(), "positional") {
		t.Fatalf("error does not explain the danger: %v", err)
	}
}

// TestAssetsDecodesAndNormalises.
func TestAssetsDecodesAndNormalises(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  {"universe":[{"name":"BTC","szDecimals":5,"maxLeverage":40}]},
		  [{"funding":"0.0001","markPx":"60000","oraclePx":"59995","dayNtlVlm":"1234567",
		    "openInterest":"10","midPx":"60005","impactPxs":["59994","60006"]}]
		]`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), URL: srv.URL}
	assets, err := c.Assets(context.Background())
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	a, ok := assets["BTC"]
	if !ok {
		t.Fatal("BTC missing")
	}
	if !a.PricesOk {
		t.Fatal("prices flagged bad on a complete response")
	}
	if !near(a.FundingBpsPerHour(), 1, 1e-9) {
		t.Fatalf("bps/hour %v, want 1", a.FundingBpsPerHour())
	}
	if !near(a.FundingBpsPerDay(), 24, 1e-9) {
		t.Fatalf("bps/day %v, want 24", a.FundingBpsPerDay())
	}
	if !near(a.OpenInterestUSD(), 600000, 1e-6) {
		t.Fatalf("OI USD %v, want 600000", a.OpenInterestUSD())
	}
	if a.ImpactSpreadBps <= 0 {
		t.Fatal("impact spread not computed")
	}
	if a.MaxLeverage != 40 {
		t.Fatalf("maxLeverage %d, want 40", a.MaxLeverage)
	}
}

// TestUnknownVenueIsCountedNotAssumed.
func TestUnknownVenueIsCountedNotAssumed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
		  ["KAITO",[
		     ["HlPerp",{"fundingRate":"0.0001","nextFundingTime":1}],
		     ["BinPerp",{"fundingRate":"0.0008","nextFundingTime":2}],
		     ["MysteryPerp",{"fundingRate":"0.5","nextFundingTime":3}]
		  ]]
		]`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), URL: srv.URL}
	rates, unknown, err := c.PredictedFundings(context.Background())
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if unknown != 1 {
		t.Fatalf("unknown venues %d, want 1", unknown)
	}
	if _, present := rates["KAITO"]["MysteryPerp"]; present {
		t.Fatal("a venue with no known settlement interval was kept")
	}

	hl := rates["KAITO"]["hyperliquid"]
	bin := rates["KAITO"]["binance"]

	// Hyperliquid settles hourly for EVERY asset, so its interval belongs to
	// the venue and arrives already applied.
	if !hl.Known() {
		t.Fatal("hyperliquid rate came back without an interval")
	}
	if !near(hl.BpsPerHour, 1, 1e-9) {
		t.Fatalf("hyperliquid %v bps/hr, want 1", hl.BpsPerHour)
	}

	// Binance must arrive RAW AND UNUSABLE.
	//
	// This used to carry a hardcoded 8. Binance publishes the interval per
	// SYMBOL -- KAITOUSDT is 4 hours -- so that constant made every 4-hour
	// pair read twice as rich as it was, and the pair we traded on 2026-08-12
	// showed a 24.8 bps/hr spread whose true value was 0.33.
	if bin.Known() {
		t.Fatalf("binance arrived pre-normalised at %v bps/hr; its interval is a "+
			"property of the SYMBOL and this package cannot know it", bin.BpsPerHour)
	}
	if bin.BpsPerHour != 0 {
		t.Fatalf("unnormalised rate exposed a BpsPerHour of %v; it must be 0 until "+
			"an interval is supplied", bin.BpsPerHour)
	}
	if !near(bin.RawRate, 0.0008, 1e-12) {
		t.Fatalf("raw rate %v, want 0.0008 untouched", bin.RawRate)
	}

	// The same raw rate means two different things on two different clocks.
	// This is the entire bug, in two lines.
	if got := bin.WithInterval(8).BpsPerHour; !near(got, 1, 1e-9) {
		t.Fatalf("0.0008 over 8h = %v bps/hr, want 1", got)
	}
	if got := bin.WithInterval(4).BpsPerHour; !near(got, 2, 1e-9) {
		t.Fatalf("0.0008 over 4h = %v bps/hr, want 2 -- the 8h assumption HALVED this", got)
	}

	// A nonsense interval must produce an unusable rate, not a huge one.
	if z := bin.WithInterval(0); z.Known() || z.BpsPerHour != 0 {
		t.Fatalf("a zero interval produced %v bps/hr", z.BpsPerHour)
	}
}
