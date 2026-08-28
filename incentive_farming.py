#!/usr/bin/env python3
"""Incentive farming is a VOLUME business, not a yield business.

WHY EVERY MEASUREMENT SO FAR MISSED IT

Five days of work measured funding carry and found it pays roughly nothing over
lending: median funding 3.8%/yr on notional against 4.45%/yr to borrow dollars.
That result is solid. It is also the wrong place to have been looking.

Hyperliquid Season 1 distributed 274.15M HYPE to 94,028 wallets -- an average of
2,915 each, top decile $50k-$500k+ -- for TRADING VOLUME between Nov 2023 and
May 2024. Not for holding. Not for yield. For volume.

So the arithmetic is nothing like carry:

	reward per $ of volume  =  (tokens to farmers x price) / total campaign volume
	cost   per $ of volume  =  your net fee rate
	return on capital       =  (reward - cost) x volume per $ of capital

The last term is what makes this different. Carry earns a rate on capital held.
This earns a margin on volume PUSHED, and volume can be 50-100x capital per
month. A margin of half a basis point becomes 30%+ annualised at high turnover.

WHICH IS WHY MAKER EXECUTION IS THE WHOLE BUSINESS

Priced as a cost saving on a carry trade, maker looked marginal -- 7 bps off a
45 bps round trip. Priced here it is the difference between a business and a
donation: at 2.5 bps taker you almost certainly pay more in fees than the
airdrop is worth per unit of volume. At 0 bps or a rebate, the volume is free
and the reward is pure margin. Delta-neutral is simply how you churn volume
without taking price risk.

WHAT CANNOT BE KNOWN IN ADVANCE, and it is most of it

	the numerator    token price at unlock, and after decay
	the denominator  TOTAL volume farmed by everyone -- your share falls as
	                 others pile in, and the crowd arrives once it is working
	the rules        retroactive criteria, sybil filters, wash-trade exclusion

So this tool does not forecast. It answers: GIVEN a set of assumptions, what
must be true for farming to beat lending at 4.45%? The breakeven fee rate and
breakeven denominator are the outputs that matter, because those are the two
things you can actually check against a live campaign.

Run:  python3 incentive_farming.py
"""

LEND = 4.45          # %/yr, the do-nothing benchmark, measured on Bybit


class Campaign:
    def __init__(self, name, tokens_m, price, total_vol_b, months, note=""):
        self.name = name
        self.tokens = tokens_m * 1e6
        self.price = price              # USD at the point you could sell
        self.total_vol = total_vol_b * 1e9
        self.months = months
        self.note = note

    @property
    def pot(self):
        return self.tokens * self.price

    @property
    def bps_per_volume(self):
        """Reward in bps per dollar of volume traded."""
        if self.total_vol <= 0:
            return 0.0
        return self.pot / self.total_vol * 10000.0


def annualised(margin_bps, turnover_month):
    """margin_bps per $ volume, turnover = volume per $ capital per month."""
    monthly = margin_bps / 10000.0 * turnover_month
    return ((1 + monthly) ** 12 - 1) * 100.0


def main():
    print("INCENTIVE FARMING -- what has to be true for it to beat %.2f%% lending\n"
          % LEND)

    # Hyperliquid S1 is the reference case. Token price is the honest problem:
    # HYPE opened near $2 and ran far higher, so the answer depends entirely on
    # when you sold. Both are shown because the gap between them IS the risk.
    cases = [
        Campaign("Hyperliquid S1 @ $2", 274.15, 2.0, 150, 6,
                 "sold at listing"),
        Campaign("Hyperliquid S1 @ $10", 274.15, 10.0, 150, 6,
                 "held through the run"),
        Campaign("mid-size @ $50M pot", 100, 0.5, 30, 3,
                 "typical smaller perp DEX"),
        Campaign("small @ $10M pot", 50, 0.2, 20, 3,
                 "crowded, modest allocation"),
    ]

    print("%-24s %10s %12s %14s  %s"
          % ("campaign", "pot $M", "total vol $B", "reward bps/vol", "note"))
    for c in cases:
        print("%-24s %10.0f %12.0f %14.3f  %s"
              % (c.name, c.pot / 1e6, c.total_vol / 1e9, c.bps_per_volume, c.note))

    print("\nFEE REGIMES -- net margin per $ of volume, after cost")
    fees = [("taker 2.5 bps", 2.5), ("taker 1.0 bps", 1.0),
            ("maker 0 bps", 0.0), ("maker rebate -0.5 bps", -0.5)]
    print("%-24s %s" % ("campaign", "  ".join("%-14s" % f[0] for f in fees)))
    for c in cases:
        row = "%-24s" % c.name
        for _, f in fees:
            row += "  %-14.3f" % (c.bps_per_volume - f)
        print(row)

    print("\nANNUALISED RETURN ON CAPITAL, by monthly turnover")
    print("(turnover = dollars of volume pushed per dollar of capital, per month)")
    turns = [10, 30, 60, 100, 200]
    for _, fee in (("maker 0 bps", 0.0), ("taker 1.0 bps", 1.0)):
        print("\n  fee regime: %.1f bps" % fee)
        print("  %-24s %s" % ("campaign",
                              "  ".join("%8s" % ("%dx" % t) for t in turns)))
        for c in cases:
            m = c.bps_per_volume - fee
            row = "  %-24s" % c.name
            for t in turns:
                r = annualised(m, t)
                row += "  %7.0f%%" % r if abs(r) < 100000 else "  %7s" % "--"
            print(row)

    print("\nBREAKEVEN CONDITIONS -- the two numbers to check against a live campaign")
    print("%-24s %18s %22s" % ("campaign", "max fee (bps)", "max total vol ($B)"))
    for c in cases:
        # Volume at which reward per $ of volume equals a 1 bps fee.
        max_vol = c.pot / (1.0 / 10000.0) / 1e9 if c.pot else 0
        print("%-24s %18.3f %22.0f" % (c.name, c.bps_per_volume, max_vol))

    print("\nREAD THIS BEFORE BELIEVING ANY NUMBER ABOVE")
    print("  Every figure depends on the DENOMINATOR -- total volume farmed by")
    print("  everyone -- and that is unknowable in advance and rises precisely")
    print("  because the opportunity is working. Your share shrinks as the")
    print("  crowd arrives.")
    print("  The numerator is a token price you cannot know and may not be able")
    print("  to sell at. Hyperliquid at $2 and at $10 are the same campaign.")
    print("  Sybil filters, wash-trade exclusion and retroactive rule changes")
    print("  can zero the whole thing after the work is done.")
    print("\n  This is not a yield. It is a bet on a distribution, financed by")
    print("  a carry trade that roughly covers its own costs. Size it as a bet.")


if __name__ == "__main__":
    main()
