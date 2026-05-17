package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
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
	AcceptingDerivatives            bool
	DerivativesHedge                DerivativesHedgeConfig
}

type DerivativesHedgeConfig struct {
	WebhookURL                    string
	WebhookKey                    string
	WebhookSignatureHMACSHA256Key string
}

type CoinTypes struct {
	Ton             CoinConfig
	Jettons         map[string]CoinConfig
	ExtraCurrencies map[uint32]CoinConfig
}

type VaultCoinBalanceConfig struct {
	MinBalance string
	MaxBalance string
}

type VaultConfig struct {
	UseOnOurSide     bool
	AllowOnTheirSide bool
	PrivateKey       []byte
	Coins            map[string]VaultCoinBalanceConfig
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
	Vault                          VaultConfig
	ChannelConfig                  ChannelsConfig
}

const LatestConfigVersion = 6

func generateBase64Random(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func Generate() (*Config, error) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}

	_, walletPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}

	_, vaultPriv, err := ed25519.GenerateKey(nil)
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
	hedgeKey, err := generateBase64Random(12)
	if err != nil {
		return nil, err
	}
	hedgeSigKey, err := generateBase64Random(32)
	if err != nil {
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
		Vault: VaultConfig{
			UseOnOurSide:     false,
			AllowOnTheirSide: false,
			PrivateKey:       vaultPriv.Seed(),
			Coins:            map[string]VaultCoinBalanceConfig{},
		},
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
			AcceptingDerivatives:            false,
			DerivativesHedge: DerivativesHedgeConfig{
				WebhookKey:                    hedgeKey,
				WebhookSignatureHMACSHA256Key: hedgeSigKey,
			},
		},
	}

	cfg.assignBalanceID()
	return cfg, nil
}

func Upgrade(cfg *Config) (bool, error) {
	if cfg.Version >= LatestConfigVersion {
		return false, nil
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

	if cfg.Version < 4 {
		if cfg.ChannelConfig.DerivativesHedge.WebhookURL == "" {
			cfg.ChannelConfig.DerivativesHedge = DerivativesHedgeConfig{}
		}
	}

	if cfg.Version < 5 {
		if cfg.ChannelConfig.DerivativesHedge.WebhookURL == "" &&
			cfg.ChannelConfig.DerivativesHedge.WebhookKey == "" &&
			cfg.ChannelConfig.DerivativesHedge.WebhookSignatureHMACSHA256Key == "" {
			cfg.ChannelConfig.DerivativesHedge = DerivativesHedgeConfig{}
		} else {
			if cfg.ChannelConfig.DerivativesHedge.WebhookKey == "" {
				key, err := generateBase64Random(12)
				if err != nil {
					return false, fmt.Errorf("generate derivatives hedge webhook key: %w", err)
				}
				cfg.ChannelConfig.DerivativesHedge.WebhookKey = key
			}
			if cfg.ChannelConfig.DerivativesHedge.WebhookSignatureHMACSHA256Key == "" {
				key, err := generateBase64Random(32)
				if err != nil {
					return false, fmt.Errorf("generate derivatives hedge signature key: %w", err)
				}
				cfg.ChannelConfig.DerivativesHedge.WebhookSignatureHMACSHA256Key = key
			}
		}
	}

	if cfg.Version < 6 {
		if len(cfg.Vault.PrivateKey) == 0 {
			_, vaultPriv, err := ed25519.GenerateKey(nil)
			if err != nil {
				return false, fmt.Errorf("generate vault key: %w", err)
			}
			cfg.Vault.PrivateKey = vaultPriv.Seed()
		}
		if cfg.Vault.Coins == nil {
			cfg.Vault.Coins = map[string]VaultCoinBalanceConfig{}
		}
	}

	cfg.Version = LatestConfigVersion
	return true, nil
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
