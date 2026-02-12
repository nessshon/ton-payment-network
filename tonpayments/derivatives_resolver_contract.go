package tonpayments

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals"
	condcontracts "github.com/xssnick/ton-payment-network/pkg/payments/conditionals/contracts"
	"github.com/xssnick/ton-payment-network/pkg/payments/conditionals/oracle"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
)

const (
	derivativeResolverQuarantineDuration = uint32(1)
	derivativeResolverAcceptionWindow    = uint32(10)
	derivativeResolverDeployAmount       = "0.05"
)

var errDerivativeResolverMetaMissing = errors.New("derivative resolver metadata missing")

func resolveDerivativeOracleKey(assetID uint32) ([]byte, error) {
	resolver := oracle.PriceResolvers[assetID]
	if resolver == nil {
		return nil, fmt.Errorf("price resolver is not configured for asset %d", assetID)
	}
	if key := resolver.GetProofPublicKey(); len(key) == ed25519.PublicKeySize {
		return append([]byte(nil), key...), nil
	}
	return nil, fmt.Errorf("price resolver proof key is unavailable for asset %d", assetID)
}

func (s *Service) buildDerivativeResolverContract(channel *db.Channel, key []byte, amount *big.Int, details conditionals.ConditionalResolvableDetails) (*tlb.StateInit, *address.Address, conditionals.ConditionalResolvableInstructionDetails, error) {
	if channel == nil {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("channel is nil")
	}
	if len(key) != 32 {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("invalid derivative key size: %d", len(key))
	}
	if amount == nil || amount.Sign() < 0 {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("invalid derivative amount")
	}

	entry := details.EntryPrice.Nano()
	if entry == nil || entry.Sign() <= 0 {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("invalid derivative entry price")
	}
	oracleKey, err := resolveDerivativeOracleKey(details.AssetID)
	if err != nil {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("failed to resolve derivative oracle key: %w", err)
	}

	amountCoins, err := tlb.FromNano(amount, 9)
	if err != nil {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("failed to convert derivative amount: %w", err)
	}
	entryCoins, err := tlb.FromNano(entry, 9)
	if err != nil {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("failed to convert derivative entry price: %w", err)
	}

	cfg := condcontracts.DerivativeConfig{
		OracleKey:          oracleKey,
		QuarantineDuration: derivativeResolverQuarantineDuration,
		AcceptionWindow:    derivativeResolverAcceptionWindow,
		AddressA:           address.MustParseAddr(channel.SideA().Address),
		AddressB:           address.MustParseAddr(channel.SideB().Address),
	}

	storage, err := condcontracts.BuildDerivativeStorage(
		key,
		cfg,
		amountCoins,
		details.Leverage,
		details.IsLong,
		entryCoins,
		0,
	)
	if err != nil {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("failed to build derivative resolver storage: %w", err)
	}

	stateInit, err := condcontracts.BuildDerivativeStateInit(storage)
	if err != nil {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("failed to build derivative resolver state init: %w", err)
	}

	resolverAddr, err := condcontracts.CalcDerivativeAddress(stateInit)
	if err != nil {
		return nil, nil, conditionals.ConditionalResolvableInstructionDetails{}, fmt.Errorf("failed to calc derivative resolver address: %w", err)
	}

	instruction := conditionals.ConditionalResolvableInstructionDetails{
		ResolverContractCodeHash: append([]byte{}, stateInit.Code.Hash()...),
		ResolverContractData:     stateInit.Data,
	}

	return stateInit, resolverAddr, instruction, nil
}

func parseDerivativeResolverMeta(detailsCell *cell.Cell) (*derivativeResolverMeta, error) {
	if detailsCell == nil {
		return nil, nil
	}

	var details conditionals.ConditionalResolvableInstructionDetails
	if err := payments.LoadState(&details, detailsCell); err != nil {
		return nil, err
	}

	if details.ResolverContractData == nil || len(details.ResolverContractCodeHash) == 0 {
		return nil, nil
	}

	return &derivativeResolverMeta{
		CodeHash: base64.StdEncoding.EncodeToString(details.ResolverContractCodeHash),
		DataBOC:  base64.StdEncoding.EncodeToString(details.ResolverContractData.ToBOC()),
	}, nil
}

func buildDerivativeMetaAny(details conditionals.ConditionalResolvableDetails, instructionDetails *cell.Cell) any {
	packed := derivativeMetaAny{
		Details: details,
	}

	resolver, err := parseDerivativeResolverMeta(instructionDetails)
	if err == nil && resolver != nil {
		packed.Resolver = resolver
	}

	return packed
}

func loadDerivativeResolverStateInit(meta *db.ConditionalMeta) (*tlb.StateInit, error) {
	if meta == nil || meta.SpecialDetails == nil {
		return nil, errDerivativeResolverMetaMissing
	}

	var packed derivativeMetaAny
	if err := recodeJSON(meta.SpecialDetails, &packed); err != nil {
		return nil, errDerivativeResolverMetaMissing
	}
	if packed.Resolver == nil || packed.Resolver.DataBOC == "" {
		return nil, errDerivativeResolverMetaMissing
	}

	dataBOC, err := base64.StdEncoding.DecodeString(packed.Resolver.DataBOC)
	if err != nil {
		return nil, fmt.Errorf("failed to decode resolver data BOC: %w", err)
	}

	data, err := cell.FromBOC(dataBOC)
	if err != nil {
		return nil, fmt.Errorf("failed to parse resolver data BOC: %w", err)
	}

	stateInit, err := condcontracts.BuildDerivativeStateInit(data)
	if err != nil {
		return nil, err
	}

	if packed.Resolver.CodeHash != "" {
		wantHash, err := base64.StdEncoding.DecodeString(packed.Resolver.CodeHash)
		if err != nil {
			return nil, fmt.Errorf("failed to decode resolver code hash: %w", err)
		}
		if !bytes.Equal(stateInit.Code.Hash(), wantHash) {
			return nil, fmt.Errorf("resolver code hash mismatch")
		}
	}

	return stateInit, nil
}

func (s *Service) scheduleDerivativeResolverDeployTask(ctx context.Context, channelAddr string, channelInitAt *time.Time, executeAfter *time.Time) error {
	var initAt int64
	if channelInitAt != nil {
		initAt = channelInitAt.Unix()
	} else {
		ch, err := s.db.GetChannel(ctx, channelAddr)
		if err == nil {
			initAt = ch.InitAt.Unix()
		}
	}

	return s.db.CreateTask(
		ctx,
		PaymentsTaskPool,
		"deploy-derivative-resolvers",
		channelAddr+"-derivative-resolvers",
		fmt.Sprintf("deploy-derivative-resolvers-%s-%d", channelAddr, initAt),
		db.ChannelTask{Address: channelAddr},
		executeAfter,
		nil,
	)
}

type derivativeResolverDeployInfo struct {
	Address   *address.Address
	StateInit *tlb.StateInit
}

func (s *Service) collectChannelDerivativeResolvers(ctx context.Context, channel *db.Channel) ([]derivativeResolverDeployInfo, error) {
	if channel == nil {
		return nil, fmt.Errorf("channel is nil")
	}

	collected := map[string]struct{}{}
	var result []derivativeResolverDeployInfo

	collectFromDict := func(dict *cell.Dictionary) error {
		if dict == nil {
			return nil
		}

		items, err := dict.LoadAll()
		if err != nil {
			return fmt.Errorf("failed to load conditionals: %w", err)
		}

		for _, kv := range items {
			if kv.Value == nil || (kv.Value.BitsLeft() == 0 && kv.Value.RefsNum() == 0) {
				continue
			}

			parsed, err := payments.CodeToConditional(ctx, kv.Value.MustToCell(), s)
			if err != nil {
				continue
			}

			cond, ok := parsed.(*conditionals.ConditionalResolvable)
			if !ok || cond.ResolverAddr == nil || cond.Amount == nil || cond.Amount.Sign() <= 0 {
				continue
			}

			addrKey := cond.ResolverAddr.String()
			if _, exists := collected[addrKey]; exists {
				continue
			}

			var stateInit *tlb.StateInit

			meta, err := s.db.GetVirtualChannelMeta(ctx, cond.GetKey())
			if err == nil {
				stateInit, err = loadDerivativeResolverStateInit(meta)
				if err != nil && !errors.Is(err, errDerivativeResolverMetaMissing) {
					return fmt.Errorf("failed to load resolver state init from meta: %w", err)
				}
			} else if !errors.Is(err, db.ErrNotFound) {
				return fmt.Errorf("failed to load derivative meta: %w", err)
			}

			if stateInit == nil {
				stateInit, _, _, err = s.buildDerivativeResolverContract(channel, cond.GetKey(), cond.Amount, cond.Details)
				if err != nil {
					log.Warn().Err(err).Str("key", base64.StdEncoding.EncodeToString(cond.GetKey())).Msg("failed to rebuild derivative resolver state init")
					continue
				}
			}

			calculatedAddr, err := condcontracts.CalcDerivativeAddress(stateInit)
			if err != nil {
				return fmt.Errorf("failed to calc derivative resolver addr: %w", err)
			}
			if !calculatedAddr.Equals(cond.ResolverAddr) {
				log.Warn().
					Str("expected", cond.ResolverAddr.String()).
					Str("calculated", calculatedAddr.String()).
					Str("key", base64.StdEncoding.EncodeToString(cond.GetKey())).
					Msg("resolver address mismatch, skipping deployment")
				continue
			}

			collected[addrKey] = struct{}{}
			result = append(result, derivativeResolverDeployInfo{
				Address:   calculatedAddr,
				StateInit: stateInit,
			})
		}

		return nil
	}

	if err := collectFromDict(channel.Our.Data.Conditionals); err != nil {
		return nil, err
	}
	if err := collectFromDict(channel.Their.Data.Conditionals); err != nil {
		return nil, err
	}

	return result, nil
}

func (s *Service) deployChannelDerivativeResolvers(ctx context.Context, channelAddr string) error {
	channel, err := s.db.GetChannel(ctx, channelAddr)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("failed to get channel: %w", err)
	}
	if channel.Status == db.ChannelStateInactive {
		return nil
	}

	onchain, err := s.channelClient.GetChannel(ctx, address.MustParseAddr(channelAddr), true, channel.Our.LastProcessedTxAt)
	if err != nil {
		return fmt.Errorf("failed to get onchain channel: %w", err)
	}
	if onchain.Status == payments.ChannelStatusOpen {
		return ErrStillPending
	}
	if onchain.Status == payments.ChannelStatusUninitialized {
		return nil
	}

	toDeploy, err := s.collectChannelDerivativeResolvers(ctx, channel)
	if err != nil {
		return err
	}
	if len(toDeploy) == 0 {
		return nil
	}

	deployAmount := tlb.MustFromTON(derivativeResolverDeployAmount)
	for _, resolver := range toDeploy {
		if resolver.Address == nil || resolver.StateInit == nil {
			continue
		}

		acc, err := s.ton.GetAccount(ctx, resolver.Address, channel.Our.LastProcessedTxAt)
		if err != nil {
			return fmt.Errorf("failed to get resolver account state: %w", err)
		}
		if acc != nil && acc.HasState {
			continue
		}

		if _, err = s.wallet.DoTransactionMany(ctx, "Deploy derivative resolver", []WalletMessage{{
			Amount:    deployAmount,
			StateInit: resolver.StateInit,
		}}); err != nil {
			return fmt.Errorf("failed to deploy resolver %s: %w", resolver.Address.String(), err)
		}

		log.Info().Str("channel", channelAddr).Str("resolver", resolver.Address.String()).Msg("derivative resolver deployed")
	}

	return nil
}
