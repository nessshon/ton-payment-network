package tonpayments

import (
	"testing"

	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
)

func TestDerivativesServiceGetSymbolByID(t *testing.T) {
	svc := &DerivativesService{}

	tests := []struct {
		name string
		id   uint32
		want string
	}{
		{
			name: "btcusdt",
			id:   oracle.GetResolverID("binance", "BTCUSDT"),
			want: "BTCUSDT",
		},
		{
			name: "tonusdt",
			id:   oracle.GetResolverID("binance", "TONUSDT"),
			want: "TONUSDT",
		},
		{
			name: "unknown",
			id:   0,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := svc.GetSymbolByID(tt.id)
			if got != tt.want {
				t.Fatalf("GetSymbolByID(%d) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}
