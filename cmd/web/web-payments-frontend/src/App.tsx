import React, {useEffect, useMemo, useState} from 'react';
import './App.css';
import {TonConnectButton, useTonAddress, useTonConnectUI, useTonWallet} from "@tonconnect/ui-react";

import {Send, ArrowDown, ArrowUp, RefreshCw, Copy, PlusCircle, MinusCircle, Activity, Check, Plus, Repeat} from "lucide-react";
import {Card, CardContent} from "./components/ui/card";
import {Input} from "./components/ui/input";
import {Button} from "./components/ui/button";
import {PaymentChannelHistoryItem} from "./index";
import {CHAIN} from "@tonconnect/sdk";


function App() {
  const [tonConnectUI] = useTonConnectUI();
  const wallet = useTonWallet();
  let addr = useTonAddress();
  let [paymentAddr, setPaymentAddr] = useState("Loading...");
  let [balances, setBalances] = useState<Record<string, string>>({});
  let [capacities, setCapacities] = useState<Record<string, string>>({});
  let [lockedBalance, setLockedBalance] = useState<Record<string, string>>({});
  let [pendingIn, setPendingIn] = useState<Record<string, string>>({});
  let [history, setHistory] = useState<PaymentChannelHistoryItem[] | null>(null);
  let [supportedCurrencies, setSupportedCurrencies] = useState<string[]>(["TON"]);

  window.onPaymentNetworkLoaded = function(addr) {
    setPaymentAddr(addr);
    console.log("Payment network loaded: "+addr);
  }
  window.onPaymentChannelUpdated = function(ev) {
    console.log("Payment channel updated: "+JSON.stringify(ev));

    setBalances(ev.balances || {});
    setCapacities(ev.capacities || {});
    setLockedBalance(ev.locked || {});
    setPendingIn(ev.pendingIn || {});

    const currencies = Array.from(new Set([
      ...Object.keys(ev.balances || {}),
      ...Object.keys(ev.capacities || {}),
      ...Object.keys(ev.locked || {}),
      ...Object.keys(ev.pendingIn || {}),
    ]));

    if (currencies.length === 0) {
      setSupportedCurrencies(["TON"]);
      return;
    }

    setSupportedCurrencies(currencies);

    window.getChannelHistory(5).then(history => {
      setHistory(history);
    }).catch(e => {
      console.error(e);
    });
  }

  window.onPaymentChannelHistoryUpdated = function() {
    window.getChannelHistory(5).then(history => {
      setHistory(history);
    }).catch(e => {
      console.error(e);
    });
  }

  useEffect(() => {
    if (!wallet) return;

    const initWasm = async () => {
      window.walletAddress = () => {
        return addr;
      };

      window.doTransaction = async (reason, messages) => {
        console.log("requested tx: "+ reason);

        let list = [];
        for (let i = 0; i < messages.length; i++) {
          list.push({
            address:  messages[i].to,
            amount:  messages[i].amtNano,
            stateInit:  messages[i].stateInit,
            payload:  messages[i].body,
          })
        }

        const transaction = {
          validUntil: Math.floor(Date.now() / 1000) + 90,
          network: CHAIN.TESTNET,
          messages: list
        }

        let resp = await tonConnectUI.sendTransaction(transaction);
        return resp.boc;
      }

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
      } catch (e) {
        console.error(e);
      }
    };

    initWasm();
  }, [wallet]);

  if (!wallet) {
    return (
        <div className="min-h-screen bg-white text-gray-800 p-6 space-y-6">
          <div className="flex justify-between items-center">
            <h1 className="text-2xl font-bold text-[#0098ea]">TON Payments Wallet</h1>
            <TonConnectButton/>
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
};

const WalletUI: React.FC<WalletUIProps> = ({ paymentAddr, balances, locked, capacities, pendingIn, transactions, currencies }) => {
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

  const availableCurrencies = useMemo(() => currencies.length ? currencies : ["TON"], [currencies]);
  const swapPairs = useMemo(() => [{ from: "TON", to: "USDX", coeff: 2 }], []);
  const activeSwapPair = useMemo(() => swapPairs.find(p => p.from === swapFromCurrency && p.to === swapToCurrency) ?? swapPairs[0], [swapPairs, swapFromCurrency, swapToCurrency]);

  useEffect(() => {
    if (!availableCurrencies.includes(sendCurrency)) {
      setSendCurrency(availableCurrencies[0]);
    }
    if (!availableCurrencies.includes(modalCurrency)) {
      setModalCurrency(availableCurrencies[0]);
    }
  }, [availableCurrencies]);

  useEffect(() => {
    if (swapFromAmount && activeSwapPair) {
      updateSwapPreview(swapFromAmount, activeSwapPair.coeff);
    }
  }, [activeSwapPair]);

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
    if (modalType == "topup") {
      window.topupChannel(modalAmount, modalCurrency);
    } else if (modalType == "withdraw") {
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
    return Object.entries(map)
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

  return (
      <div className="min-h-screen bg-white text-gray-800 p-4 sm:p-6 flex justify-center">
        <div className="w-full max-w-xl space-y-6">
          <div className="flex justify-between items-center">
            <h1 className="text-2xl font-bold text-[#0098ea]">TON Payments Wallet</h1>
            <h2 className="text-lg font-bold text-[#772233]">Testnet</h2>
            <TonConnectButton />
          </div>

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
                                    className="bg-[#0098ea] text-white px-4 py-2 rounded-xl"
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
                <Button disabled={!sendAmount || !sendTo} className="bg-[#0098ea] text-white px-4 py-2 rounded-xl flex items-center gap-2 disabled:bg-gray-300" onClick={()=>{
                  setTransferStatus("loading");
                  setSendFeeAmount("");

                  window.sendTransfer(sendAmount, sendTo, sendCurrency).then(res => {
                    setTransferStatus("success");
                    console.log("transferred: "+sendAmount+" "+sendCurrency+" to "+sendTo);
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
                                  return <RefreshCw   className="text-blue-500"  {...p} />;
                                case 2: // Transfer-in
                                  return <ArrowDown   className="text-green-500" {...p} />;
                                case 3: // Transfer-out
                                  return <ArrowUp     className="text-red-500"   {...p} />;
                                case 4: // Uncooperative close
                                  return <Activity    className="text-orange-500" {...p} />;
                                case 5: // Closed
                                  return <Check       className="text-gray-500"  {...p} />;
                                case 6: // Their capacity rented
                                  return <PlusCircle  className="text-green-500" {...p} />;
                                case 7: // Our capacity rented
                                  return <Plus        className="text-green-500" {...p} />;
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
        {transferStatus && (
            <div className="fixed inset-0 bg-black bg-opacity-40 flex items-center justify-center z-50">
              <div className="bg-white p-6 rounded-2xl shadow-xl w-80 space-y-4">
                <div className="flex items-center justify-center gap-3">
                  {transferStatus === "loading" ? (
                      <>
                        <RefreshCw className="animate-spin" size={24}/>
                        <span className="text-lg">Connecting to recipient...</span>
                      </>
                  ) : (
                      <>
                        <Check className="text-green-500" size={24}/>
                        <span className="text-lg">Transfer on the way</span>
                        <div className="flex justify-between gap-4">
                          <Button onClick={() =>{setTransferStatus(null)}} className="bg-[#0098ea] text-white w-full">OK</Button>
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
