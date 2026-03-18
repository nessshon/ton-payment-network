package config

import (
	"testing"

	"github.com/xssnick/tonutils-go/address"
)

func TestGenerateSetsVaultDefaults(t *testing.T) {
	cfg, err := Generate()
	if err != nil {
		t.Fatalf("generate config failed: %v", err)
	}

	if cfg.Version != LatestConfigVersion {
		t.Fatalf("unexpected config version %d", cfg.Version)
	}
	if cfg.Vault.UseOnOurSide {
		t.Fatalf("vault use-on-our-side must be disabled by default")
	}
	if cfg.Vault.AllowOnTheirSide {
		t.Fatalf("vault allow-on-their-side must be disabled by default")
	}
	if len(cfg.Vault.PrivateKey) == 0 {
		t.Fatalf("vault private key must be generated")
	}
	if cfg.Vault.Coins == nil {
		t.Fatalf("vault coins config must be initialized")
	}
	if cfg.ChannelConfig.SupportedCoins.Ton.balanceID == "" {
		t.Fatalf("ton balance id must stay assigned after generate")
	}
}

func TestUpgradeAddsVaultConfig(t *testing.T) {
	cfg, err := Generate()
	if err != nil {
		t.Fatalf("generate config failed: %v", err)
	}

	cfg.Version = 5
	cfg.Vault.PrivateKey = nil
	cfg.Vault.Coins = nil
	cfg.ChannelConfig.SupportedCoins.Ton.balanceID = ""
	cfg.ChannelConfig.SupportedCoins.Jettons = map[string]CoinConfig{
		address.MustParseRawAddr("0:1111111111111111111111111111111111111111111111111111111111111111").String(): {
			Enabled: true,
			Symbol:  "TEST",
		},
	}

	updated, err := Upgrade(cfg)
	if err != nil {
		t.Fatalf("upgrade config failed: %v", err)
	}
	if !updated {
		t.Fatalf("upgrade must report changes for older config")
	}
	if cfg.Version != LatestConfigVersion {
		t.Fatalf("unexpected config version after upgrade %d", cfg.Version)
	}
	if len(cfg.Vault.PrivateKey) == 0 {
		t.Fatalf("upgrade must generate vault private key")
	}
	if cfg.Vault.Coins == nil {
		t.Fatalf("upgrade must initialize vault coins config")
	}
}
