import React from 'react';
import ReactDOM from 'react-dom/client';
import './index.css';
import App from './App';
import reportWebVitals from './reportWebVitals';
import {THEME, TonConnectUIProvider} from "@tonconnect/ui-react";

interface PaymentChannelEvent {
    active: boolean;
    balances: Record<string, string>;
    capacities: Record<string, string>;
    locked: Record<string, string>;
    pendingIn: Record<string, string>;
    address: string;
}

export interface PaymentChannelHistoryItem {
    id: string;
    action: number;
    timestamp: string;
    amounts?: Record<string, string>;
    party?: string;
    isTheir?: boolean;
}

export interface TxMessage {
    to: string;
    amtNano: string;
    body: string;
    stateInit?: string;
}

declare global {
    interface Window {
        startPaymentNetwork: (peerPubKey: string, channelPubKey: string) => void;
        walletAddress: () => string;
        onPaymentNetworkLoaded: (addr: string) => void;
        onPaymentChannelUpdated: (ev: PaymentChannelEvent) => void;
        onPaymentChannelHistoryUpdated: () => void;
        topupChannel: (amount: string, currency: string) => void;
        sendTransfer: (amount: string, to: string, currency?: string) => Promise<string>;
        estimateTransfer: (amount: string, to: string, currency?: string) => string;
        getChannelHistory: (limit: number) => Promise<PaymentChannelHistoryItem[] | null>;
        openChannel: () => void;
        withdrawChannel: (amount: string, currency: string, target: string) => void;
        doTransaction: (reason: string, messages: TxMessage[]) => Promise<string>;
    }
}

const root = ReactDOM.createRoot(
  document.getElementById('root') as HTMLElement
);

root.render(
  <React.StrictMode>
      <TonConnectUIProvider uiPreferences={{ theme: THEME.LIGHT }} manifestUrl={window.location.origin+ "/tonconnect-manifest.json"}>
        <App />
      </TonConnectUIProvider>
  </React.StrictMode>
);

// If you want to start measuring performance in your app, pass a function
// to log results (for example: reportWebVitals(console.log))
// or send to an analytics endpoint. Learn more: https://bit.ly/CRA-vitals
reportWebVitals();
