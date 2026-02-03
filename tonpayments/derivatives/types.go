package derivatives

// PositionView represents a simplified view of a derivatives position for UI.
// Prices are decimal strings (scaled by asset decimals or external market scale).
type PositionView struct {
	ChannelAddress   string  `json:"channel_address"`
	IsLong           bool    `json:"is_long"`
	Leverage         int     `json:"leverage"`
	EntryAt          int64   `json:"entry_at"`
	EntryPrice       string  `json:"entry_price"`
	CurrentPrice     string  `json:"current_price"`
	PnLPercent       float64 `json:"pnl_percent"`
	LiquidationPrice string  `json:"liquidation_price"`
}
