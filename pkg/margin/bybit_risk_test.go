package margin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func riskServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestRetCodeFailureIsNotAnEmptySchedule.
//
// Bybit answers HTTP 200 on failure. Decoding an error into an empty tier list
// would produce a schedule that TierFor rejects -- correct by accident, with
// no explanation of what actually went wrong.
func TestRetCodeFailureIsNotAnEmptySchedule(t *testing.T) {
	srv := riskServer(`{"retCode":10001,"retMsg":"params error","result":{}}`)
	defer srv.Close()

	b := &BybitRisk{HTTP: srv.Client(), BaseURL: srv.URL}
	_, err := b.Fetch(context.Background(), "BADUSDT")
	if err == nil {
		t.Fatal("a retCode failure produced a schedule")
	}
	if !strings.Contains(err.Error(), "10001") {
		t.Fatalf("error hides the venue's own code: %v", err)
	}
}

// TestImplausibleMaintenanceRateIsRefused.
//
// THE UNIT TRAP. If a venue moved from "0.005" to "0.5" for the same 0.5%,
// every liquidation price in this project would shift by 100x -- in the
// direction that makes positions look safe. Rescaling on a guess about which
// unit was meant is exactly the mistake that cost us a day.
func TestImplausibleMaintenanceRateIsRefused(t *testing.T) {
	srv := riskServer(`{"retCode":0,"retMsg":"OK","result":{"list":[
	  {"symbol":"XUSDT","riskLimitValue":"50000","maintenanceMargin":"1.5","initialMargin":"3","maxLeverage":"50"}
	]}}`)
	defer srv.Close()

	b := &BybitRisk{HTTP: srv.Client(), BaseURL: srv.URL}
	_, err := b.Fetch(context.Background(), "XUSDT")
	if err == nil {
		t.Fatal("accepted a 150% maintenance rate")
	}
	// The message must explain the UNIT error, not just report a number.
	// Bound moved from 0.5 to 1.0 on 2026-08-14 after it refused Bybit's
	// legitimate 60% top tier for BTCUSDT.
	if !strings.Contains(err.Error(), "fraction") {
		t.Fatalf("refusal does not name the danger: %v", err)
	}
}

// TestZeroMaintenanceIsRefused. Zero means "never liquidated", which is the
// most dangerous wrong answer this file could give.
func TestZeroMaintenanceIsRefused(t *testing.T) {
	srv := riskServer(`{"retCode":0,"retMsg":"OK","result":{"list":[
	  {"symbol":"XUSDT","riskLimitValue":"50000","maintenanceMargin":"0","initialMargin":"0.02","maxLeverage":"50"}
	]}}`)
	defer srv.Close()

	b := &BybitRisk{HTTP: srv.Client(), BaseURL: srv.URL}
	if _, err := b.Fetch(context.Background(), "XUSDT"); err == nil {
		t.Fatal("accepted a zero maintenance rate")
	}
}

// TestEmptyListIsRefused.
func TestEmptyListIsRefused(t *testing.T) {
	srv := riskServer(`{"retCode":0,"retMsg":"OK","result":{"list":[]}}`)
	defer srv.Close()

	b := &BybitRisk{HTTP: srv.Client(), BaseURL: srv.URL}
	if _, err := b.Fetch(context.Background(), "XUSDT"); err == nil {
		t.Fatal("accepted a response with no tiers")
	}
}

// TestGoodResponseIsParsedAndVerified.
func TestGoodResponseIsParsedAndVerified(t *testing.T) {
	srv := riskServer(`{"retCode":0,"retMsg":"OK","result":{"list":[
	  {"symbol":"XUSDT","riskLimitValue":"50000","maintenanceMargin":"0.01","initialMargin":"0.02","maxLeverage":"50"},
	  {"symbol":"XUSDT","riskLimitValue":"100000","maintenanceMargin":"0.015","initialMargin":"0.03","maxLeverage":"33"}
	]}}`)
	defer srv.Close()

	b := &BybitRisk{HTTP: srv.Client(), BaseURL: srv.URL}
	s, err := b.Fetch(context.Background(), "XUSDT")
	if err != nil {
		t.Fatalf("refused a good response: %v", err)
	}
	if !s.Verified {
		t.Fatal("a schedule read from the venue's own API is not marked verified")
	}
	if s.VerifiedAt.IsZero() || s.Source == "" {
		t.Fatal("provenance not recorded")
	}
	if len(s.Tiers) != 2 {
		t.Fatalf("%d tiers, want 2", len(s.Tiers))
	}

	// A $40k position takes the 1% bracket, an $80k one the 1.5% bracket.
	if tier, ok := s.TierFor(40_000); !ok || tier.MaintenanceMarginRate != 0.01 {
		t.Fatalf("$40k got %v (ok=%v), want 1%%", tier.MaintenanceMarginRate, ok)
	}
	if tier, ok := s.TierFor(80_000); !ok || tier.MaintenanceMarginRate != 0.015 {
		t.Fatalf("$80k got %v (ok=%v), want 1.5%%", tier.MaintenanceMarginRate, ok)
	}
	if _, ok := s.TierFor(500_000); ok {
		t.Fatal("a position above every bracket was accepted")
	}

	// And it lands in the cache.
	if _, ok := b.Cached("XUSDT"); !ok {
		t.Fatal("not cached")
	}
}
