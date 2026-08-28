package main

import (
	"context"
	"testing"

	"github.com/imtiyaz/vega-bot/pkg/fees"
	"github.com/imtiyaz/vega-bot/pkg/orderbook"
)

func TestGetTakerFor_Registry(t *testing.T) {
	reg, err := fees.Load("../../config/fees.json")
	if err != nil {
		t.Fatalf("failed to load fees registry: %v", err)
	}

	tests := []struct {
		venue string
		want  float64
	}{
		{"okx", 5.0},
		{"binance", 5.0},
		{"bybit", 5.5},
	}

	for _, tt := range tests {
		got, ok := getTakerFor(tt.venue, "BTC-USDT-SWAP", reg, nil)
		if !ok {
			t.Errorf("getTakerFor(%q) returned not ok", tt.venue)
		}
		if got != tt.want {
			t.Errorf("getTakerFor(%q) = %v, want %v", tt.venue, got, tt.want)
		}
	}
}

func TestGetTakerFor_Absent(t *testing.T) {
	reg, err := fees.Load("../../config/fees.json")
	if err != nil {
		t.Fatalf("failed to load fees registry: %v", err)
	}

	got, ok := getTakerFor("unknown", "BTCUSDT", reg, nil)
	if ok {
		t.Errorf("expected absent venue to return not ok, got ok=true with fee %v", got)
	}
	if got != 0 {
		t.Errorf("expected absent venue to return 0 fee, got %v", got)
	}
}

type mockMEXC struct {
	takerFunc func(symbol string) (float64, bool)
}

func (m mockMEXC) TakerBps(symbol string) (float64, bool) {
	return m.takerFunc(symbol)
}

func (m mockMEXC) Venue() string                          { return "mexc" }
func (m mockMEXC) LoadSymbols(ctx context.Context) error  { return nil }
func (m mockMEXC) SymbolCount() int                       { return 1 }
func (m mockMEXC) ResolveCoin(coin string) (string, bool) { return coin, true }
func (m mockMEXC) Book(ctx context.Context, symbol string) (orderbook.Book, error) {
	return orderbook.Book{}, nil
}
func (m mockMEXC) FundingIntervalHours(symbol string) orderbook.FundingInterval {
	return orderbook.FundingInterval{}
}
func (m mockMEXC) Symbols() []string { return []string{"BTC_USDT"} }

func TestGetTakerFor_MEXC(t *testing.T) {
	reg, err := fees.Load("../../config/fees.json")
	if err != nil {
		t.Fatalf("failed to load fees registry: %v", err)
	}

	mockReader := mockMEXC{
		takerFunc: func(symbol string) (float64, bool) {
			if symbol == "BTC_USDT" {
				return 4.0, true
			}
			return 0, false
		},
	}

	readers := map[string]orderbook.PerpReader{
		"mexc": mockReader,
	}

	got, ok := getTakerFor("mexc", "BTC_USDT", reg, readers)
	if !ok {
		t.Errorf("expected mexc to resolve taker, got ok=false")
	}
	if got != 4.0 {
		t.Errorf("expected mexc to resolve to 4.0, got %v", got)
	}
}

func TestFeesBps(t *testing.T) {
	reg, err := fees.Load("../../config/fees.json")
	if err != nil {
		t.Fatalf("failed to load fees registry: %v", err)
	}

	takerA, okA := getTakerFor("bybit", "BTCUSDT", reg, nil)
	takerB, okB := getTakerFor("binance", "BTCUSDT", reg, nil)

	if !okA || !okB {
		t.Fatalf("failed to get takers: okA=%v okB=%v", okA, okB)
	}

	feesBps := 2 * (takerA + takerB)
	want := 2 * (5.5 + 5.0)
	if feesBps != want {
		t.Errorf("feesBps = %v, want %v", feesBps, want)
	}
}
