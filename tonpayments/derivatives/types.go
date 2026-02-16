package derivatives

// PositionView represents a simplified view of a derivatives position for UI.
// Prices are decimal strings (scaled by asset decimals or external market scale).
type PositionView struct {
	ID               string  `json:"id"`
	Symbol           string  `json:"symbol"`
	ChannelAddress   string  `json:"channel_address"`
	Collateral       string  `json:"collateral"`
	Fee              string  `json:"fee"`
	IsLong           bool    `json:"is_long"`
	Leverage         int     `json:"leverage"`
	Status           string  `json:"status"`
	Opened           bool    `json:"opened"`
	OpenedAt         int64   `json:"opened_at,omitempty"`
	EntryAt          int64   `json:"entry_at"`
	EntryPrice       string  `json:"entry_price"`
	CurrentPrice     string  `json:"current_price"`
	PnLPercent       float64 `json:"pnl_percent"`
	LiquidationPrice string  `json:"liquidation_price"`
}

type QuoteView struct {
	Symbol   string `json:"symbol"`
	Price    string `json:"price"`
	RawPrice string `json:"raw_price"`
	At       int64  `json:"at"`
}

type PriceHistoryPoint struct {
	At    int64  `json:"at"`
	Price string `json:"price"`
}
