package db

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xssnick/ton-payment-network/pkg/payments"
	"github.com/xssnick/tonutils-go/tlb"
	"github.com/xssnick/tonutils-go/tvm/cell"
	"time"
)

func (d *DB) SetOnChannelUpdated(f func(ctx context.Context, ch *Channel, statusChanged bool)) {
	d.onChannelStateChange = f
}

func (d *DB) SetOnChannelHistoryUpdated(f func(ctx context.Context, ch *Channel, item ChannelHistoryItem)) {
	d.onChannelHistoryUpdate = f
}

func (d *DB) GetOnChannelUpdated() func(ctx context.Context, ch *Channel, statusChanged bool) {
	return d.onChannelStateChange
}

func (d *DB) GetOnChannelHistoryUpdated() func(ctx context.Context, ch *Channel, item ChannelHistoryItem) {
	return d.onChannelHistoryUpdate
}

func (d *DB) AddUrgentPeer(ctx context.Context, peerAddress []byte) error {
	if len(peerAddress) != 32 {
		return fmt.Errorf("invalid peer address length: expected 32 bytes, got %d", len(peerAddress))
	}

	key := []byte("urgent-peer:" + base64.StdEncoding.EncodeToString(peerAddress))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existence: %w", err)
		}
		if has {
			// Peer already exists, no need to add
			return nil
		}

		if err = tx.Put(key, []byte{}); err != nil {
			return fmt.Errorf("failed to add urgent peer to db: %w", err)
		}
		return nil
	})
}

func (d *DB) RemoveUrgentPeer(ctx context.Context, peerAddress []byte) error {
	if len(peerAddress) != 32 {
		return fmt.Errorf("invalid peer address length: expected 32 bytes, got %d", len(peerAddress))
	}

	key := []byte("urgent-peer:" + base64.StdEncoding.EncodeToString(peerAddress))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existence: %w", err)
		}
		if !has {
			// reverse compatibility
			key = []byte("urgent-peer:" + string(peerAddress))

			has, err = tx.Has(key)
			if err != nil {
				return fmt.Errorf("failed to check alt existence: %w", err)
			}
			if !has {
				// Peer doesn't exist, nothing to remove
				return nil
			}
		}

		if err = tx.Delete(key); err != nil {
			return fmt.Errorf("failed to remove urgent peer from db: %w", err)
		}
		return nil
	})
}

func (d *DB) GetUrgentPeers(ctx context.Context) ([][]byte, error) {
	tx := d.storage.GetExecutor(ctx)

	iter := tx.NewIterator([]byte("urgent-peer:"), true)
	defer iter.Release()

	var peers [][]byte
	for iter.Next() {
		peerAddress := iter.Key()[len("urgent-peer:"):]
		if len(peerAddress) != 32 { // reverse compatibility before migration
			peerAddress, _ = base64.StdEncoding.DecodeString(string(peerAddress))
			if len(peerAddress) != 32 {
				return nil, fmt.Errorf("invalid peer address length: expected 32 bytes, got %d", len(peerAddress))
			}
		}
		peers = append(peers, append([]byte{}, peerAddress...))
	}

	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("failed to retrieve urgent peers: %w", err)
	}

	return peers, nil
}

func (d *DB) CreateChannel(ctx context.Context, channel *Channel) error {
	key := []byte("ch:" + channel.Our.Address)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return ErrAlreadyExists
		}

		channel.DBVersion = time.Now().UnixNano()
		data, err := json.Marshal(channel)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		if d.onChannelStateChange != nil {
			d.onChannelStateChange(ctx, channel, true)
		}
		return nil
	})
}

func (d *DB) CreateActionCode(ctx context.Context, action *cell.Cell) error {
	key := []byte("ac:" + base64.StdEncoding.EncodeToString(action.Hash()))

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return nil
		}

		data, err := json.Marshal(action)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		return nil
	})
}

func (d *DB) GetActionCode(ctx context.Context, hash []byte) (*cell.Cell, error) {
	tx := d.storage.GetExecutor(ctx)

	key := []byte("ac:" + base64.StdEncoding.EncodeToString(hash))

	data, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var code *cell.Cell
	if err = json.Unmarshal(data, &code); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}

	if !bytes.Equal(code.Hash(), hash) {
		return nil, fmt.Errorf("invalid action code hash")
	}

	return code, nil
}

func (d *DB) UpdateChannel(ctx context.Context, channel *Channel) error {
	key := []byte("ch:" + channel.Our.Address)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		curChannel, err := d.GetChannel(ctx, channel.Our.Address)
		if err != nil {
			return fmt.Errorf("failed to get channel: %w", err)
		}

		if curChannel.DBVersion != channel.DBVersion {
			return fmt.Errorf("version mismatch retry changes (current %d, update %d)", curChannel.DBVersion, channel.DBVersion)
		}

		channel.DBVersion = time.Now().UnixNano()
		data, err := json.Marshal(channel)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		if d.onChannelStateChange != nil {
			d.onChannelStateChange(ctx, channel, curChannel.Status != channel.Status)
		}

		return nil
	})
}

func (d *DB) GetChannel(ctx context.Context, addr string) (*Channel, error) {
	tx := d.storage.GetExecutor(ctx)

	key := []byte("ch:" + addr)

	data, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var channel *Channel
	if err = json.Unmarshal(data, &channel); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}

	return channel, nil
}

func (d *DB) GetChannels(ctx context.Context, key ed25519.PublicKey, status ChannelStatus) ([]*Channel, error) {
	tx := d.storage.GetExecutor(ctx)

	iter := tx.NewIterator([]byte("ch:"), true)
	defer iter.Release()

	// TODO: optimize, use indexing
	var channels []*Channel
	for iter.Next() {
		var channel *Channel
		if err := json.Unmarshal(iter.Value(), &channel); err != nil {
			return nil, fmt.Errorf("failed to decode json data: %w", err)
		}

		if (status == ChannelStateAny || channel.Status == status) && (key == nil || bytes.Equal(channel.Their.OnchainInfo.Key, key)) {
			channels = append(channels, channel)
		}
	}

	if err := iter.Error(); err != nil {
		return nil, err
	}

	return channels, nil
}

func (d *DB) CreateChannelEvent(ctx context.Context, channel *Channel, at time.Time, item ChannelHistoryItem) error {
	key := channel.getChannelHistoryIndexKey(at, item.Action)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return nil
		}

		data, err := json.Marshal(item)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		if d.onChannelHistoryUpdate != nil {
			d.onChannelHistoryUpdate(ctx, channel, item)
		}

		return nil
	})
}

func (d *DB) SaveChannelPendingState(ctx context.Context, channel *Channel, body payments.StateBody) error {
	state := PendingChannelState{
		Seqno:     body.Seqno,
		OurData:   channel.Our.Data,
		TheirData: channel.Their.Data,
	}

	key, err := channel.getSavedChannelStateKey(body)
	if err != nil {
		return err
	}

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return nil
		}

		data, err := json.Marshal(state)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}

		return nil
	})
}

func (d *DB) GetChannelPendingState(ctx context.Context, channel *Channel, body payments.StateBody) (*PendingChannelState, error) {
	tx := d.storage.GetExecutor(ctx)

	key, err := channel.getSavedChannelStateKey(body)
	if err != nil {
		return nil, fmt.Errorf("failed to generate key: %w", err)
	}

	data, err := tx.Get(key)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var state PendingChannelState
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}

	return &state, nil
}

func (d *DB) CleanupChannelPendingStates(ctx context.Context, channel *Channel, body payments.StateBody) error {
	key, err := channel.getSavedChannelStateKey(body)
	if err != nil {
		return fmt.Errorf("failed to generate key: %w", err)
	}

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		if err = tx.Delete(key); err != nil {
			return fmt.Errorf("failed to delete: %w", err)
		}

		// Delete all states with seqno less than current
		seqnoBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(seqnoBytes, body.Seqno)
		prefix := append([]byte("chp:"+channel.Our.Address+":"), seqnoBytes...)

		iter := tx.NewIterator([]byte("chp:"+channel.Our.Address+":"), true)
		defer iter.Release()

		for iter.Next() {
			ikey := iter.Key()
			if bytes.Compare(ikey, prefix) >= 0 {
				break
			}
			if err = tx.Delete(ikey); err != nil {
				return fmt.Errorf("failed to delete old state: %w", err)
			}
		}

		return nil
	})
}

func (d *DB) GetChannelsHistoryByPeriod(
	ctx context.Context, addr string, limit int,
	before, after *time.Time,
) ([]ChannelHistoryItem, error) {
	tx := d.storage.GetExecutor(ctx)

	historyKeyPrefix := []byte("chs:" + addr + ":")
	iter := tx.NewIterator(historyKeyPrefix, false)
	defer iter.Release()

	var results []ChannelHistoryItem

	for iter.Next() {
		k := iter.Key()

		if len(k) < len(historyKeyPrefix)+8 {
			continue
		}

		tsBytes := k[len(historyKeyPrefix) : len(historyKeyPrefix)+8]
		ts := time.Unix(0, int64(binary.BigEndian.Uint64(tsBytes)))

		if after != nil && ts.Before(*after) {
			continue
		}
		if before != nil && ts.After(*before) {
			break
		}

		var hist ChannelHistoryItem
		if err := json.Unmarshal(iter.Value(), &hist); err != nil {
			return nil, fmt.Errorf("failed to decode history json: %w", err)
		}
		hist.At = ts

		results = append(results, hist)

		if limit > 0 && len(results) >= limit {
			break
		}
	}

	return results, nil
}

func (ch *Channel) getChannelHistoryIndexKey(at time.Time, typ ChannelHistoryEventType) []byte {
	atBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(atBytes, uint64(at.UTC().UnixNano()))

	return append(append([]byte("chs:"+ch.Our.Address+":"), atBytes...), fmt.Sprintf("%d", typ)...)
}

func (ch *Channel) getSavedChannelStateKey(body payments.StateBody) ([]byte, error) {
	c, err := tlb.ToCell(body)
	if err != nil {
		return nil, err
	}

	seq := make([]byte, 8)
	binary.BigEndian.PutUint64(seq, body.Seqno)

	return append(append([]byte("chp:"+ch.Our.Address+":"), seq...), c.Hash()...), nil
}
