package db

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

const vchActiveSpecialMetasPrefix = "vchi:ActiveSpecialMetas:"

func virtualChannelMetaKey(key []byte) []byte {
	return []byte("vch:" + base64.StdEncoding.EncodeToString(key))
}

func activeSpecialMetaIndexKey(key []byte) []byte {
	return []byte(vchActiveSpecialMetasPrefix + base64.StdEncoding.EncodeToString(key))
}

func (d *DB) CreateVirtualChannelMeta(ctx context.Context, meta *ConditionalMeta) error {
	key := virtualChannelMetaKey(meta.Key)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if has {
			return ErrAlreadyExists
		}

		data, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		if err = syncActiveSpecialMetasIndex(tx, nil, meta); err != nil {
			return err
		}
		return nil
	})
}

func (d *DB) UpdateVirtualChannelMeta(ctx context.Context, meta *ConditionalMeta) error {
	key := virtualChannelMetaKey(meta.Key)

	return d.Transaction(ctx, func(ctx context.Context) error {
		tx := d.storage.GetExecutor(ctx)

		has, err := tx.Has(key)
		if err != nil {
			return fmt.Errorf("failed to check existance: %w", err)
		}
		if !has {
			return ErrNotFound
		}

		prevRaw, err := tx.Get(key)
		if err != nil {
			return fmt.Errorf("failed to load previous virtual channel meta: %w", err)
		}

		var prev *ConditionalMeta
		if err = json.Unmarshal(prevRaw, &prev); err != nil {
			return fmt.Errorf("failed to decode previous virtual channel meta: %w", err)
		}

		data, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("failed to encode json: %w", err)
		}

		if err = tx.Put(key, data); err != nil {
			return fmt.Errorf("failed to put: %w", err)
		}
		if err = syncActiveSpecialMetasIndex(tx, prev, meta); err != nil {
			return err
		}
		return nil
	})
}

func (d *DB) GetVirtualChannelMeta(ctx context.Context, key []byte) (*ConditionalMeta, error) {
	tx := d.storage.GetExecutor(ctx)

	data, err := tx.Get(virtualChannelMetaKey(key))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to get from db: %w", err)
	}

	var vc *ConditionalMeta
	if err = json.Unmarshal(data, &vc); err != nil {
		return nil, fmt.Errorf("failed to decode json data: %w", err)
	}
	return vc, nil
}

func (d *DB) ForEachActiveSpecialMetaKey(ctx context.Context, fn func(key ed25519.PublicKey) error) error {
	tx := d.storage.GetExecutor(ctx)

	iter := tx.NewIterator([]byte(vchActiveSpecialMetasPrefix), true)
	defer iter.Release()

	for iter.Next() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		val := append([]byte{}, iter.Value()...)
		if len(val) != ed25519.PublicKeySize {
			// Fallback to index key suffix if value is missing/corrupted.
			raw := iter.Key()[len(vchActiveSpecialMetasPrefix):]
			dec, err := base64.StdEncoding.DecodeString(string(raw))
			if err != nil || len(dec) != ed25519.PublicKeySize {
				continue
			}
			val = dec
		}

		if err := fn(ed25519.PublicKey(val)); err != nil {
			return err
		}
	}

	if err := iter.Error(); err != nil {
		return fmt.Errorf("failed to iterate active special metas index: %w", err)
	}

	return nil
}

func syncActiveSpecialMetasIndex(tx Executor, prev, next *ConditionalMeta) error {
	prevIndexed := shouldIndexActiveSpecialMeta(prev)
	nextIndexed := shouldIndexActiveSpecialMeta(next)

	if prevIndexed && (next == nil || !nextIndexed || !bytes.Equal(prev.Key, next.Key)) {
		if err := tx.Delete(activeSpecialMetaIndexKey(prev.Key)); err != nil {
			return fmt.Errorf("failed to delete active special meta index: %w", err)
		}
	}

	if nextIndexed {
		if err := tx.Put(activeSpecialMetaIndexKey(next.Key), append([]byte{}, next.Key...)); err != nil {
			return fmt.Errorf("failed to put active special meta index: %w", err)
		}
	}

	return nil
}

func shouldIndexActiveSpecialMeta(meta *ConditionalMeta) bool {
	if meta == nil || meta.Incoming == nil || meta.Status != ConditionalStateActive {
		return false
	}
	return hasSpecialDetails(meta.SpecialDetails)
}

func hasSpecialDetails(v any) bool {
	if v == nil {
		return false
	}

	raw, err := json.Marshal(v)
	if err != nil {
		return false
	}

	var obj map[string]json.RawMessage
	if err = json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return len(obj) > 0
}
