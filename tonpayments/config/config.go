package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"math/big"
)

type VirtualConfig struct {
	MaxCapacityToRentPerTx      string
	CapacityDepositFee          string
	CapacityFeePercentPer30Days float64
	ProxyMaxCapacity            string
	ProxyMinFee                 string
	ProxyFeePercent             float64
	DerivativeFeePercent        float64
	AllowTunneling              bool
}

type BalanceControlConfig struct {
	DepositWhenAmountLessThan string
	DepositUpToAmount         string
	WithdrawWhenAmountReached string
}

type CoinConfig struct {
	Enabled               bool
	VirtualTunnelConfig   VirtualConfig
	Symbol                string
	Decimals              uint8
	MinCapacityRequest    string
	FeePerWithdrawPropose string

	balanceID string

	BalanceControl *BalanceControlConfig
}

func (c *CoinConfig) MustAmount(nano *big.Int) tlb.Coins {
	return tlb.MustFromNano(nano, int(c.Decimals))
}

func (c *CoinConfig) MustAmountDecimal(str string) tlb.Coins {
	return tlb.MustFromDecimal(str, int(c.Decimals))
}

func (c *CoinConfig) GetBalanceID() string {
	if c.balanceID == "" {
		panic("empty balance id")
	}
	return c.balanceID
}

type ChannelsConfig struct {
	SupportedCoins CoinTypes

	BufferTimeToCommit              uint32
	QuarantineDurationSec           uint32
	ActionsDuration                 uint32
	ConditionalCloseDurationSec     uint32
	MinSafeVirtualChannelTimeoutSec uint32
	ReplicationMessageAttachAmount  string
}

type CoinTypes struct {
	Ton             CoinConfig
	Jettons         map[string]CoinConfig
	ExtraCurrencies map[uint32]CoinConfig
}

type Config struct {
	Version                        int
	ADNLServerKey                  []byte
	PaymentNodePrivateKey          []byte
	WalletPrivateKey               []byte
	APIListenAddr                  string
	WebTransportListenAddr         string
	MetricsListenAddr              string
	MetricsNamespace               string
	WebhooksSignatureHMACSHA256Key string
	NodeListenAddr                 string
	ExternalIP                     string
	NetworkConfigUrl               string
	DBPath                         string
	SecureProofPolicy              bool
	ChannelConfig                  ChannelsConfig
}

const LatestConfigVersion = 3

func Generate() (*Config, error) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}

	_, walletPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}

	_, nodePriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}

	whKey := make([]byte, 32)
	if _, err = rand.Read(whKey); err != nil {
		return nil, err
	}

	cfg := &Config{
		Version:                        LatestConfigVersion,
		ADNLServerKey:                  nodePriv.Seed(),
		PaymentNodePrivateKey:          priv.Seed(),
		WalletPrivateKey:               walletPriv.Seed(),
		APIListenAddr:                  "127.0.0.1:8096",
		WebTransportListenAddr:         "",
		MetricsListenAddr:              "127.0.0.1:8097",
		MetricsNamespace:               "",
		NodeListenAddr:                 "0.0.0.0:17555",
		ExternalIP:                     "",
		NetworkConfigUrl:               "https://ton-blockchain.github.io/global.config.json",
		DBPath:                         "./payment-node-db",
		WebhooksSignatureHMACSHA256Key: base64.StdEncoding.EncodeToString(whKey),
		SecureProofPolicy:              false,
		ChannelConfig: ChannelsConfig{
			SupportedCoins: CoinTypes{
				Ton: CoinConfig{
					Enabled: true,
					VirtualTunnelConfig: VirtualConfig{
						MaxCapacityToRentPerTx:      "5",
						CapacityDepositFee:          "0.05",
						CapacityFeePercentPer30Days: 0.1,
						ProxyMaxCapacity:            "5",
						ProxyMinFee:                 "0.0005",
						ProxyFeePercent:             0.5,
						DerivativeFeePercent:        0.5,
						AllowTunneling:              true,
					},
					Symbol:                "TON",
					Decimals:              9,
					MinCapacityRequest:    "1",
					FeePerWithdrawPropose: "0.05",
					BalanceControl: &BalanceControlConfig{
						DepositWhenAmountLessThan: "2",
						DepositUpToAmount:         "3",
						WithdrawWhenAmountReached: "5",
					},
				},
				Jettons: map[string]CoinConfig{
					"EQCxE6mUtQJKFnGfaROTKOt1lZbDiiX1kCixRv7Nw2Id_sDs": {
						Enabled: false,
						VirtualTunnelConfig: VirtualConfig{
							MaxCapacityToRentPerTx:      "10",
							CapacityDepositFee:          "0.3",
							CapacityFeePercentPer30Days: 0.1,
							ProxyMaxCapacity:            "15.5",
							ProxyMinFee:                 "0.002",
							ProxyFeePercent:             0.8,
							DerivativeFeePercent:        0.5,
							AllowTunneling:              false,
						},
						Symbol:                "USDT",
						Decimals:              6,
						MinCapacityRequest:    "3",
						FeePerWithdrawPropose: "0.3",
						BalanceControl:        nil,
					},
				},
				ExtraCurrencies: map[uint32]CoinConfig{},
			},
			BufferTimeToCommit:              3 * 3600,
			QuarantineDurationSec:           6 * 3600,
			ActionsDuration:                 2 * 3600,
			ConditionalCloseDurationSec:     3 * 3600,
			MinSafeVirtualChannelTimeoutSec: 60,
			ReplicationMessageAttachAmount:  "0.1",
		},
	}

	cfg.assignBalanceID()
	return cfg, nil
}

func Upgrade(cfg *Config) bool {
	if cfg.Version >= LatestConfigVersion {
		return false
	}

	if cfg.Version < 2 {
		upgrade := func(cc CoinConfig) CoinConfig {
			if cc.VirtualTunnelConfig.MaxCapacityToRentPerTx == "" {
				cc.VirtualTunnelConfig.MaxCapacityToRentPerTx = "0"
			}
			if cc.VirtualTunnelConfig.CapacityDepositFee == "" {
				cc.VirtualTunnelConfig.CapacityDepositFee = "0"
			}
			if cc.MinCapacityRequest == "" {
				cc.MinCapacityRequest = "0"
			}
			return cc
		}

		cfg.ChannelConfig.SupportedCoins.Ton = upgrade(cfg.ChannelConfig.SupportedCoins.Ton)
		for s := range cfg.ChannelConfig.SupportedCoins.Jettons {
			cfg.ChannelConfig.SupportedCoins.Jettons[s] = upgrade(cfg.ChannelConfig.SupportedCoins.Jettons[s])
		}
		for s := range cfg.ChannelConfig.SupportedCoins.ExtraCurrencies {
			cfg.ChannelConfig.SupportedCoins.ExtraCurrencies[s] = upgrade(cfg.ChannelConfig.SupportedCoins.ExtraCurrencies[s])
		}
	}

	if cfg.Version < 3 {
		upgrade := func(cc CoinConfig) CoinConfig {
			if cc.FeePerWithdrawPropose == "" {
				cc.FeePerWithdrawPropose = "0"
			}
			return cc
		}

		cfg.ChannelConfig.SupportedCoins.Ton = upgrade(cfg.ChannelConfig.SupportedCoins.Ton)
		for s := range cfg.ChannelConfig.SupportedCoins.Jettons {
			cfg.ChannelConfig.SupportedCoins.Jettons[s] = upgrade(cfg.ChannelConfig.SupportedCoins.Jettons[s])
		}
		for s := range cfg.ChannelConfig.SupportedCoins.ExtraCurrencies {
			cfg.ChannelConfig.SupportedCoins.ExtraCurrencies[s] = upgrade(cfg.ChannelConfig.SupportedCoins.ExtraCurrencies[s])
		}
	}

	cfg.Version = LatestConfigVersion
	return true
}

func (cfg *Config) assignBalanceID() {
	cfg.ChannelConfig.SupportedCoins.Ton.balanceID = payments.GetTONBalanceID()
	for s, cc := range cfg.ChannelConfig.SupportedCoins.Jettons {
		cc.balanceID = payments.GetJettonBalanceID(address.MustParseAddr(s))
		cfg.ChannelConfig.SupportedCoins.Jettons[s] = cc
	}
	for s, cc := range cfg.ChannelConfig.SupportedCoins.ExtraCurrencies {
		cc.balanceID = payments.GetECBalanceID(s)
		cfg.ChannelConfig.SupportedCoins.ExtraCurrencies[s] = cc
	}
}
