import React from 'react';
import ReactDOM from 'react-dom/client';
import './index.css';
import App from './App';
import reportWebVitals from './reportWebVitals';
import { THEME, TonConnectUIProvider } from "@tonconnect/ui-react";

interface PaymentChannelEvent {
    active: boolean;
    balances: Record<string, string>;
    capacities: Record<string, string>;
    locked: Record<string, string>;
    pendingIn: Record<string, string>;
    address: string;
    uncooperativeClose: boolean;
    expectedWalletApprovals: number;
}

export interface PaymentChannelHistoryItem {
    id: string;
    action: number;
    timestamp: string;
    amounts?: Record<string, string>;
    party?: string;
    isTheir?: boolean;
}

export interface DerivativesPosition {
    id: string;
    symbol: string;
    channel_address: string;
    collateral: string;
    fee: string;
    is_long: boolean;
    leverage: number;
    status: "open" | "pending_open";
    opened: boolean;
    opened_at?: number;
    entry_at: number;
    entry_price: string;
    current_price: string;
    pnl_percent: number;
    liquidation_price: string;
}

export interface DerivativesQuote {
    symbol: string;
    price: string;
    raw_price: string;
    at: number;
}

export interface PriceHistoryPoint {
    at: number;
    price: string;
}

export interface DerivativesOrderBookLevel {
    price: string;
    quantity: string;
}

export interface DerivativesVolumePoint {
    at: number;
    volume: string;
}

export interface DerivativesOrderBookVolume {
    symbol: string;
    at: number;
    volume: string;
    volume_history: DerivativesVolumePoint[];
    bids: DerivativesOrderBookLevel[];
    asks: DerivativesOrderBookLevel[];
}

export interface TxMessage {
    to: string;
    amtNano: string;
    body?: string;
    stateInit?: string;
}

export interface PaymentWalletRequestEvent {
    phase: "queued" | "requested" | "submitted" | "failed";
    reason: string;
    messages: number;
    details?: string;
    at: number;
}

declare global {
    interface Window {
        startPaymentNetwork: (peerPubKey: string, channelPubKey: string) => void;
        walletAddress: () => string;
        onPaymentNetworkLoaded: (addr: string) => void;
        onPaymentChannelUpdated: (ev: PaymentChannelEvent) => void;
        onPaymentChannelHistoryUpdated: () => void;
        onPaymentWalletRequestUpdated: (ev: PaymentWalletRequestEvent) => void;
        topupChannel: (amount: string, currency: string) => void;
        sendTransfer: (amount: string, to: string, currency?: string) => Promise<string>;
        estimateTransfer: (amount: string, to: string, currency?: string) => string;
        executeSwap: (fromCurrency: string, toCurrency: string, amount: string, coeff: number) => Promise<void>;
        getDerivativesPositions: (symbol?: string) => Promise<DerivativesPosition[]>;
        getDerivativeMarketPrice: (symbol: string) => Promise<DerivativesQuote>;
        getDerivativePriceHistory: (symbol: string) => Promise<PriceHistoryPoint[]>;
        openDerivativePosition: (symbol: string, side: "long" | "short", leverage: number, amount: string, type?: "market" | "limit", price?: string) => Promise<string>;
        closeDerivativePosition: (positionIdOrSymbol: string, type?: "market" | "cancel") => Promise<void>;
        isDerivativesEnabled: () => boolean;
        getChannelHistory: (limit: number) => Promise<PaymentChannelHistoryItem[] | null>;
        openChannel: () => void;
        closeChannelUncooperative: (channelAddress: string) => Promise<void>;
        withdrawChannel: (amount: string, currency: string, target: string) => void;
        doTransaction: (reason: string, messages: TxMessage[]) => Promise<string>;
    }
}

const root = ReactDOM.createRoot(
    document.getElementById('root') as HTMLElement
);

root.render(
    <React.StrictMode>
        <TonConnectUIProvider uiPreferences={{ theme: THEME.LIGHT }} manifestUrl={window.location.origin + "/tonconnect-manifest.json"}>
            <App />
        </TonConnectUIProvider>
    </React.StrictMode>
);

// If you want to start measuring performance in your app, pass a function
// to log results (for example: reportWebVitals(console.log))
// or send to an analytics endpoint. Learn more: https://bit.ly/CRA-vitals
reportWebVitals();
