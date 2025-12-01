//go:build js && wasm

package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/log"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/ton-payment-network/tonpayments"
	"github.com/xssnick/ton-payment-network/tonpayments/chain"
	"github.com/xssnick/ton-payment-network/tonpayments/chain/client"
	"github.com/xssnick/ton-payment-network/tonpayments/config"
	"github.com/xssnick/ton-payment-network/tonpayments/db"
	"github.com/xssnick/ton-payment-network/tonpayments/db/browser"
	"github.com/xssnick/ton-payment-network/tonpayments/transport"
	"github.com/xssnick/ton-payment-network/tonpayments/transport/web"
	"github.com/xssnick/ton-payment-network/tonpayments/wallet"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/tlb"
	"math/big"
	"strings"
	"syscall/js"
	"time"
)

var DB *db.DB
var Service *tonpayments.Service
var Config *config.Config

const MinFee = "0.000000001"
const FeePercentDiv100 = 0

func main() {
	var started bool
	var sPub ed25519.PublicKey

	js.Global().Set("openChannel", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !started {
			return js.Null()
		}

		_, err := Service.OpenChannelWithNode(context.Background(), sPub)
		if err != nil {
			println(err.Error())
		}
		return js.Null()
	}))

	js.Global().Set("sendTransfer", js.FuncOf(func(this js.Value, args []js.Value) any {
		promiseCtor := js.Global().Get("Promise")

		return promiseCtor.New(js.FuncOf(func(this js.Value, prArgs []js.Value) any {
			resolve := prArgs[0]
			reject := prArgs[1]

			go func() {
				if !started {
					reject.Invoke("not started")
					return
				}

				if len(args) != 3 {
					reject.Invoke("wrong number of arguments")
					return
				}

				// currency is optional third arg, default TON for backward compatibility
				currency := args[2].String()

				cc, err := Service.ResolveCoinConfigBySymbol(currency)
				if err != nil {
					reject.Invoke("failed to get coin config: " + err.Error())
					return
				}

				amt, err := tlb.FromDecimal(args[0].String(), int(cc.Decimals))
				if err != nil {
					reject.Invoke("failed to parse amount: " + err.Error())
					return
				}

				if amt.IsNegative() {
					reject.Invoke("amount below 0")
					return
				}

				// min per-hop fee based on coin config
				minFeeAmt := cc.VirtualTunnelConfig.ProxyMinFee.Nano()

				flt := new(big.Float).SetInt(amt.Nano())
				flt.Mul(flt, big.NewFloat(cc.VirtualTunnelConfig.ProxyFeePercent/100))
				fee, acc := flt.Int(new(big.Int))
				if acc == big.Below {
					fee.Add(fee, big.NewInt(1))
				}

				if fee.Cmp(minFeeAmt) < 0 {
					fee.Set(minFeeAmt)
				}

				addr, err := base64.StdEncoding.DecodeString(args[1].String())
				if err != nil {
					reject.Invoke("failed to parse address: " + err.Error())
					return
				}

				_, vKey, err := sendTransfer(amt, cc.MustAmount(fee), cc, [][]byte{sPub, addr}, false)
				if err != nil {
					reject.Invoke("failed to send transfer: " + err.Error())
					return
				}

				resolve.Invoke(base64.StdEncoding.EncodeToString(vKey))
				return
			}()

			return nil
		}))
	}))

	js.Global().Set("estimateTransfer", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !started {
			return js.Null()
		}

		if len(args) != 3 {
			return js.ValueOf("wrong number of arguments")
		}

		currency := args[2].String()

		cc, err := Service.ResolveCoinConfigBySymbol(currency)
		if err != nil {
			return js.ValueOf("failed to get coin config: " + err.Error())
		}

		amt, err := tlb.FromDecimal(args[0].String(), int(cc.Decimals))
		if err != nil {
			return js.ValueOf("")
		}

		if amt.IsNegative() {
			return js.ValueOf("")
		}

		addr, err := base64.StdEncoding.DecodeString(args[1].String())
		if err != nil {
			return js.ValueOf("")
		}
		if len(addr) != 32 {
			return js.ValueOf("")
		}

		// min per-hop fee based on coin config
		minFeeAmt := cc.VirtualTunnelConfig.ProxyMinFee.Nano()

		flt := new(big.Float).SetInt(amt.Nano())
		flt.Mul(flt, big.NewFloat(cc.VirtualTunnelConfig.ProxyFeePercent/100))
		fee, acc := flt.Int(new(big.Int))
		if acc == big.Below {
			fee.Add(fee, big.NewInt(1))
		}

		if fee.Cmp(minFeeAmt) < 0 {
			fee.Set(minFeeAmt)
		}

		fullAmt, _, err := sendTransfer(amt, cc.MustAmount(fee), cc, [][]byte{sPub, addr}, true)
		if err != nil {
			return js.ValueOf("failed to estimate transfer: " + err.Error())
		}

		return js.ValueOf(tlb.MustFromNano(fullAmt.Sub(fullAmt, amt.Nano()), int(cc.Decimals)).String())
	}))

	js.Global().Set("sendTransferWithPath", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !started {
			return js.Null()
		}

		if len(args) != 4 {
			println("wrong number of arguments")
			return js.Null()
		}

		currency := args[3].String()

		cc, err := Service.ResolveCoinConfigBySymbol(currency)
		if err != nil {
			println("failed to get coin config: " + err.Error())
			return js.Null()
		}

		keys := args[0]
		if keys.Type() != js.TypeObject || !keys.InstanceOf(js.Global().Get("Array")) {
			println("expected an array of strings")
			return js.Null()
		}

		var parsedKeys [][]byte
		for i := 0; i < keys.Length(); i++ {
			if keys.Index(i).Type() != js.TypeString {
				println("element at index", i, "is not a string")
				return js.Null()
			}
			strKey := keys.Index(i).String()

			btsKey, err := base64.StdEncoding.DecodeString(strKey)
			if err != nil {
				println("incorrect format of key: " + err.Error())
				return js.Null()
			}
			if len(btsKey) != 32 {
				println("incorrect len of key: " + err.Error())
				return js.Null()
			}

			parsedKeys = append(parsedKeys, btsKey)
		}

		amt, err := tlb.FromDecimal(args[1].String(), int(cc.Decimals))
		if err != nil {
			println("failed to parse amount: " + err.Error())
			return js.Null()
		}

		feeAmt, err := tlb.FromDecimal(args[2].String(), int(cc.Decimals))
		if err != nil {
			println("failed to parse fee amount: " + err.Error())
			return js.Null()
		}

		if _, _, err = sendTransfer(amt, feeAmt, cc, parsedKeys, false); err != nil {
			println("failed to send transfer: " + err.Error())
			return js.Null()
		}

		return js.Null()
	}))

	js.Global().Set("withdrawChannel", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !started {
			return js.Null()
		}

		if len(args) != 3 {
			println("wrong number of arguments")
			return js.Null()
		}
		currency := args[1].String()

		targetAddress, err := address.ParseAddr(args[2].String())
		if err != nil {
			println("failed to parse target address: " + err.Error())
			return js.Null()
		}

		ch, err := getPrimaryChanel(sPub)
		if err != nil {
			println("failed to get primary channel: " + err.Error())
			return js.Null()
		}

		cc, err := Service.ResolveCoinConfigBySymbol(currency)
		if err != nil {
			println("failed to get coin config: " + err.Error())
			return js.Null()
		}

		amt, err := tlb.FromDecimal(args[0].String(), int(cc.Decimals))
		if err != nil {
			println("failed to parse amount: " + err.Error())
			return js.Null()
		}

		if err = Service.RequestWithdrawToAddr(context.Background(), ch.Our.Address, targetAddress, cc, amt.Nano()); err != nil {
			println("failed to request withdraw: " + err.Error())
			return js.Null()
		}

		return js.Null()
	}))

	js.Global().Set("topupChannel", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !started {
			return js.Null()
		}

		if len(args) != 2 {
			println("wrong number of arguments")
			return js.Null()
		}
		currency := args[1].String()

		ch, err := getPrimaryChanel(sPub)
		if err != nil {
			println("failed to get primary channel: " + err.Error())
			return js.Null()
		}

		cc, err := Service.ResolveCoinConfigBySymbol(currency)
		if err != nil {
			println("failed to get coin config: " + err.Error())
			return js.Null()
		}

		amt, err := tlb.FromDecimal(args[0].String(), int(cc.Decimals))
		if err != nil {
			println("failed to parse amount: " + err.Error())
			return js.Null()
		}

		err = Service.ExecuteTopup(context.Background(), ch.Our.Address, cc.BalanceID, amt, false)
		if err != nil {
			println(err.Error())
		}

		return js.Null()
	}))

	js.Global().Set("listChannelsPrint", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !started {
			return js.Null()
		}

		Service.DebugPrintChannels(context.Background(), db.ChannelStateActive)
		return js.Null()
	}))

	js.Global().Set("listChannelsPrintAll", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !started {
			return js.Null()
		}

		Service.DebugPrintChannels(context.Background(), db.ChannelStateAny)
		return js.Null()
	}))

	js.Global().Set("getChannelHistory", js.FuncOf(func(this js.Value, args []js.Value) any {
		promiseCtor := js.Global().Get("Promise")

		return promiseCtor.New(js.FuncOf(func(this js.Value, prArgs []js.Value) any {
			resolve := prArgs[0]
			reject := prArgs[1]

			go func() {
				if len(args) != 1 {
					reject.Invoke("wrong number of arguments")
					return
				}

				if !started {
					resolve.Invoke(js.Null())
					return
				}

				num := args[0].Int()
				if num == 0 {
					resolve.Invoke(js.Null())
					return
				}

				ch, err := getPrimaryChanel(sPub)
				if err != nil {
					resolve.Invoke(js.Global().Get("Array").New(0))
					println("failed to get primary channel: " + err.Error())
					return
				}

				events, err := Service.GetChannelsHistoryByPeriod(
					context.Background(), ch.Our.Address, num, nil, nil,
				)
				if err != nil {
					reject.Invoke("get channel history err: " + err.Error())
					return
				}

				arr := js.Global().Get("Array").New(len(events))
				for i, e := range events {
					obj := js.Global().Get("Object").New()
					obj.Set("id", fmt.Sprint(e.At.UnixNano()))
					obj.Set("action", int(e.Action))
					obj.Set("timestamp", e.At.Format("2006-01-02 15:04"))

					switch expr := e.ParseData().(type) {
					case *db.ChannelHistoryActionAmountData:
						obj.Set("isTheir", expr.IsTheir)
						obj.Set("amounts", mapConvert(expr.Amounts))
					case *db.ChannelHistoryActionTransferInData:
						obj.Set("amounts", mapConvert(expr.Amounts))
						obj.Set("party", base64.StdEncoding.EncodeToString(expr.From))
					case *db.ChannelHistoryActionTransferOutData:
						obj.Set("amounts", mapConvert(expr.Amounts))
						obj.Set("party", base64.StdEncoding.EncodeToString(expr.To))
					}

					arr.SetIndex(i, obj)
				}

				resolve.Invoke(arr)
			}()

			return nil
		}))
	}))

	js.Global().Set("stopPaymentNetwork", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !started {
			return js.Null()
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := Service.CommitAllOurVirtualChannelsAndWait(ctx); err != nil {
			panic(err.Error())
			return js.Null()
		}
		Service.Stop()

		sPub = nil
		started = false
		return js.Null()
	}))

	js.Global().Set("dumpTasks", js.FuncOf(func(this js.Value, args []js.Value) any {
		pfx := args[0].String()
		all := args[1].Bool()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		list, err := DB.DumpTasks(ctx, pfx)
		cancel()
		if err != nil {
			log.Error().Err(err).Msg("failed to load planned tasks")
			return js.Null()
		}

		for _, task := range list {
			if task.CompletedAt != nil {
				if all {
					log.Info().Str("type", task.Type).
						Str("id", task.ID).
						Time("created_at", task.CreatedAt).
						Time("completed_at", *task.CompletedAt).
						Msg("completed task")
				}
				continue
			}

			if task.ExecuteTill != nil && task.ExecuteTill.Before(time.Now()) {
				if all {
					log.Info().Str("type", task.Type).
						Str("id", task.ID).
						Time("created_at", task.CreatedAt).
						Time("execute_till", *task.ExecuteTill).
						Msg("outdated task")
				}
				continue
			}

			log.Info().Str("type", task.Type).
				Str("id", task.ID).
				Time("created_at", task.CreatedAt).
				Str("last_error", task.LastError).
				Time("after", task.ExecuteAfter).
				Str("queue", task.Queue).
				Msg("planned task")
		}

		return js.Null()
	}))

	js.Global().Set("startPaymentNetwork", js.FuncOf(func(this js.Value, args []js.Value) any {
		if started {
			return js.Null()
		}

		if len(args) != 2 {
			println("wrong number of arguments")
			return js.Null()
		}

		serverNetPub, err := base64.StdEncoding.DecodeString(args[0].String())
		if err != nil {
			panic(err)
			return js.Null()
		}

		serverChPub, err := base64.StdEncoding.DecodeString(args[1].String())
		if err != nil {
			panic(err)
			return js.Null()
		}

		started = true
		go start(serverNetPub, serverChPub)
		sPub = serverChPub

		go func() {
			for {
				time.Sleep(5 * time.Second)

				jsNow := int64(js.Global().Get("Date").Call("now").Float())
				goNow := time.Now().UnixMilli()

				diff := jsNow - goNow
				if diff < 0 {
					diff = -diff
				}

				if diff > 3000 {
					println("time diff discovered, reloading page to sync", diff)
					js.Global().Get("location").Call("reload")
				}
			}
		}()

		return js.Null()
	}))
	select {}
}

func start(peerKey, channelKey []byte) {
	wl, _ := wallet.InitWallet()
	userId := hex.EncodeToString(wl.WalletAddress().Data())

	var configPath = "payments-config-v2-" + userId
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		panic(err)
	}
	if config.Upgrade(cfg) {
		if err = config.SaveConfig(cfg, configPath); err != nil {
			log.Fatal().Err(err).Msg("failed to update config file")
			return
		}
	}
	Config = cfg

	cfg.ChannelConfig.SupportedCoins.Ton.MinCapacityRequest = "1"
	cfg.ChannelConfig.SupportedCoins.Ton.FeePerWithdrawPropose = "0.05"
	cfg.ChannelConfig.SupportedCoins.Ton.VirtualTunnelConfig.MaxCapacityToRentPerTx = "5"
	cfg.ChannelConfig.SupportedCoins.Ton.VirtualTunnelConfig.CapacityDepositFee = "0.05"
	cfg.ChannelConfig.SupportedCoins.Ton.VirtualTunnelConfig.CapacityFeePercentPer30Days = 0.1
	cfg.ChannelConfig.SupportedCoins.Ton.BalanceControl = nil

	cfg.ChannelConfig.SupportedCoins.Jettons["EQDQp0PWKNlb3rFzP3WgLp_0vzL0bAcoZXWlvs9SmcGRPkJv"] = config.CoinConfig{
		Enabled: true,
		VirtualTunnelConfig: config.VirtualConfig{
			MaxCapacityToRentPerTx:      "10",
			CapacityDepositFee:          "0.3",
			CapacityFeePercentPer30Days: 0.1,
			ProxyMaxCapacity:            "15.5",
			ProxyMinFee:                 "0.002",
			ProxyFeePercent:             0.8,
		},
		Symbol:                "USDX",
		Decimals:              6,
		MinCapacityRequest:    "3",
		FeePerWithdrawPropose: "0.3",
		BalanceControl:        nil,
	}

	if err = config.SaveConfig(cfg, configPath); err != nil {
		panic(err)
	}

	idb, freshDb, err := browser.NewIndexedDB(userId + ".v2")
	if err != nil {
		panic(err.Error())
	}

	d := db.NewDB(idb, ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey).Public().(ed25519.PublicKey))
	DB = d

	if freshDb {
		if err = d.SetMigrationVersion(context.Background(), len(db.Migrations)); err != nil {
			log.Fatal().Err(err).Msg("failed to set initial migration version")
		}
	} else {
		if err = db.RunMigrations(d); err != nil {
			log.Fatal().Err(err).Msg("failed to run migrations")
		}
	}

	pKey := peerKey
	sPub := channelKey

	tn := client.NewTON()
	nt := web.NewHTTP(tn, ed25519.NewKeyFromSeed(cfg.ADNLServerKey), sPub, pKey)
	tr := transport.NewTransport(ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey), nt, false)

	ch := make(chan any, 10)
	sc := chain.NewScanner(tn, ch)

	pcuFunc := js.Global().Get("onPaymentChannelUpdated")
	if pcuFunc.Type() != js.TypeFunction {
		panic("onPaymentChannelUpdated is not a function (not registered from js)")
	}

	pcuHistoryFunc := js.Global().Get("onPaymentChannelHistoryUpdated")
	if pcuHistoryFunc.Type() != js.TypeFunction {
		panic("onPaymentChannelHistoryUpdated is not a function (not registered from js)")
	}

	onUpd := func(ctx context.Context, ch *db.Channel, statusChanged bool) {
		sc.OnChannelUpdate(ctx, ch, statusChanged)

		resBalances, resCapacities,
			resLocked, resPending := map[string]any{}, map[string]any{},
			map[string]any{}, map[string]any{}

		balances, err := ch.CalcBalance(ctx, false, Service)
		if err != nil {
			println("failed to calc balance: " + err.Error())
			return
		}
		for _, info := range balances {
			resBalances[info.CoinConfig.Symbol] = info.CoinConfig.MustAmount(info.Available()).String()
		}

		capacity, err := ch.CalcBalance(ctx, true, Service)
		if err != nil {
			println("failed to calc capacity: " + err.Error())
			return
		}
		for _, info := range capacity {
			resCapacities[info.CoinConfig.Symbol] = info.CoinConfig.MustAmount(info.Available()).String()
		}

		// compute locked (our locked deposits) for this coin
		for s, ld := range ch.Our.LockedDeposits {
			cc, _ := Service.ResolveCoinConfig(s)
			resLocked[cc.Symbol] = cc.MustAmount(ld.Available()).String()
		}

		pendSums := map[string]*big.Int{}
		// compute commits in progress from their side
		for key, pn := range ch.Their.PendingOnchainTransfers {
			if !strings.HasPrefix(key, "commit_") {
				continue
			}

			for s, v := range pn.Amounts {
				b := pendSums[s]
				if b == nil {
					b = big.NewInt(0)
					pendSums[s] = b
				}
				b.Add(b, v)
			}
		}

		for s, b := range pendSums {
			cc, _ := Service.ResolveCoinConfig(s)
			resPending[cc.Symbol] = cc.MustAmount(b).String()
		}

		jsEvent := map[string]any{
			"active":     ch.Status == db.ChannelStateActive,
			"balances":   js.ValueOf(resBalances),
			"capacities": js.ValueOf(resCapacities),
			"locked":     js.ValueOf(resLocked),
			"pendingIn":  js.ValueOf(resPending),
			"address":    ch.Our.Address,
		}

		pcuFunc.Invoke(js.ValueOf(jsEvent))
	}

	d.SetOnChannelUpdated(onUpd)
	d.SetOnChannelHistoryUpdated(func(ctx context.Context, ch *db.Channel, item db.ChannelHistoryItem) {
		pcuHistoryFunc.Invoke()
	})

	svc, err := tonpayments.NewService(tn, d, tr, nil, wl, ch, ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey), cfg.ChannelConfig, false)
	if err != nil {
		panic(err)
	}

	tr.SetService(svc)
	log.Info().Str("pubkey", base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(cfg.PaymentNodePrivateKey).Public().(ed25519.PublicKey))).Msg("payment node initialized")

	go svc.Start()
	Service = svc

	chList, err := d.GetChannels(context.Background(), nil, db.ChannelStateAny)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load channels")
		return
	}

	noChannels := true
	for _, channel := range chList {
		if channel.Status != db.ChannelStateInactive {
			noChannels = false
			onUpd(context.Background(), channel, true)
		}
	}

	loaded := js.Global().Get("onPaymentNetworkLoaded")
	if loaded.Type() == js.TypeFunction {
		addr := base64.StdEncoding.EncodeToString(Service.GetPrivateKey().Public().(ed25519.PublicKey))
		loaded.Invoke(addr)
	}

	if noChannels {
		jsEvent := map[string]any{
			"active":     false,
			"balances":   js.ValueOf(map[string]any{}),
			"capacities": js.ValueOf(map[string]any{}),
			"locked":     js.ValueOf(map[string]any{}),
			"pendingIn":  js.ValueOf(map[string]any{}),
			"address":    "",
		}

		pcuFunc.Invoke(js.ValueOf(jsEvent))
	}

	select {}
}

func getPrimaryChanel(with ed25519.PublicKey) (*db.Channel, error) {
	list, err := Service.ListChannels(context.Background(), with, db.ChannelStateActive)
	if err != nil {
		return nil, fmt.Errorf("failed to list channels: %w", err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no active channels")
	}

	return list[0], nil
}

func sendTransfer(amt, feeAmt tlb.Coins, cc *payments.CoinConfig, keys [][]byte, justEstimate bool) (*big.Int, ed25519.PublicKey, error) {
	safeHopTTL := time.Duration(Config.ChannelConfig.QuarantineDurationSec+Config.ChannelConfig.BufferTimeToCommit+Config.ChannelConfig.ConditionalCloseDurationSec+
		Config.ChannelConfig.ActionsDuration+
		Config.ChannelConfig.MinSafeVirtualChannelTimeoutSec) * time.Second

	fullAmt := new(big.Int).Set(amt.Nano())
	var tunChain []transport.TunnelChainPart
	for i, parsedKey := range keys {
		fee := big.NewInt(0)
		if len(keys)-i > 1 {
			fee = new(big.Int).Mul(feeAmt.Nano(), big.NewInt(int64(len(keys)-i)-1))
			fullAmt = fullAmt.Add(fullAmt, fee)
		}

		tunChain = append(tunChain, transport.TunnelChainPart{
			Target:   parsedKey,
			Capacity: amt.Nano(),
			Fee:      fee,
			Deadline: time.Now().Add(3*time.Hour + safeHopTTL*time.Duration(len(keys)-i)),
		})
	}

	if justEstimate {
		return fullAmt, nil, nil
	}

	vPub, vPriv, _ := ed25519.GenerateKey(nil)

	firstInstructionKey, tun, err := transport.GenerateTunnel(vPriv, tunChain, 0, true, Service.GetPrivateKey(), cc)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate tunnel: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	err = Service.CreateSendConditional(ctx, firstInstructionKey, vPriv, tunChain[0], tunChain[len(tunChain)-1], tun, cc)
	cancel()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open virtual channel: %w", err)
	}

	// commit state to server to not get uncoop closed in case of browser page close
	if err := Service.CommitAllOurVirtualChannelsAndWait(ctx); err != nil {
		println("warn: transfer sent, but state not committed:" + err.Error())
	}

	return fullAmt, vPub, nil
}

func mapConvert(m map[string]string) map[string]any {
	var res = make(map[string]any)
	for k, v := range m {
		res[k] = v
	}
	return res
}
