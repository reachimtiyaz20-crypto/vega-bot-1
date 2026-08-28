package exchange

// REFUSAL CODES
//
// A stable, machine-readable name for each way Assess can say no.
//
// These exist so a book can COUNT its refusals. Without them the only record is
// a formatted sentence, and a sentence cannot answer "which gate is binding" --
// which is the question that decides whether a threshold is calibrated or is
// quietly excluding everything worth trading.
//
// The code is set at the point the message is written, deliberately. Deriving
// it by matching substrings of Reason would mean that rewording a message
// silently reclassifies the gate as unknown, and a gate that appears to stop
// firing is worse than one that was never counted.
const (
	GateOK = ""

	// GateVenueInvalid carries whatever Venue.Validate refused on: unverified
	// fees, a missing source URL, or a venue with no spot markets at all.
	GateVenueInvalid = "VENUE_INVALID"

	// GateNoSpotPair means this symbol has no spot market, so the long leg of
	// a cash-and-carry cannot be constructed at any price.
	GateNoSpotPair = "NO_SPOT_PAIR"

	GatePerpVolumeTooLow = "PERP_VOLUME_TOO_LOW"
	GateSpotVolumeTooLow = "SPOT_VOLUME_TOO_LOW"

	// GateLiquidityUnmeasured means the order book could not be read, so
	// execution cost is unknown. Refusing here is deliberate: the alternative
	// is entering on the configured fallback, which is an assumption.
	GateLiquidityUnmeasured = "LIQUIDITY_UNMEASURED"

	GateSpotBookTooThin = "SPOT_BOOK_TOO_THIN"
	GatePerpBookTooThin = "PERP_BOOK_TOO_THIN"

	// GateSlippageTooHigh means measured round-trip slippage exceeded the
	// ceiling. This is the gate most likely to be miscalibrated after a size
	// change, because the ceiling is absolute while slippage scales with size.
	GateSlippageTooHigh = "SLIPPAGE_TOO_HIGH"

	// GateExpectedNetNegative means every structural check passed and the
	// trade still does not pay: funding over the expected hold is smaller
	// than the round trip.
	//
	// This is the honest refusal. It is not a threshold anyone chose and
	// it is not a proxy for liquidity -- it is the arithmetic saying no.
	// A book whose refusals are dominated by this one has no gate to
	// loosen; it has an edge problem.
	GateExpectedNetNegative = "EXPECTED_NET_NEGATIVE"
)

// AllGates lists every refusal code, for building complete reports where a
// gate that fired zero times should still appear as a zero.
func AllGates() []string {
	return []string{
		GateVenueInvalid,
		GateNoSpotPair,
		GatePerpVolumeTooLow,
		GateSpotVolumeTooLow,
		GateLiquidityUnmeasured,
		GateSpotBookTooThin,
		GatePerpBookTooThin,
		GateSlippageTooHigh,
		GateExpectedNetNegative,
	}
}
