package cache

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var bucket = []byte("cache")

// sweepBatch is how many keys one sweep transaction looks at, so the sweep
// never holds bbolt's single writer lock for long.
const sweepBatch = 1000

// Bolt is a Cache in a bbolt file. It's meant for a single process: the
// file is locked, and other instances wouldn't see its invalidations.
//
// Values are stored as an 8-byte big-endian expiry (Unix nanoseconds, 0 for
// never) followed by the value.
type Bolt struct {
	db   *bolt.DB
	now  func() time.Time
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// OpenBolt creates a fresh cache at path, sweeping expired keys every
// sweepEvery. Any existing file is removed first: writes aren't synced to
// disk, so after a crash it may be corrupt, and it may be stale anyway.
func OpenBolt(path string, sweepEvery time.Duration) (*Bolt, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second, NoSync: true, NoFreelistSync: true})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket(bucket)
		return err
	}); err != nil {
		db.Close()
		return nil, err
	}
	c := &Bolt{db: db, now: time.Now, stop: make(chan struct{}), done: make(chan struct{})}
	go c.sweepLoop(sweepEvery)
	return c, nil
}

func (c *Bolt) Get(key string) ([]byte, bool, error) {
	var val []byte
	var ok bool
	err := c.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get([]byte(key))
		if v == nil || c.expired(v) {
			return nil
		}
		// v is only valid inside the transaction.
		val, ok = append([]byte(nil), v[8:]...), true
		return nil
	})
	return val, ok, err
}

func (c *Bolt) Set(key string, val []byte, ttl time.Duration) error {
	v := make([]byte, 8+len(val))
	if ttl > 0 {
		binary.BigEndian.PutUint64(v, uint64(c.now().Add(ttl).UnixNano()))
	}
	copy(v[8:], val)
	return c.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(key), v)
	})
}

func (c *Bolt) Delete(keys ...string) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		for _, k := range keys {
			if err := b.Delete([]byte(k)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close stops the sweeper and closes the file. Later calls do nothing.
func (c *Bolt) Close() error {
	var err error
	c.once.Do(func() {
		close(c.stop)
		<-c.done
		err = c.db.Close()
	})
	return err
}

func (c *Bolt) expired(v []byte) bool {
	exp := binary.BigEndian.Uint64(v)
	return exp != 0 && int64(exp) <= c.now().UnixNano()
}

func (c *Bolt) sweepLoop(every time.Duration) {
	defer close(c.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			if err := c.sweep(); err != nil {
				slog.Warn("cache sweep", "err", err)
			}
		}
	}
}

// sweep deletes expired keys, one batch per transaction. Keys are collected
// first and deleted after, since deleting under a bbolt cursor can make
// Next skip keys.
func (c *Bolt) sweep() error {
	var from []byte // first key the next batch looks at; nil is the start
	for {
		var next []byte
		err := c.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucket)
			cur := b.Cursor()
			k, v := cur.First()
			if from != nil {
				k, v = cur.Seek(from)
			}
			var dead [][]byte
			for n := 0; k != nil && n < sweepBatch; n++ {
				if c.expired(v) {
					dead = append(dead, append([]byte(nil), k...))
				}
				k, v = cur.Next()
			}
			if k != nil {
				next = append([]byte(nil), k...)
			}
			for _, k := range dead {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil || next == nil {
			return err
		}
		from = next
	}
}
