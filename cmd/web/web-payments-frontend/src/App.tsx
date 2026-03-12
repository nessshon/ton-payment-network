import React, { useEffect, useMemo, useRef, useState } from 'react';
import './App.css';
import { TonConnectButton, useTonAddress, useTonConnectUI, useTonWallet } from "@tonconnect/ui-react";

import { Send, ArrowDown, ArrowUp, RefreshCw, Copy, PlusCircle, MinusCircle, Activity, Check, Plus, Repeat, X, TrendingUp, TrendingDown } from "lucide-react";
import { Card, CardContent } from "./components/ui/card";
import { Input } from "./components/ui/input";
import { Button } from "./components/ui/button";
import {
  DerivativesOrderBookLevel,
  DerivativesOrderBookVolume,
  DerivativesPosition,
  DerivativesQuote,
  DerivativesVolumePoint,
  PaymentWalletRequestEvent,
  PaymentChannelHistoryItem,
  PriceHistoryPoint,
  TxMessage,
} from "./index";

const derivativeBookVolumeEndpoint = "/web-channel/api/v1/derivatives/book_volume";

const fetchDerivativeBookVolume = async (symbol: string): Promise<DerivativesOrderBookVolume> => {
  const query = new URLSearchParams({
    symbol,
    depth: "100",
    volume_limit: "180",
  });
  const response = await fetch(`${derivativeBookVolumeEndpoint}?${query.toString()}`);
  if (!response.ok) {
    let text = "";
    try {
      text = await response.text();
    } catch (_) {
      text = "";
    }
    throw new Error(text || `failed to load order book and volume (${response.status})`);
  }
  return await response.json();
};

const formatCompactNumber = (raw: string, maxFractionDigits = 3): string => {
  const val = Number.parseFloat(raw);
  if (!Number.isFinite(val)) {
    return raw;
  }
  return val.toLocaleString(undefined, { maximumFractionDigits: maxFractionDigits });
};

const formatPriceNumber = (raw: string): string => {
  const val = Number.parseFloat(raw);
  if (!Number.isFinite(val)) {
    return raw;
  }
  const decimals = Math.abs(val) >= 1000 ? 2 : 4;
  return val.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: decimals });
};

const resolveGroupingOptions = (symbol: string): number[] => {
  if (symbol === "BTCUSDT") {
    return [0, 1, 5, 10, 25, 50, 100];
  }
  if (symbol === "TONUSDT") {
    return [0, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5];
  }
  return [0, 0.01, 0.05, 0.1, 0.5, 1, 5];
};

const formatGroupingLabel = (step: number): string => {
  if (step <= 0) {
    return "Raw";
  }
  return formatCompactNumber(step.toString(), 6);
};

const groupOrderBookLevels = (levels: DerivativesOrderBookLevel[], step: number, side: "ask" | "bid"): DerivativesOrderBookLevel[] => {
  if (step <= 0) {
    return levels;
  }

  const stepStr = step.toString();
  const decimals = stepStr.includes(".") ? Math.min(8, stepStr.split(".")[1].length) : 0;
  const grouped = new Map<string, number>();
  for (const lvl of levels) {
    const price = Number.parseFloat(lvl.price);
    const qty = Number.parseFloat(lvl.quantity);
    if (!Number.isFinite(price) || !Number.isFinite(qty) || qty <= 0) {
      continue;
    }

    const scaled = price / step;
    const bucket = (side === "ask" ? Math.ceil(scaled) : Math.floor(scaled)) * step;
    const key = bucket.toFixed(decimals);
    grouped.set(key, (grouped.get(key) || 0) + qty);
  }

  const out = Array.from(grouped.entries()).map(([price, quantity]) => ({
    price,
    quantity: quantity.toFixed(8).replace(/\.?0+$/, ""),
  }));
  out.sort((a, b) => {
    const ap = Number.parseFloat(a.price);
    const bp = Number.parseFloat(b.price);
    if (side === "ask") {
      return ap - bp;
    }
    return bp - ap;
  });
  return out;
};


function App() {
  const [tonConnectUI] = useTonConnectUI();
  const wallet = useTonWallet();
  let addr = useTonAddress();
  const wasmInitializedRef = useRef(false);
  const [derivativesEnabled, setDerivativesEnabled] = useState(false);
  let [paymentAddr, setPaymentAddr] = useState("Loading...");
  let [balances, setBalances] = useState<Record<string, string>>({});
  let [capacities, setCapacities] = useState<Record<string, string>>({});
  let [lockedBalance, setLockedBalance] = useState<Record<string, string>>({});
  let [pendingIn, setPendingIn] = useState<Record<string, string>>({});
  let [history, setHistory] = useState<PaymentChannelHistoryItem[] | null>(null);
  let [supportedCurrencies, setSupportedCurrencies] = useState<string[]>(["TON"]);
  const [walletRequestStatus, setWalletRequestStatus] = useState<PaymentWalletRequestEvent | null>(null);
  const [uncooperativeCloseWarning, setUncooperativeCloseWarning] = useState(false);
  const [uncooperativeCloseApprovals, setUncooperativeCloseApprovals] = useState(0);

  const updateDerivativesAvailability = () => {
    if (typeof window.isDerivativesEnabled !== "function") {
      setDerivativesEnabled(false);
      return;
    }

    try {
      setDerivativesEnabled(Boolean(window.isDerivativesEnabled()));
    } catch (e) {
      console.error(e);
      setDerivativesEnabled(false);
    }
  };

  window.onPaymentNetworkLoaded = function (addr) {
    setPaymentAddr(addr);
    updateDerivativesAvailability();
    console.log("Payment network loaded: " + addr);
  }
  window.onPaymentChannelUpdated = function (ev) {
    console.log("Payment channel updated: " + JSON.stringify(ev));

    setBalances(ev.balances || {});
    setCapacities(ev.capacities || {});
    setLockedBalance(ev.locked || {});
    setPendingIn(ev.pendingIn || {});
    setUncooperativeCloseWarning(Boolean(ev.uncooperativeClose));
    setUncooperativeCloseApprovals(Number(ev.expectedWalletApprovals || 0));

    const currencies = Array.from(new Set([
      ...Object.keys(ev.balances || {}),
      ...Object.keys(ev.capacities || {}),
      ...Object.keys(ev.locked || {}),
      ...Object.keys(ev.pendingIn || {}),
    ]));

    // Ensure deterministic order: TON first, then alphabetically by symbol
    const currenciesSorted = currencies.sort((a, b) => {
      if (a === 'TON' && b !== 'TON') return -1;
      if (b === 'TON' && a !== 'TON') return 1;
      return a.localeCompare(b);
    });

    if (currenciesSorted.length === 0) {
      setSupportedCurrencies(["TON"]);
      return;
    }

    setSupportedCurrencies(currenciesSorted);

    window.getChannelHistory(5).then(history => {
      setHistory(history);
    }).catch(e => {
      console.error(e);
    });

    updateDerivativesAvailability();
  }

  window.onPaymentChannelHistoryUpdated = function () {
    window.getChannelHistory(5).then(history => {
      setHistory(history);
    }).catch(e => {
      console.error(e);
    });
    updateDerivativesAvailability();
  }

  useEffect(() => {
    if (!wallet) {
      setDerivativesEnabled(false);
      setWalletRequestStatus(null);
      setUncooperativeCloseWarning(false);
      setUncooperativeCloseApprovals(0);
      return;
    }

    window.walletAddress = () => {
      return addr;
    };

    updateDerivativesAvailability();
    const derivativesAvailabilityInterval = window.setInterval(updateDerivativesAvailability, 1000);

    window.doTransaction = async (reason: string, messages: TxMessage[]) => {
      console.log("requested tx: " + reason);

      const list: Array<{ address: string; amount: string; payload?: string; stateInit?: string }> = [];
      for (let i = 0; i < messages.length; i++) {
        const msg: { address: string; amount: string; payload?: string; stateInit?: string } = {
          address: messages[i].to,
          amount: messages[i].amtNano,
        };
        if (messages[i].body) {
          msg.payload = messages[i].body;
        }
        if (messages[i].stateInit) {
          msg.stateInit = messages[i].stateInit;
        }
        list.push(msg);
      }

      const transaction = {
        validUntil: Math.floor(Date.now() / 1000) + 300,
        network: wallet.account.chain,
        messages: list
      }

      let resp = await tonConnectUI.sendTransaction(transaction);
      return resp.boc;
    };
    window.onPaymentWalletRequestUpdated = (ev: PaymentWalletRequestEvent) => {
      console.log("wallet request update", ev);
      setWalletRequestStatus(ev);
    };

    if (wasmInitializedRef.current) {
      return () => {
        window.clearInterval(derivativesAvailabilityInterval);
      };
    }
    wasmInitializedRef.current = true;

    const initWasm = async () => {
      const go = new (window as any).Go();
      const wasmUrl = 'web.wasm';
      let wasmModule;

      if ('instantiateStreaming' in WebAssembly) {
        wasmModule = await WebAssembly.instantiateStreaming(fetch(wasmUrl), go.importObject);
      } else {
        const resp = await fetch(wasmUrl);
        const bytes = await resp.arrayBuffer();
        wasmModule = await WebAssembly.instantiate(bytes, go.importObject);
      }

      go.run(wasmModule.instance);

      const waitForStartPaymentNetwork = (timeoutMs: number = 5000, intervalMs: number = 50): Promise<void> => {
        return new Promise((resolve, reject) => {
          const startTime = Date.now();
          const interval = setInterval(() => {
            if (typeof window.startPaymentNetwork === 'function') {
              clearInterval(interval);
              resolve();
            } else if (Date.now() - startTime > timeoutMs) {
              clearInterval(interval);
              reject(new Error('startPaymentNetwork was not registered in time'));
            }
          }, intervalMs);
        });
      };

      try {
        await waitForStartPaymentNetwork();
        window.startPaymentNetwork("tAHpSEpUcxpxfqNJVZzYa+5ktseCKMZOw5yMoJnSW4s=", "zT4aAGrfYw57jTWElGQPFPHzqGzaRgpThLaAeUk9sps=");
        updateDerivativesAvailability();
      } catch (e) {
        wasmInitializedRef.current = false;
        setDerivativesEnabled(false);
        console.error(e);
      }
    };

    initWasm();
    return () => {
      window.clearInterval(derivativesAvailabilityInterval);
    };
  }, [wallet, addr, tonConnectUI]);

  useEffect(() => {
    if (!walletRequestStatus) {
      return;
    }
    if (walletRequestStatus.phase === "queued" || walletRequestStatus.phase === "requested") {
      return;
    }

    const timeout = window.setTimeout(() => {
      setWalletRequestStatus((current) => {
        if (!current || current.at !== walletRequestStatus.at) {
          return current;
        }
        return null;
      });
    }, walletRequestStatus.phase === "submitted" ? 5000 : 10000);

    return () => {
      window.clearTimeout(timeout);
    };
  }, [walletRequestStatus]);

  if (!wallet) {
    return (
      <div className="min-h-screen bg-white text-gray-800 p-6 space-y-6">
        <div className="flex justify-between items-center">
          <h1 className="text-2xl font-bold text-[#0098ea]">TON Payments Wallet</h1>
          <TonConnectButton />
        </div>
      </div>
    );
  }

  return (
    <WalletUI
      paymentAddr={paymentAddr}
      balances={balances}
      locked={lockedBalance}
      capacities={capacities}
      pendingIn={pendingIn}
      transactions={history}
      currencies={supportedCurrencies}
      derivativesEnabled={derivativesEnabled}
      walletRequestStatus={walletRequestStatus}
      uncooperativeCloseWarning={uncooperativeCloseWarning}
      uncooperativeCloseApprovals={uncooperativeCloseApprovals}
    />
  );
}

type WalletUIProps = {
  paymentAddr: string;
  balances: Record<string, string>;
  capacities: Record<string, string>;
  pendingIn: Record<string, string>;
  locked: Record<string, string>;
  currencies: string[];
  transactions: PaymentChannelHistoryItem[] | null;
  derivativesEnabled: boolean;
  walletRequestStatus: PaymentWalletRequestEvent | null;
  uncooperativeCloseWarning: boolean;
  uncooperativeCloseApprovals: number;
};

type LimitOrderConfirmState = {
  side: "long" | "short";
  symbol: string;
  leverage: number;
  amount: string;
  orderType: "market" | "limit";
  limitPrice: string;
  currentPrice: string;
  diffPercent: number;
};

type OpeningDerivativeDraft = {
  id: string;
  symbol: string;
  isLong: boolean;
  leverage: number;
  amount: string;
  createdAt: number;
};

const walletRequestTitle = (phase: PaymentWalletRequestEvent["phase"]): string => {
  switch (phase) {
    case "queued":
      return "Wallet request queued";
    case "requested":
      return "Approve transaction in wallet";
    case "submitted":
      return "Wallet transaction submitted";
    case "failed":
      return "Wallet transaction failed";
  }
};

const WalletUI: React.FC<WalletUIProps> = ({ paymentAddr, balances, locked, capacities, pendingIn, transactions, currencies, derivativesEnabled, walletRequestStatus, uncooperativeCloseWarning, uncooperativeCloseApprovals }) => {
  const [sendTo, setSendTo] = useState("");
  const [sendAmount, setSendAmount] = useState("");
  const [sendFeeAmount, setSendFeeAmount] = useState("");
  const [sendCurrency, setSendCurrency] = useState<string>(currencies[0] ?? "TON");
  const [copied, setCopied] = useState(false);
  const [creationStarted, setCreationStarted] = useState(false);
  const [modalType, setModalType] = useState<"topup" | "withdraw" | null>(null);
  const [modalCurrency, setModalCurrency] = useState<string>(currencies[0] ?? "TON");
  const [modalAmount, setModalAmount] = useState("");
  const [withdrawTarget, setWithdrawTarget] = useState("");
  const [transferStatus, setTransferStatus] = useState<"loading" | "success" | null>(null);
  const [swapModalOpen, setSwapModalOpen] = useState(false);
  const [swapFromCurrency, setSwapFromCurrency] = useState("TON");
  const [swapToCurrency, setSwapToCurrency] = useState("USDX");
  const [swapFromAmount, setSwapFromAmount] = useState("");
  const [swapToAmount, setSwapToAmount] = useState("");
  const [derivativesModalOpen, setDerivativesModalOpen] = useState(false);
  const [derivativeSymbol, setDerivativeSymbol] = useState("BTCUSDT");
  const [derivativeAmount, setDerivativeAmount] = useState("");
  const [derivativeLeverage, setDerivativeLeverage] = useState("3");
  const [derivativesPositions, setDerivativesPositions] = useState<DerivativesPosition[]>([]);
  const [derivativeQuote, setDerivativeQuote] = useState<DerivativesQuote | null>(null);
  const [derivativesLoading, setDerivativesLoading] = useState(false);
  const [derivativeActionLoading, setDerivativeActionLoading] = useState<"long" | "short" | "close" | "cancel" | null>(null);
  const [cancellingDerivativeIds, setCancellingDerivativeIds] = useState<string[]>([]);
  const [derivativeLimitPrice, setDerivativeLimitPrice] = useState("");
  const [derivativeOrderType, setDerivativeOrderType] = useState<"market" | "limit">("market");
  const [priceHistory, setPriceHistory] = useState<PriceHistoryPoint[]>([]);
  const [orderBookVolume, setOrderBookVolume] = useState<DerivativesOrderBookVolume | null>(null);
  const [orderBookVolumeLoading, setOrderBookVolumeLoading] = useState(true);
  const [limitOrderConfirm, setLimitOrderConfirm] = useState<LimitOrderConfirmState | null>(null);
  const [openingDerivativeDraft, setOpeningDerivativeDraft] = useState<OpeningDerivativeDraft | null>(null);

  const availableCurrencies = useMemo(() => currencies.length ? currencies : ["TON"], [currencies]);
  const swapPairs = useMemo(() => [{ from: "TON", to: "USDX", coeff: 2 }], []);
  const activeSwapPair = useMemo(() => swapPairs.find(p => p.from === swapFromCurrency && p.to === swapToCurrency) ?? swapPairs[0], [swapPairs, swapFromCurrency, swapToCurrency]);
  const derivativeSymbols = useMemo(() => ["BTCUSDT", "TONUSDT"], []);

  const syncCancellingDerivativeIds = (positions: DerivativesPosition[]) => {
    const activeIDs = new Set(positions.map((p) => p.id));
    setCancellingDerivativeIds((prev) => prev.filter((id) => activeIDs.has(id)));
  };

  const syncOpeningDerivativeDraft = (positions: DerivativesPosition[]) => {
    setOpeningDerivativeDraft((prev) => {
      if (!prev) {
        return prev;
      }
      const exists = positions.some((p) => p.id === prev.id);
      if (exists) {
        return null;
      }
      return prev;
    });
  };

  useEffect(() => {
    if (!availableCurrencies.includes(sendCurrency)) {
      setSendCurrency(availableCurrencies[0]);
    }
    if (!availableCurrencies.includes(modalCurrency)) {
      setModalCurrency(availableCurrencies[0]);
    }
  }, [availableCurrencies, sendCurrency, modalCurrency]);

  useEffect(() => {
    if (!swapFromAmount || !activeSwapPair) {
      return;
    }

    const numeric = parseFloat(swapFromAmount);
    if (isNaN(numeric)) {
      setSwapToAmount("");
      return;
    }

    setSwapToAmount((numeric * activeSwapPair.coeff).toString());
  }, [activeSwapPair, swapFromAmount]);

  // Poll positions + quote + price history + order book/volume every second when modal is open
  useEffect(() => {
    if (!derivativesEnabled || !derivativesModalOpen) {
      return;
    }
    setOrderBookVolume(null);
    setOrderBookVolumeLoading(true);
    let first = true;
    const fetchAll = () => {
      if (first) {
        setDerivativesLoading(true);
      }
      Promise.allSettled([
        window.getDerivativesPositions(),
        window.getDerivativeMarketPrice(derivativeSymbol),
        window.getDerivativePriceHistory(derivativeSymbol),
        fetchDerivativeBookVolume(derivativeSymbol),
      ]).then((results) => {
        const [positionsRes, quoteRes, historyRes, bookVolumeRes] = results;
        if (positionsRes.status === "fulfilled") {
          const nextPositions = positionsRes.value ?? [];
          setDerivativesPositions(nextPositions);
          syncCancellingDerivativeIds(nextPositions);
          syncOpeningDerivativeDraft(nextPositions);
        } else {
          console.error(positionsRes.reason);
        }

        if (quoteRes.status === "fulfilled") {
          setDerivativeQuote(quoteRes.value ?? null);
        } else {
          console.error(quoteRes.reason);
        }

        if (historyRes.status === "fulfilled") {
          setPriceHistory(historyRes.value ?? []);
        } else {
          console.error(historyRes.reason);
        }

        if (bookVolumeRes.status === "fulfilled") {
          setOrderBookVolume(bookVolumeRes.value ?? null);
          setOrderBookVolumeLoading(false);
        } else {
          console.error(bookVolumeRes.reason);
        }
      }).finally(() => {
        if (first) {
          setDerivativesLoading(false);
          first = false;
        }
      });
    };
    fetchAll();
    const iv = window.setInterval(fetchAll, 1000);
    return () => window.clearInterval(iv);
  }, [derivativesEnabled, derivativesModalOpen, derivativeSymbol]);

  useEffect(() => {
    if (!derivativesEnabled) {
      setDerivativesModalOpen(false);
      setDerivativesPositions([]);
      setDerivativeQuote(null);
      setDerivativesLoading(false);
      setDerivativeActionLoading(null);
      setCancellingDerivativeIds([]);
      setOrderBookVolume(null);
      setOrderBookVolumeLoading(true);
      setLimitOrderConfirm(null);
      setOpeningDerivativeDraft(null);
    }
  }, [derivativesEnabled]);

  const handleCopy = () => {
    if (!paymentAddr) return;
    navigator.clipboard.writeText(paymentAddr);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  const closeModal = () => {
    setModalType(null);
    setModalAmount("");
    setWithdrawTarget("");
  };

  const confirmModal = () => {
    if (modalType === "topup") {
      window.topupChannel(modalAmount, modalCurrency);
    } else if (modalType === "withdraw") {
      if (!withdrawTarget) {
        alert("Please enter withdraw address");
        return;
      }
      window.withdrawChannel(modalAmount, modalCurrency, withdrawTarget);
    }
    closeModal();
  };

  const formatAmounts = (map?: Record<string, string>) => {
    if (!map) return "";
    const entries = Object.entries(map).sort(([a], [b]) => {
      if (a === 'TON' && b !== 'TON') return -1;
      if (b === 'TON' && a !== 'TON') return 1;
      return a.localeCompare(b);
    });
    return entries
      .map(([symbol, amount]) => `${symbol}: ${amount}`)
      .join(", ");
  };

  const updateFeeEstimate = (amount: string, recipient: string, currency: string) => {
    const val = parseFloat(amount);
    if (isNaN(val) || !recipient) {
      setSendFeeAmount("");
      return;
    }

    const fee = window.estimateTransfer(amount, recipient, currency);
    setSendFeeAmount(fee);
  };

  const updateSwapPreview = (amount: string, coefficient?: number) => {
    const coeff = coefficient ?? activeSwapPair?.coeff ?? 1;
    setSwapFromAmount(amount);
    const numeric = parseFloat(amount);
    if (isNaN(numeric)) {
      setSwapToAmount("");
      return;
    }
    setSwapToAmount((numeric * coeff).toString());
  };

  const handleSwapPairChange = (from: string) => {
    const nextPair = swapPairs.find(p => p.from === from) ?? swapPairs[0];
    setSwapFromCurrency(nextPair.from);
    setSwapToCurrency(nextPair.to);
    updateSwapPreview(swapFromAmount, nextPair.coeff);
  };

  const confirmSwap = () => {
    const pair = activeSwapPair;
    if (!pair) {
      alert("Selected swap pair is not available");
      return;
    }
    if (!swapFromAmount || parseFloat(swapFromAmount) <= 0) {
      alert("Please enter amount to swap");
      return;
    }
    if (!availableCurrencies.includes(pair.from) || !availableCurrencies.includes(pair.to)) {
      alert("Selected currencies are not available");
      return;
    }

    setTransferStatus("loading");
    window.executeSwap(pair.from, pair.to, swapFromAmount, pair.coeff).then(() => {
      setTransferStatus("success");
      setSwapModalOpen(false);
      setSwapFromAmount("");
      setSwapToAmount("");
    }).catch(err => {
      setTransferStatus(null);
      alert(err);
      console.error(err);
    });
  };

  const refreshDerivatives = () => {
    if (!derivativesEnabled) {
      return;
    }

    setDerivativesLoading(true);
    Promise.allSettled([
      window.getDerivativesPositions(),
      window.getDerivativeMarketPrice(derivativeSymbol),
      window.getDerivativePriceHistory(derivativeSymbol),
      fetchDerivativeBookVolume(derivativeSymbol),
    ]).then((results) => {
      const [positionsRes, quoteRes, historyRes, bookVolumeRes] = results;

      if (positionsRes.status === "fulfilled") {
        const nextPositions = positionsRes.value ?? [];
        setDerivativesPositions(nextPositions);
        syncCancellingDerivativeIds(nextPositions);
        syncOpeningDerivativeDraft(nextPositions);
      }

      if (quoteRes.status === "fulfilled") {
        const quote = quoteRes.value ?? null;
        setDerivativeQuote(quote);
        if (derivativeOrderType === "limit" && quote?.price && !derivativeLimitPrice) {
          setDerivativeLimitPrice(quote.price);
        }
      }

      if (historyRes.status === "fulfilled") {
        setPriceHistory(historyRes.value ?? []);
      }

      if (bookVolumeRes.status === "fulfilled") {
        setOrderBookVolume(bookVolumeRes.value ?? null);
        setOrderBookVolumeLoading(false);
      }
    }).catch(err => {
      alert(err);
      console.error(err);
    }).finally(() => {
      setDerivativesLoading(false);
    });
  };

  const openDerivativesModal = () => {
    if (!derivativesEnabled) {
      return;
    }

    setDerivativesModalOpen(true);
  };

  const executeOpenDerivative = (
    symbol: string,
    side: "long" | "short",
    leverageNum: number,
    amount: string,
    orderType: "market" | "limit",
    limitPrice?: string
  ) => {
    setDerivativeActionLoading(side);
    window.openDerivativePosition(
      symbol,
      side,
      leverageNum,
      amount,
      orderType,
      orderType === "limit" ? limitPrice : undefined
    ).then((id) => {
      setDerivativeAmount("");
      setDerivativeActionLoading(null);
      setLimitOrderConfirm(null);
      if (typeof id === "string" && id.trim() !== "") {
        setOpeningDerivativeDraft({
          id: id.trim(),
          symbol,
          isLong: side === "long",
          leverage: leverageNum,
          amount,
          createdAt: Date.now(),
        });
      }
      refreshDerivatives();
    }).catch(err => {
      setDerivativeActionLoading(null);
      alert(err);
      console.error(err);
    });
  };

  const openDerivative = (side: "long" | "short") => {
    if (!derivativeAmount || parseFloat(derivativeAmount) <= 0) {
      alert("Please enter collateral amount");
      return;
    }

    const leverageNum = parseInt(derivativeLeverage, 10);
    if (!Number.isFinite(leverageNum) || leverageNum <= 0) {
      alert("Please enter valid leverage");
      return;
    }

    if (derivativeOrderType === "limit" && (!derivativeLimitPrice || parseFloat(derivativeLimitPrice) <= 0)) {
      alert("Please enter limit price");
      return;
    }

    if (derivativeOrderType === "limit" && derivativeQuote?.price) {
      const current = parseFloat(derivativeQuote.price);
      const desired = parseFloat(derivativeLimitPrice);
      if (Number.isFinite(current) && current > 0 && Number.isFinite(desired) && desired > 0) {
        const diffPercent = Math.abs(((desired - current) / current) * 100);
        if (diffPercent >= 10) {
          setLimitOrderConfirm({
            side,
            symbol: derivativeSymbol,
            leverage: leverageNum,
            amount: derivativeAmount,
            orderType: derivativeOrderType,
            limitPrice: derivativeLimitPrice,
            currentPrice: derivativeQuote.price,
            diffPercent,
          });
          return;
        }
      }
    }

    executeOpenDerivative(derivativeSymbol, side, leverageNum, derivativeAmount, derivativeOrderType, derivativeLimitPrice);
  };

  const closeDerivative = (positionId: string) => {
    setDerivativeActionLoading("close");
    window.closeDerivativePosition(positionId, "market").then(() => {
      setDerivativeActionLoading(null);
      refreshDerivatives();
    }).catch(err => {
      setDerivativeActionLoading(null);
      alert(err);
      console.error(err);
    });
  };

  const cancelDerivative = (positionId: string) => {
    setCancellingDerivativeIds((prev) => prev.includes(positionId) ? prev : [...prev, positionId]);
    window.closeDerivativePosition(positionId, "cancel").then(() => {
      // Keep loading on the order until it disappears from the next refresh/poll.
      refreshDerivatives();
    }).catch(err => {
      setCancellingDerivativeIds((prev) => prev.filter((id) => id !== positionId));
      alert(err);
      console.error(err);
    });
  };

  return (
    <div className="min-h-screen bg-white text-gray-800 p-4 sm:p-6 flex justify-center">
      <div className="w-full max-w-xl space-y-6">
        <div className="flex justify-between items-center">
          <h1 className="text-2xl font-bold text-[#0098ea]">TON Payments Wallet</h1>
          <h2 className="text-lg font-bold text-[#772233]">Testnet</h2>
          <TonConnectButton />
        </div>

        {walletRequestStatus && (
          <Card className={
            walletRequestStatus.phase === "failed"
              ? "border-red-200 bg-red-50 shadow-sm rounded-2xl"
              : walletRequestStatus.phase === "submitted"
                ? "border-emerald-200 bg-emerald-50 shadow-sm rounded-2xl"
                : "border-amber-200 bg-amber-50 shadow-sm rounded-2xl"
          }>
            <CardContent className="p-4">
              <div className="flex items-start gap-3">
                {walletRequestStatus.phase === "failed" ? (
                  <X className="w-5 h-5 text-red-600 mt-0.5" />
                ) : walletRequestStatus.phase === "submitted" ? (
                  <Check className="w-5 h-5 text-emerald-600 mt-0.5" />
                ) : (
                  <Activity className="w-5 h-5 text-amber-600 mt-0.5" />
                )}
                <div className="space-y-1 min-w-0">
                  <div className="text-sm font-semibold">
                    {walletRequestTitle(walletRequestStatus.phase)}
                  </div>
                  <div className="text-sm break-words">
                    {walletRequestStatus.reason}
                  </div>
                  {walletRequestStatus.details ? (
                    <div className="text-xs opacity-80 break-all">
                      {walletRequestStatus.details}
                    </div>
                  ) : (
                    <div className="text-xs opacity-80">
                      Messages: {walletRequestStatus.messages}
                    </div>
                  )}
                </div>
              </div>
            </CardContent>
          </Card>
        )}

        {uncooperativeCloseWarning && (
          <Card className="border-amber-200 bg-amber-50 shadow-sm rounded-2xl">
            <CardContent className="p-4">
              <div className="flex items-start gap-3">
                <Activity className="w-5 h-5 text-amber-600 mt-0.5" />
                <div className="space-y-1 min-w-0">
                  <div className="text-sm font-semibold">
                    Uncooperative close in progress
                  </div>
                  <div className="text-sm">
                    Keep this page open until the channel close is completed.
                  </div>
                  <div className="text-xs opacity-80">
                    {uncooperativeCloseApprovals > 0
                      ? `Up to ${uncooperativeCloseApprovals} wallet approvals may be required.`
                      : "Additional wallet approvals may still be required."}
                  </div>
                </div>
              </div>
            </CardContent>
          </Card>
        )}

        <Card className="bg-[#f0f8ff] shadow-md rounded-2xl">
          <CardContent className="p-6 space-y-4">
            <h2 className="text-xl font-semibold">Balance</h2>
            <div className="space-y-2">
              {availableCurrencies.map((c) => (
                <div key={c} className="flex items-center justify-between">
                  <div className="text-lg text-[#0098ea] font-semibold">{balances[c] ?? "0"} {c}</div>
                  <div className="space-x-2">
                    {balances[c] === undefined && capacities[c] === undefined ? (
                      creationStarted ? (
                        <Button className="bg-gray-200 text-gray-700 px-4 py-2 rounded-xl" disabled>
                          <RefreshCw className="animate-spin inline mr-2" size={16} />
                          Creating...
                        </Button>
                      ) : (
                        <Button
                          onClick={() => {
                            setCreationStarted(true);
                            window.openChannel();
                          }}
                          disabled={paymentAddr === "Loading..."}
                          aria-disabled={paymentAddr === "Loading..."}
                          title={paymentAddr === "Loading..." ? "Please wait until Your Address is loaded" : undefined}
                          className={`px-4 py-2 rounded-xl ${paymentAddr === "Loading..." ? "bg-gray-200 text-gray-500 cursor-not-allowed" : "bg-[#0098ea] text-white"}`}
                        >
                          Create Wallet
                        </Button>
                      )
                    ) : (
                      <>
                        <Button
                          onClick={() => { setModalType("topup"); setModalCurrency(c); }}
                          className="bg-[#0098ea] text-white px-3 py-1 rounded-lg text-sm"
                        >
                          Top Up
                        </Button>
                        <Button
                          onClick={() => { setModalType("withdraw"); setModalCurrency(c); }}
                          className="bg-gray-200 text-gray-700 px-3 py-1 rounded-lg text-sm"
                        >
                          Withdraw
                        </Button>
                      </>
                    )}
                  </div>
                </div>
              ))}
            </div>

            {availableCurrencies.map((c) => (
              <div key={`capacity-${c}`} className="flex items-center justify-between mt-1">
                <span className="text-sm text-gray-500">Receive Capacity ({c})</span>
                <span className="text-sm font-medium">{capacities[c] ?? "0"} {c}</span>
              </div>
            ))}

            {Object.keys(locked).length > 0 ?
              <div className="mt-1 space-y-1">
                {Object.entries(locked).map(([c, val]) => (
                  <div key={`locked-${c}`} className="flex items-center justify-between">
                    <span className="text-sm text-gray-500">Balance on hold ({c})</span>
                    <span className="text-sm font-medium">{val} {c}</span>
                  </div>
                ))}
              </div> : ""}

            {Object.keys(pendingIn).length > 0 ?
              <div className="mt-1 space-y-1">
                {Object.entries(pendingIn).map(([c, val]) => (
                  <div key={`pending-${c}`} className="flex items-center justify-between">
                    <span className="text-sm text-gray-500">Pending incoming amount ({c})</span>
                    <span className="text-sm font-medium">{val} {c}</span>
                  </div>
                ))}
              </div> : ""}

            <h2 className="text-xl font-semibold">Your Address</h2>
            {paymentAddr === "Loading..." ? (
              <div className="text-gray-500 flex items-center gap-2">
                <RefreshCw className="animate-spin" size={18} /> Loading...
              </div>
            ) : paymentAddr === "" ? (
              <div className="relative bg-gradient-to-r from-[#f0f8ff] to-white border border-[#cce5ff] rounded-xl px-4 py-3">
                <div className="text-xs text-gray-700 font-mono truncate pr-10">{"Not deployed"}</div>
              </div>
            ) : (
              <div className="relative bg-gradient-to-r from-[#f0f8ff] to-white border border-[#cce5ff] rounded-xl px-4 py-3">
                <div className="text-xs text-gray-700 font-mono truncate pr-10">{paymentAddr}</div>
                <button
                  onClick={handleCopy}
                  className="absolute top-1/2 right-3 -translate-y-1/2 text-[#0098ea] hover:text-blue-600"
                >
                  {copied ? <span className="text-sm animate-pulse">Copied!</span> : <Copy size={16} />}
                </button>
              </div>
            )}
          </CardContent>
        </Card>

        <Card className="bg-[#f9fcff] shadow-md rounded-2xl">
          <CardContent className="p-6 space-y-4">
            <h2 className="text-xl font-semibold">Send</h2>
            <div className="flex items-center gap-3">
              <span className="text-sm text-gray-600">Currency</span>
              <select
                className="border border-gray-200 rounded-lg px-3 py-2 text-sm bg-white"
                value={sendCurrency}
                onChange={(e) => {
                  setSendCurrency(e.target.value);
                  updateFeeEstimate(sendAmount, sendTo, e.target.value);
                }}
              >
                {availableCurrencies.map((c) => (
                  <option key={`send-${c}`} value={c}>{c}</option>
                ))}
              </select>
            </div>

            <Input placeholder="Recipient address" value={sendTo} onChange={(e) => {
              setSendTo(e.target.value)
              updateFeeEstimate(sendAmount, e.target.value, sendCurrency);
            }} />
            <Input placeholder={`Amount in ${sendCurrency}`} value={sendAmount} onChange={(e) => {
              setSendAmount(e.target.value);
              updateFeeEstimate(e.target.value, sendTo, sendCurrency);
            }} />
            <div className="flex items-center justify-between mt-2">
              <Button disabled={!sendAmount || !sendTo} className="bg-[#0098ea] text-white px-4 py-2 rounded-xl flex items-center gap-2 disabled:bg-gray-300" onClick={() => {
                setTransferStatus("loading");
                setSendFeeAmount("");

                window.sendTransfer(sendAmount, sendTo, sendCurrency).then(res => {
                  setTransferStatus("success");
                  console.log("transferred: " + sendAmount + " " + sendCurrency + " to " + sendTo);
                  setSendAmount("");
                  setSendTo("");
                }).catch(err => {
                  setTransferStatus(null);
                  alert(err);
                  console.error(err);
                });

              }}>
                <Send size={16} /> Send
              </Button>
              {sendFeeAmount ? <span className="text-sm text-gray-500">Fee: {sendFeeAmount} {sendCurrency}</span> : ""}
            </div>
          </CardContent>
        </Card>

        <Card className="bg-[#f9fcff] shadow-md rounded-2xl">
          <CardContent className="p-6 space-y-4">
            <div className="flex items-center justify-between">
              <h2 className="text-xl font-semibold">Swap</h2>
              {activeSwapPair && (
                <span className="text-sm text-gray-600">Rate: 1 {activeSwapPair.from} = {activeSwapPair.coeff} {activeSwapPair.to}</span>
              )}
            </div>
            <p className="text-sm text-gray-600">Swap TON to USDX using the fixed rate.</p>
            <Button className="bg-[#0098ea] text-white px-4 py-2 rounded-xl flex items-center gap-2" onClick={() => {
              const pair = swapPairs[0];
              setSwapModalOpen(true);
              setSwapFromCurrency(pair.from);
              setSwapToCurrency(pair.to);
              updateSwapPreview("", pair.coeff);
            }}>
              <Repeat size={16} /> Start Swap
            </Button>
          </CardContent>
        </Card>

        {derivativesEnabled && (
          <Card className="bg-[#f9fcff] shadow-md rounded-2xl">
            <CardContent className="p-6 space-y-4">
              <div className="flex items-center justify-between">
                <h2 className="text-xl font-semibold">Derivatives</h2>
                <span className="text-sm text-gray-600">Open positions: {derivativesPositions.length}</span>
              </div>
              <p className="text-sm text-gray-600">Open/close trustless derivative positions in your channel.</p>
              <Button className="bg-[#0098ea] text-white px-4 py-2 rounded-xl flex items-center gap-2" onClick={openDerivativesModal}>
                <Activity size={16} /> Derivatives
              </Button>
            </CardContent>
          </Card>
        )}

        {transactions && (
          <Card className="bg-[#f9fcff] shadow-md rounded-2xl">
            <CardContent className="p-6 space-y-4">
              <h2 className="text-xl font-semibold">History</h2>

              <div className="space-y-2">
                {transactions.map((tx) => (
                  <div
                    key={tx.id}
                    className="flex justify-between items-center border-b border-gray-100 pb-2"
                  >
                    <div className="flex items-center gap-2">
                      {(() => {
                        const p = { size: 16 };
                        switch (tx.action) {
                          case 1: // Balance changed
                            return <RefreshCw className="text-blue-500"  {...p} />;
                          case 2: // Transfer-in
                            return <ArrowDown className="text-green-500" {...p} />;
                          case 3: // Transfer-out
                            return <ArrowUp className="text-red-500"   {...p} />;
                          case 4: // Uncooperative close
                            return <Activity className="text-orange-500" {...p} />;
                          case 5: // Closed
                            return <Check className="text-gray-500"  {...p} />;
                          case 6: // Their capacity rented
                            return <PlusCircle className="text-green-500" {...p} />;
                          case 7: // Our capacity rented
                            return <Plus className="text-green-500" {...p} />;
                          case 8: // Withdraw transaction request
                            return <MinusCircle className="text-red-500"   {...p} />;
                          default:
                            return <ArrowDown   {...p} />;
                        }
                      })()}

                      <span className="text-sm text-gray-600">{tx.timestamp}</span>
                    </div>

                    <div className="flex flex-col items-end">
                      {tx.amounts && (
                        <div className="text-sm font-medium text-right">{formatAmounts(tx.amounts)}</div>
                      )}
                      {tx.isTheir !== undefined && (
                        <div className="text-xs text-gray-500">{tx.isTheir ? "Counterparty balance changes" : "Our balance changes"}</div>
                      )}

                      {tx.party && (
                        <button
                          className="group flex items-center gap-1 text-xs text-blue-600 hover:underline"
                          onClick={() => navigator.clipboard.writeText(tx.party!)}
                          title="Copy address"
                        >
                          <Copy
                            size={12}
                            className="opacity-70 group-hover:opacity-100"
                          />
                          {tx.party.slice(0, 4)}…{tx.party.slice(-4)}
                        </button>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            </CardContent>
          </Card>
        )}
      </div>

      {modalType && (
        <ModalAmount
          title={modalType}
          value={modalAmount}
          currency={modalCurrency}
          currencies={availableCurrencies}
          withdrawTarget={modalType === "withdraw" ? withdrawTarget : undefined}
          onChange={setModalAmount}
          onCurrencyChange={setModalCurrency}
          onWithdrawTargetChange={setWithdrawTarget}
          onConfirm={confirmModal}
          onCancel={closeModal}
        />
      )}
      {swapModalOpen && activeSwapPair && (
        <SwapModal
          fromCurrency={swapFromCurrency}
          toCurrency={swapToCurrency}
          fromAmount={swapFromAmount}
          toAmount={swapToAmount}
          pairs={swapPairs}
          coefficient={activeSwapPair.coeff}
          onFromCurrencyChange={handleSwapPairChange}
          onAmountChange={(val) => updateSwapPreview(val)}
          onConfirm={confirmSwap}
          onCancel={() => setSwapModalOpen(false)}
        />
      )}
      {derivativesEnabled && derivativesModalOpen && (
        <DerivativesModal
          symbols={derivativeSymbols}
          selectedSymbol={derivativeSymbol}
          onSymbolChange={setDerivativeSymbol}
          quote={derivativeQuote}
          positions={derivativesPositions}
          amount={derivativeAmount}
          leverage={derivativeLeverage}
          orderType={derivativeOrderType}
          limitPrice={derivativeLimitPrice}
          loading={derivativesLoading}
          actionLoading={derivativeActionLoading}
          cancellingPositionIds={cancellingDerivativeIds}
          onAmountChange={setDerivativeAmount}
          onLeverageChange={setDerivativeLeverage}
          onOrderTypeChange={setDerivativeOrderType}
          onLimitPriceChange={setDerivativeLimitPrice}
          onOpenLong={() => openDerivative("long")}
          onOpenShort={() => openDerivative("short")}
          onClosePosition={closeDerivative}
          onCancelPosition={cancelDerivative}
          onRefresh={refreshDerivatives}
          priceHistory={priceHistory}
          orderBookVolume={orderBookVolume}
          orderBookVolumeLoading={orderBookVolumeLoading}
          openingDerivativeDraft={openingDerivativeDraft}
          onCancel={() => {
            setDerivativesModalOpen(false);
            setLimitOrderConfirm(null);
          }}
        />
      )}
      {limitOrderConfirm && (
        <LimitOrderConfirmModal
          side={limitOrderConfirm.side}
          symbol={limitOrderConfirm.symbol}
          currentPrice={limitOrderConfirm.currentPrice}
          limitPrice={limitOrderConfirm.limitPrice}
          diffPercent={limitOrderConfirm.diffPercent}
          actionLoading={derivativeActionLoading}
          onCancel={() => setLimitOrderConfirm(null)}
          onConfirm={() => {
            const req = limitOrderConfirm;
            if (!req) {
              return;
            }
            executeOpenDerivative(req.symbol, req.side, req.leverage, req.amount, req.orderType, req.limitPrice);
          }}
        />
      )}
      {transferStatus && (
        <div className="fixed inset-0 bg-black bg-opacity-40 flex items-center justify-center z-50">
          <div className="bg-white p-6 rounded-2xl shadow-xl w-80 space-y-4">
            <div className="flex items-center justify-center gap-3">
              {transferStatus === "loading" ? (
                <>
                  <RefreshCw className="animate-spin" size={24} />
                  <span className="text-lg">Connecting to recipient...</span>
                </>
              ) : (
                <>
                  <Check className="text-green-500" size={24} />
                  <span className="text-lg">Transfer on the way</span>
                  <div className="flex justify-between gap-4">
                    <Button onClick={() => { setTransferStatus(null) }} className="bg-[#0098ea] text-white w-full">OK</Button>
                  </div>
                </>
              )}
            </div>
          </div>
        </div>
      )}
    </div>
  );
};


const ModalAmount: React.FC<{
  title: string;
  value: string;
  currency: string;
  currencies: string[];
  withdrawTarget?: string;
  onChange: (value: string) => void;
  onCurrencyChange: (value: string) => void;
  onWithdrawTargetChange?: (value: string) => void;
  onConfirm: () => void;
  onCancel: () => void;
}> = ({ title, value, currency, currencies, withdrawTarget, onChange, onCurrencyChange, onWithdrawTargetChange, onConfirm, onCancel }) => (
  <div className="fixed inset-0 bg-black bg-opacity-40 flex items-center justify-center z-50">
    <div className="bg-white p-6 rounded-2xl shadow-xl w-80 space-y-4">
      <h2 className="text-lg font-semibold capitalize text-center">{title}</h2>
      <div className="flex items-center gap-3">
        <span className="text-sm text-gray-600">Currency</span>
        <select
          className="border border-gray-200 rounded-lg px-3 py-2 text-sm bg-white"
          value={currency}
          onChange={(e) => onCurrencyChange(e.target.value)}
        >
          {currencies.map((c) => (
            <option key={`modal-${c}`} value={c}>{c}</option>
          ))}
        </select>
      </div>
      <Input
        type="number"
        step="0.000000001"
        placeholder={`Enter amount in ${currency}`}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
      {withdrawTarget !== undefined && (
        <Input
          placeholder="Target address"
          value={withdrawTarget}
          onChange={(e) => onWithdrawTargetChange && onWithdrawTargetChange(e.target.value)}
        />
      )}
      <div className="flex justify-between gap-4">
        <Button onClick={onConfirm} className="bg-[#0098ea] text-white w-full">Confirm</Button>
        <Button onClick={onCancel} className="bg-gray-200 text-gray-700 w-full">Cancel</Button>
      </div>
    </div>
  </div>
);

const LimitOrderConfirmModal: React.FC<{
  side: "long" | "short";
  symbol: string;
  currentPrice: string;
  limitPrice: string;
  diffPercent: number;
  actionLoading: "long" | "short" | "close" | "cancel" | null;
  onConfirm: () => void;
  onCancel: () => void;
}> = ({ side, symbol, currentPrice, limitPrice, diffPercent, actionLoading, onConfirm, onCancel }) => {
  const current = Number.parseFloat(currentPrice);
  const desired = Number.parseFloat(limitPrice);
  const isHigher = Number.isFinite(current) && Number.isFinite(desired) && desired > current;

  return (
    <div className="fixed inset-0 bg-black/45 flex items-center justify-center z-[70]">
      <div className="bg-white rounded-2xl shadow-2xl w-[min(92vw,460px)] p-6 space-y-4 border border-[#e1ebf4]">
        <h3 className="text-lg font-semibold text-gray-800">Confirm limit order</h3>
        <p className="text-sm text-gray-600">
          Your {side === "long" ? "LONG (buy)" : "SHORT (sell)"} limit price for {symbol} differs from current market price by{" "}
          <span className="font-semibold text-gray-800">{diffPercent.toFixed(2)}%</span>.
        </p>
        <div className="bg-[#f7fbff] border border-[#e0edf8] rounded-xl p-3 space-y-2 text-sm">
          <div className="flex items-center justify-between">
            <span className="text-gray-500">Current price</span>
            <span className="font-semibold text-gray-800">${formatPriceNumber(currentPrice)}</span>
          </div>
          <div className="flex items-center justify-between">
            <span className="text-gray-500">Limit price</span>
            <span className="font-semibold text-gray-800">${formatPriceNumber(limitPrice)}</span>
          </div>
          <div className="flex items-center justify-between">
            <span className="text-gray-500">Difference</span>
            <span className={`font-semibold ${isHigher ? "text-red-600" : "text-amber-600"}`}>
              {isHigher ? "Above market" : "Below market"} ({formatCompactNumber(diffPercent.toFixed(2), 2)}%)
            </span>
          </div>
        </div>
        <p className="text-xs text-gray-500">
          Check order side and limit price before confirmation.
        </p>
        <div className="flex gap-3">
          <Button
            onClick={onConfirm}
            disabled={actionLoading !== null}
            className="bg-[#0098ea] text-white flex-1 disabled:opacity-50"
          >
            {actionLoading === side ? <RefreshCw className="animate-spin" size={14} /> : "Confirm and place"}
          </Button>
          <Button
            onClick={onCancel}
            disabled={actionLoading !== null}
            className="bg-gray-200 text-gray-700 flex-1 disabled:opacity-50"
          >
            Cancel
          </Button>
        </div>
      </div>
    </div>
  );
};

const OrderBookPanel: React.FC<{
  orderBook: DerivativesOrderBookVolume | null;
  loading: boolean;
  selectedSymbol: string;
}> = ({ orderBook, loading, selectedSymbol }) => {
  const groupingOptions = useMemo(() => resolveGroupingOptions(selectedSymbol), [selectedSymbol]);
  const [groupingStep, setGroupingStep] = useState<number>(() => {
    const defaults = resolveGroupingOptions(selectedSymbol);
    return defaults.find((v) => v > 0) ?? 0;
  });

  useEffect(() => {
    const defaults = resolveGroupingOptions(selectedSymbol);
    const preferred = defaults.find((v) => v > 0) ?? 0;
    setGroupingStep(preferred);
  }, [selectedSymbol]);

  const asksRaw = orderBook?.asks ?? [];
  const bidsRaw = orderBook?.bids ?? [];
  const asksGrouped = groupOrderBookLevels(asksRaw, groupingStep, "ask").slice(0, 120);
  const bidsGrouped = groupOrderBookLevels(bidsRaw, groupingStep, "bid").slice(0, 120);
  const asks = [...asksGrouped].reverse();
  const bids = [...bidsGrouped];
  const allLevels = [...asks, ...bids];
  const maxQty = Math.max(
    1,
    ...allLevels
      .map((lvl) => Number.parseFloat(lvl.quantity))
      .filter((qty) => Number.isFinite(qty) && qty > 0)
  );

  const renderRow = (lvl: DerivativesOrderBookLevel, side: "ask" | "bid") => {
    const qty = Number.parseFloat(lvl.quantity);
    const ratio = Number.isFinite(qty) && qty > 0 ? qty / maxQty : 0;
    const width = Math.max(4, Math.min(100, ratio * 100));
    const barClass = side === "ask" ? "bg-red-100" : "bg-green-100";
    const priceClass = side === "ask" ? "text-red-600" : "text-green-600";

    return (
      <div key={`${side}-${lvl.price}-${lvl.quantity}`} className="relative h-6 rounded overflow-hidden">
        <div className={`absolute inset-y-0 right-0 ${barClass}`} style={{ width: `${width}%` }} />
        <div className="relative z-10 h-full px-2 flex items-center justify-between text-[11px]">
          <span className={`font-semibold ${priceClass}`}>{formatPriceNumber(lvl.price)}</span>
          <span className="text-gray-500">{formatCompactNumber(lvl.quantity, 4)}</span>
        </div>
      </div>
    );
  };

  if (loading || !orderBook) {
    return (
      <div className="bg-gradient-to-b from-[#f5fbff] to-white rounded-xl border border-[#dce8f3] h-[360px] flex items-center justify-center text-sm text-gray-500">
        <div className="text-center">
          <RefreshCw className="animate-spin mx-auto mb-2 text-[#0098ea] opacity-50" size={18} />
          Loading order book…
        </div>
      </div>
    );
  }

  return (
    <div className="bg-gradient-to-b from-[#f5fbff] to-white rounded-xl border border-[#dce8f3] p-3 h-[360px] flex flex-col">
      <div className="flex items-center justify-between mb-2">
        <span className="text-[11px] uppercase tracking-wide text-gray-500 font-medium">Order Book</span>
        <span className="text-[11px] text-gray-400">{new Date(orderBook.at * 1000).toLocaleTimeString()}</span>
      </div>

      <div className="flex items-center gap-1 overflow-x-auto pb-2 mb-2 border-b border-[#e6eef7]">
        <span className="text-[10px] text-gray-400 mr-1 whitespace-nowrap">Grouping</span>
        {groupingOptions.map((step) => (
          <button
            key={`group-step-${step}`}
            onClick={() => setGroupingStep(step)}
            className={`px-2 py-1 rounded-md text-[11px] font-medium whitespace-nowrap transition-colors ${
              groupingStep === step
                ? "bg-[#0098ea] text-white"
                : "bg-white text-gray-600 border border-[#dce8f3] hover:bg-[#edf6fd]"
            }`}
          >
            {formatGroupingLabel(step)}
          </button>
        ))}
      </div>

      <div className="grid grid-cols-[1fr_auto] text-[10px] text-gray-400 px-2 mb-1">
        <span>Price</span>
        <span>Amount</span>
      </div>

      <div className="flex-1 overflow-y-auto pr-1 space-y-1.5">
        {asks.length === 0 && bids.length === 0 ? (
          <div className="h-full flex items-center justify-center text-xs text-gray-400">Order book is empty</div>
        ) : (
          <>
            {asks.map((lvl) => renderRow(lvl, "ask"))}
            <div className="h-px bg-[#e8f0f8] my-1" />
            {bids.map((lvl) => renderRow(lvl, "bid"))}
          </>
        )}
      </div>
    </div>
  );
};

const PriceChart: React.FC<{ data: PriceHistoryPoint[]; volumes: DerivativesVolumePoint[]; volumesLoaded: boolean }> = ({ data, volumes, volumesLoaded }) => {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const showVolumes = volumesLoaded;

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas || data.length < 2) return;

    const ctx = canvas.getContext('2d');
    if (!ctx) return;

    const dpr = window.devicePixelRatio || 1;
    const rect = canvas.getBoundingClientRect();
    canvas.width = rect.width * dpr;
    canvas.height = rect.height * dpr;
    ctx.scale(dpr, dpr);

    const w = rect.width;
    const h = rect.height;
    const pad = { top: 16, right: 80, bottom: 32, left: 12 };
    const volumeHeight = showVolumes ? 72 : 0;
    const volumeGap = showVolumes ? 12 : 0;
    const cw = w - pad.left - pad.right;
    const ch = h - pad.top - pad.bottom - volumeHeight - volumeGap;
    const volumeTop = pad.top + ch + volumeGap;

    const prices = data.map(d => parseFloat(d.price));
    const times = data.map(d => d.at);
    const minP = Math.min(...prices);
    const maxP = Math.max(...prices);
    const range = maxP - minP || 1;
    const minT = Math.min(...times);
    const maxT = Math.max(...times);
    const rangeT = maxT - minT || 1;

    // Background
    ctx.clearRect(0, 0, w, h);
    const bgGrad = ctx.createLinearGradient(0, 0, 0, h);
    bgGrad.addColorStop(0, '#f0f8ff');
    bgGrad.addColorStop(1, '#ffffff');
    ctx.fillStyle = bgGrad;
    ctx.beginPath();
    const r = 12;
    ctx.moveTo(r, 0); ctx.lineTo(w - r, 0); ctx.quadraticCurveTo(w, 0, w, r);
    ctx.lineTo(w, h - r); ctx.quadraticCurveTo(w, h, w - r, h);
    ctx.lineTo(r, h); ctx.quadraticCurveTo(0, h, 0, h - r);
    ctx.lineTo(0, r); ctx.quadraticCurveTo(0, 0, r, 0);
    ctx.closePath();
    ctx.fill();

    // Grid
    ctx.strokeStyle = '#d0e5f5';
    ctx.lineWidth = 0.5;
    ctx.setLineDash([4, 4]);
    for (let i = 0; i <= 4; i++) {
      const y = pad.top + (ch * i) / 4;
      ctx.beginPath();
      ctx.moveTo(pad.left, y);
      ctx.lineTo(w - pad.right, y);
      ctx.stroke();
    }
    ctx.setLineDash([]);

    // Price labels
    ctx.fillStyle = '#6b8299';
    ctx.font = '500 10px -apple-system, BlinkMacSystemFont, sans-serif';
    ctx.textAlign = 'left';
    for (let i = 0; i <= 4; i++) {
      const y = pad.top + (ch * i) / 4;
      const val = maxP - (range * i) / 4;
      ctx.fillText('$' + val.toFixed(2), w - pad.right + 6, y + 4);
    }

    // Time labels
    ctx.fillStyle = '#8fa4b8';
    ctx.textAlign = 'center';
    const lc = Math.min(5, data.length);
    for (let i = 0; i < lc; i++) {
      const idx = Math.floor((i * (data.length - 1)) / (lc - 1));
      const x = pad.left + (cw * (times[idx] - minT)) / rangeT;
      const d = new Date(times[idx] * 1000);
      ctx.fillText(d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }), x, h - 6);
    }

    const toX = (t: number) => {
      const clamped = Math.min(maxT, Math.max(minT, t));
      return pad.left + (cw * (clamped - minT)) / rangeT;
    };
    const toY = (p: number) => pad.top + ch - (ch * (p - minP)) / range;

    // Gradient fill under the line
    const grad = ctx.createLinearGradient(0, pad.top, 0, pad.top + ch);
    grad.addColorStop(0, 'rgba(0,152,234,0.20)');
    grad.addColorStop(0.6, 'rgba(0,152,234,0.06)');
    grad.addColorStop(1, 'rgba(0,152,234,0)');
    ctx.beginPath();
    ctx.moveTo(toX(times[0]), pad.top + ch);
    for (let i = 0; i < data.length; i++) ctx.lineTo(toX(times[i]), toY(prices[i]));
    ctx.lineTo(toX(times[times.length - 1]), pad.top + ch);
    ctx.closePath();
    ctx.fillStyle = grad;
    ctx.fill();

    // Line shadow
    ctx.beginPath();
    ctx.moveTo(toX(times[0]), toY(prices[0]));
    for (let i = 1; i < data.length; i++) ctx.lineTo(toX(times[i]), toY(prices[i]));
    ctx.strokeStyle = 'rgba(0,152,234,0.2)';
    ctx.lineWidth = 4;
    ctx.stroke();

    // Main line
    ctx.beginPath();
    ctx.moveTo(toX(times[0]), toY(prices[0]));
    for (let i = 1; i < data.length; i++) ctx.lineTo(toX(times[i]), toY(prices[i]));
    ctx.strokeStyle = '#0098ea';
    ctx.lineWidth = 2;
    ctx.lineJoin = 'round';
    ctx.lineCap = 'round';
    ctx.stroke();

    // End dot glow
    const lastX = toX(times[times.length - 1]);
    const lastY = toY(prices[prices.length - 1]);
    ctx.beginPath();
    ctx.arc(lastX, lastY, 6, 0, Math.PI * 2);
    ctx.fillStyle = 'rgba(0,152,234,0.15)';
    ctx.fill();

    // End dot
    ctx.beginPath();
    ctx.arc(lastX, lastY, 3.5, 0, Math.PI * 2);
    ctx.fillStyle = '#0098ea';
    ctx.fill();
    ctx.beginPath();
    ctx.arc(lastX, lastY, 2, 0, Math.PI * 2);
    ctx.fillStyle = '#ffffff';
    ctx.fill();

    if (showVolumes) {
      const volumeBySecond = new Map<number, number>();
      for (const point of volumes) {
        const parsed = Number.parseFloat(point.volume);
        if (!Number.isFinite(parsed) || parsed < 0) {
          continue;
        }
        volumeBySecond.set(point.at, parsed);
      }

      const alignedPoints = times.map((at) => ({
        at,
        volume: volumeBySecond.get(at) ?? 0,
      }));
      const maxVolume = Math.max(1, ...alignedPoints.map((v) => v.volume));
      const barWidth = Math.max(1, Math.floor(cw / Math.max(alignedPoints.length, 60)));

      ctx.fillStyle = '#8fa4b8';
      ctx.textAlign = 'left';
      ctx.fillText('Volume', pad.left, volumeTop - 2);

      for (let i = 0; i < alignedPoints.length; i++) {
        const p = alignedPoints[i];
        const barHeight = (p.volume / maxVolume) * (volumeHeight - 10);
        const x = toX(p.at) - barWidth / 2;
        const y = volumeTop + volumeHeight - barHeight;
        ctx.fillStyle = i === alignedPoints.length - 1 ? 'rgba(0,152,234,0.55)' : 'rgba(183,222,248,0.95)';
        ctx.fillRect(x, y, barWidth, barHeight);
      }
    }
  }, [data, showVolumes, volumes]);

  if (data.length < 2) {
    return (
      <div className="bg-gradient-to-b from-[#f0f8ff] to-white rounded-xl p-6 text-center text-sm text-gray-400 flex items-center justify-center" style={{ height: 280 }}>
        <div>
          <RefreshCw className="animate-spin mx-auto mb-2 text-[#0098ea] opacity-40" size={20} />
          Waiting for price data…
        </div>
      </div>
    );
  }

  return <canvas ref={canvasRef} style={{ width: '100%', height: showVolumes ? 330 : 280, display: 'block' }} className="rounded-xl" />;
};

/* Leverage slider tick marks */
const LEVERAGE_TICKS = [1, 5, 10, 15, 20];

const DerivativesModal: React.FC<{
  symbols: string[];
  selectedSymbol: string;
  onSymbolChange: (value: string) => void;
  quote: DerivativesQuote | null;
  positions: DerivativesPosition[];
  amount: string;
  leverage: string;
  orderType: "market" | "limit";
  limitPrice: string;
  loading: boolean;
  actionLoading: "long" | "short" | "close" | "cancel" | null;
  cancellingPositionIds: string[];
  priceHistory: PriceHistoryPoint[];
  orderBookVolume: DerivativesOrderBookVolume | null;
  orderBookVolumeLoading: boolean;
  openingDerivativeDraft: OpeningDerivativeDraft | null;
  onAmountChange: (value: string) => void;
  onLeverageChange: (value: string) => void;
  onOrderTypeChange: (value: "market" | "limit") => void;
  onLimitPriceChange: (value: string) => void;
  onOpenLong: () => void;
  onOpenShort: () => void;
  onClosePosition: (positionId: string) => void;
  onCancelPosition: (positionId: string) => void;
  onRefresh: () => void;
  onCancel: () => void;
}> = ({
  symbols,
  selectedSymbol,
  onSymbolChange,
  quote,
  positions,
  amount,
  leverage,
  orderType,
  limitPrice,
  loading,
  actionLoading,
  cancellingPositionIds,
  priceHistory,
  orderBookVolume,
  orderBookVolumeLoading,
  openingDerivativeDraft,
  onAmountChange,
  onLeverageChange,
  onOrderTypeChange,
  onLimitPriceChange,
  onOpenLong,
  onOpenShort,
  onClosePosition,
  onCancelPosition,
  onRefresh,
  onCancel,
}) => {
  const leverageNum = parseInt(leverage, 10) || 1;
    const leverageFillPercent = Math.max(0, Math.min(100, ((leverageNum - 1) / 19) * 100));
    const pendingPositions = positions.filter((p) => !p.opened);
    const openPositions = positions.filter((p) => p.opened);
    const positionNetRoiPercent = (p: DerivativesPosition) => {
      const collateral = parseFloat(p.collateral);
      if (collateral <= 0) {
        return p.pnl_percent;
      }
      return p.pnl_percent - (parseFloat(p.fee) / collateral) * 100;
    };

    return (
      <div className="fixed inset-0 bg-black/40 backdrop-blur-sm flex items-center justify-center z-50" onClick={onCancel}>
        {/* Range slider custom styles */}
        <style>{`
        .deriv-slider { -webkit-appearance: none; appearance: none; width: 100%; height: 6px; border-radius: 3px; outline: none; cursor: pointer; background: transparent; }
        .deriv-slider::-webkit-slider-runnable-track { height: 6px; border-radius: 3px; background: transparent; }
        .deriv-slider::-webkit-slider-thumb { -webkit-appearance: none; width: 22px; height: 22px; border-radius: 50%; background: #0098ea; border: 3px solid #fff; box-shadow: 0 1px 6px rgba(0,152,234,0.4); margin-top: -8px; cursor: pointer; transition: box-shadow 0.15s; }
        .deriv-slider::-webkit-slider-thumb:hover { box-shadow: 0 2px 10px rgba(0,152,234,0.6); }
        .deriv-slider::-moz-range-track { height: 6px; border-radius: 3px; background: transparent; border: none; }
        .deriv-slider::-moz-range-thumb { width: 22px; height: 22px; border-radius: 50%; background: #0098ea; border: 3px solid #fff; box-shadow: 0 1px 6px rgba(0,152,234,0.4); cursor: pointer; }
      `}</style>
        <div
          className="bg-white rounded-2xl shadow-2xl w-[min(96vw,1280px)] max-h-[92vh] overflow-y-auto overscroll-contain border border-[#e0ecf5]"
          onClick={(e) => e.stopPropagation()}
        >
          {/* Header */}
          <div className="sticky top-0 bg-white/95 backdrop-blur-sm z-10 px-6 pt-5 pb-3 border-b border-[#eef4fa] rounded-t-2xl">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-3">
                <div className="w-9 h-9 rounded-xl bg-gradient-to-br from-[#0098ea] to-[#0077bb] flex items-center justify-center">
                  <Activity size={18} className="text-white" />
                </div>
                <div>
                  <h2 className="text-lg font-bold text-gray-800">Derivatives</h2>
                  <div className="flex items-center gap-1.5 text-xs text-gray-500">
                    {loading && <RefreshCw size={10} className="animate-spin" />}
                    {quote ? (
                      <>
                        <span className="inline-block w-1.5 h-1.5 rounded-full bg-green-400 animate-pulse" />
                        <span>${quote.price}</span>
                        <span className="text-gray-400">·</span>
                        <span>{new Date(quote.at * 1000).toLocaleTimeString()}</span>
                      </>
                    ) : (
                      <span>Connecting…</span>
                    )}
                  </div>
                </div>
              </div>
              <button
                onClick={onCancel}
                className="w-8 h-8 rounded-lg flex items-center justify-center text-gray-400 hover:text-gray-600 hover:bg-gray-100 transition-colors"
              >
                <X size={20} />
              </button>
            </div>
          </div>

          <div className="px-6 pb-6 pt-4 space-y-5">
              {/* Symbol selector */}
            <div className="flex items-center gap-2">
              {symbols.map((s) => (
                <button
                  key={`sym-${s}`}
                  onClick={() => onSymbolChange(s)}
                  className={`px-4 py-1.5 rounded-full text-sm font-medium transition-all ${selectedSymbol === s
                      ? 'bg-[#0098ea] text-white shadow-sm shadow-[#0098ea]/20'
                      : 'bg-[#f0f8ff] text-gray-600 hover:bg-[#e0ecf5]'
                    }`}
                >
                  {s}
                </button>
              ))}
            </div>

            {/* Chart + Order book */}
            <div className="grid grid-cols-1 xl:grid-cols-[320px_minmax(0,1fr)] gap-4">
              <OrderBookPanel
                orderBook={orderBookVolume}
                loading={orderBookVolumeLoading}
                selectedSymbol={selectedSymbol}
              />
              <PriceChart
                data={priceHistory}
                volumes={orderBookVolume?.volume_history ?? []}
                volumesLoaded={!orderBookVolumeLoading}
              />
            </div>

            {/* Order type pills */}
            <div className="flex items-center gap-2">
              <span className="text-xs font-medium text-gray-500 uppercase tracking-wide">Order</span>
              <div className="flex bg-[#f0f8ff] rounded-lg p-0.5">
                <button
                  onClick={() => onOrderTypeChange('market')}
                  className={`px-4 py-1.5 rounded-md text-sm font-medium transition-all ${orderType === 'market'
                      ? 'bg-white text-[#0098ea] shadow-sm'
                      : 'text-gray-500 hover:text-gray-700'
                    }`}
                >
                  Market
                </button>
                <button
                  onClick={() => onOrderTypeChange('limit')}
                  className={`px-4 py-1.5 rounded-md text-sm font-medium transition-all ${orderType === 'limit'
                      ? 'bg-white text-[#0098ea] shadow-sm'
                      : 'text-gray-500 hover:text-gray-700'
                    }`}
                >
                  Limit
                </button>
              </div>
            </div>

            {/* Leverage slider */}
            <div>
              <div className="flex items-center justify-between mb-2">
                <span className="text-xs font-medium text-gray-500 uppercase tracking-wide">Leverage</span>
                <span className="text-sm font-bold text-[#0098ea] bg-[#f0f8ff] px-3 py-0.5 rounded-full">
                  {leverageNum}x
                </span>
              </div>
              <input
                type="range"
                min={1}
                max={20}
                step={1}
                value={leverageNum}
                onChange={(e) => onLeverageChange(e.target.value)}
                style={{
                  background: `linear-gradient(90deg, #0098ea 0%, #0098ea ${leverageFillPercent}%, #d9edf9 ${leverageFillPercent}%, #d9edf9 100%)`,
                }}
                className="deriv-slider"
              />
              <div className="flex justify-between mt-1.5 px-0.5">
                {LEVERAGE_TICKS.map((t) => (
                  <button
                    key={`tick-${t}`}
                    onClick={() => onLeverageChange(String(t))}
                    className={`text-[10px] font-medium transition-colors ${leverageNum === t ? 'text-[#0098ea]' : 'text-gray-400 hover:text-gray-600'
                      }`}
                  >
                    {t}x
                  </button>
                ))}
              </div>
            </div>

            {/* Collateral input */}
            <div className="space-y-3">
              <div className="relative">
                <input
                  type="number"
                  step="0.000000001"
                  placeholder="0.00"
                  value={amount}
                  onChange={(e) => onAmountChange(e.target.value)}
                  className="w-full pl-4 pr-16 py-3 bg-[#f8fbff] border border-[#dce8f3] rounded-xl text-base font-medium text-gray-800 placeholder-gray-300 focus:outline-none focus:ring-2 focus:ring-[#0098ea]/20 focus:border-[#0098ea] transition-all"
                />
                <span className="absolute right-4 top-1/2 -translate-y-1/2 text-sm font-semibold text-[#0098ea]">TON</span>
              </div>

              {orderType === 'limit' && (
                <div className="relative">
                  <input
                    type="number"
                    step="0.000000001"
                    placeholder="Limit price"
                    value={limitPrice}
                    onChange={(e) => onLimitPriceChange(e.target.value)}
                    className="w-full pl-4 pr-12 py-3 bg-[#f8fbff] border border-[#dce8f3] rounded-xl text-base font-medium text-gray-800 placeholder-gray-300 focus:outline-none focus:ring-2 focus:ring-[#0098ea]/20 focus:border-[#0098ea] transition-all"
                  />
                  <span className="absolute right-4 top-1/2 -translate-y-1/2 text-sm font-medium text-gray-400">$</span>
                </div>
              )}
            </div>

            {/* Long / Short */}
            <div className="grid grid-cols-2 gap-3">
              <button
                onClick={onOpenLong}
                disabled={actionLoading !== null}
                className="flex items-center justify-center gap-2 py-3 rounded-xl font-semibold text-white transition-all disabled:opacity-40 disabled:cursor-not-allowed"
                style={{ background: 'linear-gradient(135deg, #16a34a, #22c55e)' }}
              >
                {actionLoading === 'long' ? (
                  <RefreshCw className="animate-spin" size={18} />
                ) : (
                  <>
                    <TrendingUp size={18} />
                    Long
                  </>
                )}
              </button>
              <button
                onClick={onOpenShort}
                disabled={actionLoading !== null}
                className="flex items-center justify-center gap-2 py-3 rounded-xl font-semibold text-white transition-all disabled:opacity-40 disabled:cursor-not-allowed"
                style={{ background: 'linear-gradient(135deg, #dc2626, #ef4444)' }}
              >
                {actionLoading === 'short' ? (
                  <RefreshCw className="animate-spin" size={18} />
                ) : (
                  <>
                    <TrendingDown size={18} />
                    Short
                  </>
                )}
              </button>
            </div>

            {/* Pending Orders */}
            <div>
              <div className="flex items-center justify-between mb-3">
                <h3 className="text-sm font-semibold text-amber-800">Pending Orders</h3>
                {pendingPositions.length > 0 && (
                  <span className="text-xs font-medium text-amber-700 bg-amber-100 px-2 py-0.5 rounded-full">
                    {pendingPositions.length}
                  </span>
                )}
              </div>
              {pendingPositions.length === 0 ? (
                openingDerivativeDraft ? (
                  <div className="bg-[#f4f9ff] border border-[#d3e7f8] rounded-xl p-4">
                    <div className="flex items-center justify-between">
                      <div className="flex items-center gap-2">
                        <RefreshCw className="animate-spin text-[#0098ea]" size={14} />
                        <span className="text-sm font-semibold text-gray-700">Opening {openingDerivativeDraft.symbol}</span>
                        <span className={`inline-flex items-center gap-1 px-2 py-0.5 rounded-md text-[10px] font-bold text-white ${openingDerivativeDraft.isLong ? 'bg-green-500' : 'bg-red-500'}`}>
                          {openingDerivativeDraft.isLong ? <TrendingUp size={10} /> : <TrendingDown size={10} />}
                          {openingDerivativeDraft.isLong ? 'LONG' : 'SHORT'}
                        </span>
                      </div>
                      <span className="text-xs text-gray-400">Please wait…</span>
                    </div>
                    <div className="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs mt-3">
                      <div>
                        <div className="text-gray-400 mb-0.5">Collateral</div>
                        <div className="font-semibold text-gray-700">{openingDerivativeDraft.amount} TON</div>
                      </div>
                      <div>
                        <div className="text-gray-400 mb-0.5">Leverage</div>
                        <div className="font-semibold text-gray-700">×{openingDerivativeDraft.leverage}</div>
                      </div>
                      <div>
                        <div className="text-gray-400 mb-0.5">Status</div>
                        <div className="font-semibold text-[#0098ea]">Submitting</div>
                      </div>
                      <div>
                        <div className="text-gray-400 mb-0.5">Time</div>
                        <div className="font-semibold text-gray-700">{new Date(openingDerivativeDraft.createdAt).toLocaleTimeString()}</div>
                      </div>
                    </div>
                  </div>
                ) : (
                  <div className="text-center py-4 text-sm text-gray-400 bg-[#fafcff] rounded-xl border border-dashed border-[#dce8f3]">
                    No pending orders
                  </div>
                )
              ) : (
                <div className="space-y-2">
                  {openingDerivativeDraft && (
                    <div className="bg-[#f4f9ff] border border-[#d3e7f8] rounded-xl p-3 flex items-center gap-2 text-xs text-gray-600">
                      <RefreshCw className="animate-spin text-[#0098ea]" size={12} />
                      Opening new order {openingDerivativeDraft.symbol}…
                    </div>
                  )}
                  {pendingPositions.map((p) => {
                    const isCancelling = cancellingPositionIds.includes(p.id);
                    return (
                      <div key={`deriv-pending-${p.id}`} className={`bg-amber-50 border border-amber-200 rounded-xl p-4 ${isCancelling ? "opacity-80" : ""}`}>
                        <div className="flex items-center justify-between mb-3">
                          <div className="flex items-center gap-2">
                            <span className={`inline-flex items-center gap-1 px-2.5 py-0.5 rounded-md text-xs font-bold text-white ${p.is_long ? 'bg-green-500' : 'bg-red-500'
                              }`}>
                              {p.is_long ? <TrendingUp size={12} /> : <TrendingDown size={12} />}
                              {p.is_long ? 'LONG' : 'SHORT'}
                            </span>
                            <span className="inline-flex items-center px-2 py-0.5 rounded-md text-[10px] font-semibold text-amber-700 bg-amber-100">
                              Waiting open
                            </span>
                            <span className="text-sm font-semibold text-gray-700">{p.symbol}</span>
                            <span className="text-xs text-gray-400 font-medium">×{p.leverage}</span>
                          </div>
                          <button
                            onClick={() => onCancelPosition(p.id)}
                            disabled={actionLoading !== null || isCancelling}
                            className="px-3 py-1 rounded-lg text-xs font-medium bg-white text-amber-800 border border-amber-200 hover:bg-amber-100 transition-colors disabled:opacity-40"
                          >
                            {isCancelling ? <RefreshCw className="animate-spin" size={12} /> : 'Cancel'}
                          </button>
                        </div>
                        <div className="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs">
                          <div>
                            <div className="text-gray-400 mb-0.5">Collateral</div>
                            <div className="font-semibold text-gray-700">{p.collateral} TON</div>
                          </div>
                          <div>
                            <div className="text-gray-400 mb-0.5">Fee</div>
                            <div className="font-semibold text-gray-700">{p.fee} TON</div>
                          </div>
                          <div>
                            <div className="text-gray-400 mb-0.5">Limit Entry</div>
                            <div className="font-semibold text-gray-700">${p.entry_price}</div>
                          </div>
                          <div>
                            <div className="text-gray-400 mb-0.5">Current</div>
                            <div className="font-semibold text-gray-700">${p.current_price}</div>
                          </div>
                        </div>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>

            {/* Open Positions */}
            <div>
              <div className="flex items-center justify-between mb-3">
                <h3 className="text-sm font-semibold text-gray-700">Open Positions</h3>
                {openPositions.length > 0 && (
                  <span className="text-xs font-medium text-[#0098ea] bg-[#f0f8ff] px-2 py-0.5 rounded-full">
                    {openPositions.length}
                  </span>
                )}
              </div>
              {openPositions.length === 0 ? (
                <div className="text-center py-4 text-sm text-gray-400 bg-[#fafcff] rounded-xl border border-dashed border-[#dce8f3]">
                  No open positions
                </div>
              ) : (
                <div className="space-y-2">
                  {openPositions.map((p) => {
                    const netRoiPercent = positionNetRoiPercent(p);
                    return (
                    <div key={`deriv-open-${p.id}`} className="bg-[#f8fbff] border border-[#e0ecf5] rounded-xl p-4">
                      <div className="flex items-center justify-between mb-3">
                        <div className="flex items-center gap-2">
                          <span className={`inline-flex items-center gap-1 px-2.5 py-0.5 rounded-md text-xs font-bold text-white ${p.is_long ? 'bg-green-500' : 'bg-red-500'
                            }`}>
                            {p.is_long ? <TrendingUp size={12} /> : <TrendingDown size={12} />}
                            {p.is_long ? 'LONG' : 'SHORT'}
                          </span>
                          <span className="text-sm font-semibold text-gray-700">{p.symbol}</span>
                          <span className="text-xs text-gray-400 font-medium">×{p.leverage}</span>
                        </div>
                        <button
                          onClick={() => onClosePosition(p.id)}
                          disabled={actionLoading !== null}
                          className="px-3 py-1 rounded-lg text-xs font-medium bg-gray-100 text-gray-600 hover:bg-red-50 hover:text-red-600 transition-colors disabled:opacity-40"
                        >
                          {actionLoading === 'close' ? <RefreshCw className="animate-spin" size={12} /> : 'Close'}
                        </button>
                      </div>
                      <div className="grid grid-cols-2 sm:grid-cols-4 gap-2 text-xs">
                        <div>
                          <div className="text-gray-400 mb-0.5">Collateral</div>
                          <div className="font-semibold text-gray-700">{p.collateral} TON</div>
                        </div>
                        <div>
                          <div className="text-gray-400 mb-0.5">Fee</div>
                          <div className="font-semibold text-gray-700">{p.fee} TON</div>
                        </div>
                        <div>
                          <div className="text-gray-400 mb-0.5">Entry</div>
                          <div className="font-semibold text-gray-700">${p.entry_price}</div>
                        </div>
                        <div>
                          <div className="text-gray-400 mb-0.5">Current</div>
                          <div className="font-semibold text-gray-700">${p.current_price}</div>
                        </div>
                      </div>
                      <div className="flex items-center justify-between mt-3 pt-2 border-t border-[#e8f0f8]">
                        <div className="text-xs text-gray-400">
                          Liq. <span className="text-gray-600 font-medium">${p.liquidation_price}</span>
                        </div>
                        <div className={`text-sm font-bold ${netRoiPercent >= 0 ? 'text-green-600' : 'text-red-500'
                          }`}>
                          {netRoiPercent >= 0 ? '+' : ''}{netRoiPercent.toFixed(2)}%
                        </div>
                      </div>
                    </div>
                    );
                  })}
                </div>
              )}
            </div>
          </div>
        </div>
      </div>
    );
  };

const SwapModal: React.FC<{
  fromCurrency: string;
  toCurrency: string;
  fromAmount: string;
  toAmount: string;
  pairs: { from: string, to: string, coeff: number }[];
  coefficient: number;
  onFromCurrencyChange: (value: string) => void;
  onAmountChange: (value: string) => void;
  onConfirm: () => void;
  onCancel: () => void;
}> = ({ fromCurrency, toCurrency, fromAmount, toAmount, pairs, coefficient, onFromCurrencyChange, onAmountChange, onConfirm, onCancel }) => (
  <div className="fixed inset-0 bg-black bg-opacity-40 flex items-center justify-center z-50">
    <div className="bg-white p-6 rounded-2xl shadow-xl w-80 space-y-4">
      <h2 className="text-lg font-semibold capitalize text-center">Swap</h2>
      <div className="flex items-center gap-3">
        <span className="text-sm text-gray-600">From</span>
        <select
          className="border border-gray-200 rounded-lg px-3 py-2 text-sm bg-white"
          value={fromCurrency}
          onChange={(e) => onFromCurrencyChange(e.target.value)}
        >
          {pairs.map((p) => (
            <option key={`swap-from-${p.from}`} value={p.from}>{p.from}</option>
          ))}
        </select>
      </div>
      <div className="flex items-center gap-3">
        <span className="text-sm text-gray-600">To</span>
        <input className="border border-gray-200 rounded-lg px-3 py-2 text-sm bg-gray-100" value={toCurrency} disabled />
      </div>
      <Input
        type="number"
        step="0.000000001"
        placeholder={`Enter amount in ${fromCurrency}`}
        value={fromAmount}
        onChange={(e) => onAmountChange(e.target.value)}
      />
      <Input
        disabled
        value={toAmount}
        placeholder={`You will receive in ${toCurrency}`}
        className="bg-gray-100"
      />
      <div className="text-sm text-gray-600 text-center">Rate: 1 {fromCurrency} = {coefficient} {toCurrency}</div>
      <div className="flex justify-between gap-4">
        <Button onClick={onConfirm} className="bg-[#0098ea] text-white w-full">Confirm</Button>
        <Button onClick={onCancel} className="bg-gray-200 text-gray-700 w-full">Cancel</Button>
      </div>
    </div>
  </div>
);



export default App;
