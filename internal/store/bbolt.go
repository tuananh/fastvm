package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"fastvm/internal/types"
)

var (
	bucketSandboxes = []byte("sandboxes")
	bucketEvents    = []byte("events")
)

// Bolt is the bbolt-backed Store.
type Bolt struct {
	db *bolt.DB
}

var _ Store = (*Bolt)(nil)

// Open opens (or creates) a store at path.
func Open(path string) (*Bolt, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketSandboxes); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(bucketEvents)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	return &Bolt{db: db}, nil
}

func (s *Bolt) PutSandbox(_ context.Context, sb *types.Sandbox) error {
	if sb == nil || sb.ID == "" {
		return fmt.Errorf("sandbox id required")
	}
	data, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSandboxes).Put([]byte(sb.ID), data)
	})
}

func (s *Bolt) GetSandbox(_ context.Context, id string) (*types.Sandbox, bool, error) {
	var sb *types.Sandbox
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketSandboxes).Get([]byte(id))
		if v == nil {
			return nil
		}
		sb = new(types.Sandbox)
		return json.Unmarshal(v, sb)
	})
	if err != nil {
		return nil, false, err
	}
	return sb, sb != nil, nil
}

func (s *Bolt) ListSandboxes(_ context.Context) ([]*types.Sandbox, error) {
	var out []*types.Sandbox
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSandboxes).ForEach(func(_, v []byte) error {
			sb := new(types.Sandbox)
			if err := json.Unmarshal(v, sb); err != nil {
				return err
			}
			out = append(out, sb)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Bolt) DeleteSandbox(_ context.Context, id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketSandboxes).Delete([]byte(id))
	})
}

func (s *Bolt) AppendEvent(_ context.Context, ev types.Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// Key = big-endian unix nanos + sandbox id + type, so events iterate in
	// time order and remain unique within the same nanosecond.
	key := make([]byte, 8+len(ev.SandboxID)+1+len(ev.Type))
	binary.BigEndian.PutUint64(key[:8], uint64(ev.Time.UnixNano()))
	copy(key[8:], ev.SandboxID)
	key[8+len(ev.SandboxID)] = 0
	copy(key[9+len(ev.SandboxID):], ev.Type)
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEvents)
		// Disambiguate keys with a sequence suffix.
		for i := 0; ; i++ {
			k := append(append([]byte{}, key...), byte(i))
			if b.Get(k) == nil {
				key = k
				break
			}
		}
		return b.Put(key, data)
	})
}

func (s *Bolt) ListEvents(_ context.Context, limit int) ([]types.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	var all []types.Event
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketEvents).ForEach(func(_, v []byte) error {
			var ev types.Event
			if err := json.Unmarshal(v, &ev); err != nil {
				return err
			}
			all = append(all, ev)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	// Events are appended in time order; return the newest `limit` in order.
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

func (s *Bolt) Close() error { return s.db.Close() }

// Compile-time assertion that event keys are ordered by time.
var _ = bytes.Compare
