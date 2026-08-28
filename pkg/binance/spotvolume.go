package binance

import (
	"context"
	"encoding/json"
	"fmt"
)

// Spot24hQuoteVolume returns 24h volume in USDT for every spot symbol.
//
// This exists because futures volume alone is a misleading filter. On
// 2026-08-05 HFTUSDT showed $96m of FUTURES volume and passed a $50m floor,
// while its SPOT book held five dollars at the touch. The long leg of a
// cash-and-carry is bought on spot, so spot liquidity is the binding
// constraint, and screening on the perp side alone lets an unbuildable
// position through.
//
// The response is large (3673 symbols, several MB). It is fetched on the same
// cadence as the books, which is fine at one poll a minute, but it is the
// heaviest call in the set.
func (c *Client) Spot24hQuoteVolume(ctx context.Context) (map[string]float64, error) {
	body, err := c.getAbs(ctx, SpotBase+"/api/v3/ticker/24hr")
	if err != nil {
		return nil, err
	}

	var raw []rawTicker24h
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("binance: decoding spot ticker/24hr: %w (first 200 bytes: %.200s)", err, body)
	}

	out := make(map[string]float64, len(raw))
	for _, r := range raw {
		if v, ok := parseOptional(r.QuoteVolume); ok {
			out[r.Symbol] = v
		}
	}
	return out, nil
}
