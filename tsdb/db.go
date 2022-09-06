// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package tsdb implements a time series storage for float64 sample data.
package tsdb

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"golang.org/x/sync/errgroup"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	tsdb_errors "github.com/prometheus/prometheus/tsdb/errors"
	_ "github.com/prometheus/prometheus/tsdb/goversion" // Load the package into main to make sure minium Go version is met.
)

const (
	// Default duration of a block in milliseconds.
	DefaultBlockDuration = int64(20 * time.Second / time.Millisecond)
)

// ErrNotReady is returned if the underlying storage is not ready yet.
var ErrNotReady = errors.New("TSDB not ready")

// DefaultOptions used for the DB. They are sane for setups using
// millisecond precision timestamps.
func DefaultOptions() *Options {
	return &Options{

		IsolationDisabled: defaultIsolationDisabled,
	}
}

// Options of the DB storage.
type Options struct {

	// SeriesLifecycleCallback specifies a list of callbacks that will be called during a lifecycle of a series.
	// It is always a no-op in Prometheus and mainly meant for external users who import TSDB.
	SeriesLifecycleCallback SeriesLifecycleCallback

	// Enables the in memory exemplar storage.
	EnableExemplarStorage bool

	// MaxExemplars sets the size, in # of exemplars stored, of the single circular buffer used to store exemplars in memory.
	// See tsdb/exemplar.go, specifically the CircularExemplarStorage struct and it's constructor NewCircularExemplarStorage.
	MaxExemplars int64

	// Disables isolation between reads and in-flight appends.
	IsolationDisabled bool
}

// DB handles reads and writes of time series falling into
// a hashed partition of a seriedb.
type DB struct {
	logger    log.Logger
	metrics   *dbMetrics
	chunkPool chunkenc.Pool

	// Mutex for that must be held when modifying the general block layout.
	mtx sync.RWMutex

	head *Head

	compactc chan struct{}
	donec    chan struct{}
	stopc    chan struct{}

	// cmtx ensures that compactions and deletions don't run simultaneously.
	cmtx sync.Mutex

	// autoCompactMtx ensures that no compaction gets triggered while
	// changing the autoCompact var.
	autoCompactMtx sync.Mutex
	autoCompact    bool
}

type dbMetrics struct {
	compactionsFailed    prometheus.Counter
	compactionsTriggered prometheus.Counter
	compactionsSkipped   prometheus.Counter
}

func newDBMetrics(r prometheus.Registerer) *dbMetrics {
	m := &dbMetrics{}

	m.compactionsTriggered = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "prometheus_tsdb_compactions_triggered_total",
		Help: "Total number of triggered compactions for the partition.",
	})
	m.compactionsFailed = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "prometheus_tsdb_compactions_failed_total",
		Help: "Total number of compactions that failed for the partition.",
	})

	m.compactionsSkipped = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "prometheus_tsdb_compactions_skipped_total",
		Help: "Total number of skipped compactions due to disabled auto compaction.",
	})

	if r != nil {
		r.MustRegister(
			m.compactionsFailed,
			m.compactionsTriggered,
			m.compactionsSkipped,
		)
	}
	return m
}

// Open returns a new DB in the given directory. If options are empty, DefaultOptions will be used.
func Open(l log.Logger, r prometheus.Registerer, opts *Options) (db *DB, err error) {
	opts = validateOpts(opts)

	return open(l, r, opts)
}

func validateOpts(opts *Options) *Options {
	if opts == nil {
		opts = DefaultOptions()
	}

	return opts
}

// open returns a new DB in the given directory.
// It initializes the lockfile, compactor, and Head, and runs the database.
// It is not safe to open more than one DB in the same directory.
func open(l log.Logger, r prometheus.Registerer, opts *Options) (_ *DB, returnedErr error) {

	if l == nil {
		l = log.NewNopLogger()
	}

	db := &DB{
		logger:      l,
		compactc:    make(chan struct{}, 1),
		donec:       make(chan struct{}),
		stopc:       make(chan struct{}),
		autoCompact: true,
		chunkPool:   chunkenc.NewPool(),
	}
	defer func() {
		// Close files if startup fails somewhere.
		if returnedErr == nil {
			return
		}

		close(db.donec) // DB is never run if it was an error, so close this channel here.

		returnedErr = tsdb_errors.NewMulti(
			returnedErr,
			errors.Wrap(db.Close(), "close DB after failed startup"),
		).Err()
	}()

	var err error

	headOpts := DefaultHeadOptions()
	headOpts.ChunkPool = db.chunkPool
	headOpts.SeriesCallback = opts.SeriesLifecycleCallback
	headOpts.EnableExemplarStorage = opts.EnableExemplarStorage
	headOpts.MaxExemplars.Store(opts.MaxExemplars)
	if opts.IsolationDisabled {
		// We only override this flag if isolation is disabled at DB level. We use the default otherwise.
		headOpts.IsolationDisabled = opts.IsolationDisabled
	}
	db.head, err = NewHead(r, l, headOpts)
	if err != nil {
		return nil, err
	}

	// Register metrics after assigning the head block.
	db.metrics = newDBMetrics(r)

	// Set the min valid time for the ingested samples
	// to be no lower than the maxt of the last block.
	minValidTime := int64(math.MinInt64)

	db.head.Init(minValidTime)

	go db.run()

	return db, nil
}

// StartTime implements the Storage interface.
func (db *DB) StartTime() (int64, error) {
	db.mtx.RLock()
	defer db.mtx.RUnlock()

	return db.head.MinTime(), nil
}

func (db *DB) run() {
	defer close(db.donec)

	backoff := time.Duration(0)

	for {
		select {
		case <-db.stopc:
			return
		case <-time.After(backoff):
		}

		select {
		case <-time.After(1 * time.Minute):

			select {
			case db.compactc <- struct{}{}:
			default:
			}
		case <-db.compactc:
			db.metrics.compactionsTriggered.Inc()

			db.autoCompactMtx.Lock()
			if db.autoCompact {
				if err := db.Compact(); err != nil {
					level.Error(db.logger).Log("msg", "compaction failed", "err", err)
					backoff = exponential(backoff, 1*time.Second, 1*time.Minute)
				} else {
					backoff = 0
				}
			} else {
				db.metrics.compactionsSkipped.Inc()
			}
			db.autoCompactMtx.Unlock()
		case <-db.stopc:
			return
		}
	}
}

// Appender opens a new appender against the database.
func (db *DB) Appender(ctx context.Context) storage.Appender {
	return dbAppender{db: db, Appender: db.head.Appender(ctx)}
}

func (db *DB) ApplyConfig(conf *config.Config) error {
	return db.head.ApplyConfig(conf)
}

// dbAppender wraps the DB's head appender and triggers compactions on commit
// if necessary.
type dbAppender struct {
	storage.Appender
	db *DB
}

var _ storage.GetRef = dbAppender{}

func (a dbAppender) GetRef(lset labels.Labels) (storage.SeriesRef, labels.Labels) {
	if g, ok := a.Appender.(storage.GetRef); ok {
		return g.GetRef(lset)
	}
	return 0, nil
}

func (a dbAppender) Commit() error {
	err := a.Appender.Commit()

	// We could just run this check every few minutes practically. But for benchmarks
	// and high frequency use cases this is the safer way.
	if a.db.head.compactable() {
		select {
		case a.db.compactc <- struct{}{}:
		default:
		}
	}
	return err
}

// Compact data if possible. After successful compaction blocks are reloaded
// which will also delete the blocks that fall out of the retention window.
// Old blocks are only deleted on reloadBlocks based on the new block's parent information.
// See DB.reloadBlocks documentation for further information.
func (db *DB) Compact() (returnErr error) {
	db.cmtx.Lock()
	defer db.cmtx.Unlock()
	defer func() {
		if returnErr != nil && !errors.Is(returnErr, context.Canceled) {
			// If we got an error because context was canceled then we're most likely
			// shutting down TSDB and we don't need to report this on metrics
			db.metrics.compactionsFailed.Inc()
		}
	}()

	start := time.Now()
	// Check whether we have pending head blocks that are ready to be persisted.
	// They have the highest priority.
	for {
		select {
		case <-db.stopc:
			return nil
		default:
		}
		if !db.head.compactable() {
			break
		}
		mint := db.head.MinTime()
		maxt := rangeForTimestamp(mint, db.head.chunkRange.Load())

		// Wrap head into a range that bounds all reads to it.
		// We remove 1 millisecond from maxt because block
		// intervals are half-open: [b.MinTime, b.MaxTime). But
		// chunk intervals are closed: [c.MinTime, c.MaxTime];
		// so in order to make sure that overlaps are evaluated
		// consistently, we explicitly remove the last value
		// from the block interval here.
		if err := db.compactHead(NewRangeHead(db.head, mint, maxt-1)); err != nil {
			return errors.Wrap(err, "compact head")
		}
	}

	compactionDuration := time.Since(start)
	if compactionDuration.Milliseconds() > db.head.chunkRange.Load() {
		level.Warn(db.logger).Log(
			"msg", "Head compaction took longer than the block time range, compactions are falling behind and won't be able to catch up",
			"duration", compactionDuration.String(),
			"block_range", db.head.chunkRange.Load(),
		)
	}
	return nil
}

// CompactHead compacts the given RangeHead.
func (db *DB) CompactHead(head *RangeHead) error {
	db.cmtx.Lock()
	defer db.cmtx.Unlock()

	if err := db.compactHead(head); err != nil {
		return errors.Wrap(err, "compact head")
	}

	return nil
}

// compactHead compacts the given RangeHead.
// The compaction mutex should be held before calling this method.
func (db *DB) compactHead(head *RangeHead) error {

	if err := db.head.truncateMemory(head.BlockMaxTime()); err != nil {
		return errors.Wrap(err, "head memory truncate")
	}
	return nil
}

// TimeRange specifies minTime and maxTime range.
type TimeRange struct {
	Min, Max int64
}

func (db *DB) String() string {
	return "HEAD"
}

// Head returns the databases's head.
func (db *DB) Head() *Head {
	return db.head
}

// Close the partition.
func (db *DB) Close() error {
	close(db.stopc)

	<-db.donec

	return nil
}

// DisableCompactions disables auto compactions.
func (db *DB) DisableCompactions() {
	db.autoCompactMtx.Lock()
	defer db.autoCompactMtx.Unlock()

	db.autoCompact = false
	level.Info(db.logger).Log("msg", "Compactions disabled")
}

// EnableCompactions enables auto compactions.
func (db *DB) EnableCompactions() {
	db.autoCompactMtx.Lock()
	defer db.autoCompactMtx.Unlock()

	db.autoCompact = true
	level.Info(db.logger).Log("msg", "Compactions enabled")
}

// Querier returns a new querier over the data partition for the given time range.
func (db *DB) Querier(_ context.Context, mint, maxt int64) (storage.Querier, error) {

	db.mtx.RLock()
	defer db.mtx.RUnlock()

	var blockQueriers []storage.Querier

	if maxt >= db.head.MinTime() {
		rh := NewRangeHead(db.head, mint, maxt)
		headQuerier, err := NewBlockQuerier(rh, mint, maxt)
		if err != nil {
			return nil, errors.Wrapf(err, "open querier for head %s", rh)
		}

		// Getting the querier above registers itself in the queue that the truncation waits on.
		// So if the querier is currently not colliding with any truncation, we can continue to use it and still
		// won't run into a race later since any truncation that comes after will wait on this querier if it overlaps.
		shouldClose, getNew, newMint := db.head.IsQuerierCollidingWithTruncation(mint, maxt)
		if shouldClose {
			if err := headQuerier.Close(); err != nil {
				return nil, errors.Wrapf(err, "closing head querier %s", rh)
			}
			headQuerier = nil
		}
		if getNew {
			rh := NewRangeHead(db.head, newMint, maxt)
			headQuerier, err = NewBlockQuerier(rh, newMint, maxt)
			if err != nil {
				return nil, errors.Wrapf(err, "open querier for head while getting new querier %s", rh)
			}
		}
		if headQuerier != nil {
			blockQueriers = []storage.Querier{headQuerier}
		}
	}

	return storage.NewMergeQuerier(blockQueriers, nil, storage.ChainedSeriesMerge), nil
}

// ChunkQuerier returns a new chunk querier over the data partition for the given time range.
func (db *DB) ChunkQuerier(_ context.Context, mint, maxt int64) (storage.ChunkQuerier, error) {

	db.mtx.RLock()
	defer db.mtx.RUnlock()

	var blockQueriers []storage.ChunkQuerier
	if maxt >= db.head.MinTime() {
		rh := NewRangeHead(db.head, mint, maxt)
		var err error
		headQuerier, err := NewBlockChunkQuerier(rh, mint, maxt)
		if err != nil {
			return nil, errors.Wrapf(err, "open querier for head %s", rh)
		}

		// Getting the querier above registers itself in the queue that the truncation waits on.
		// So if the querier is currently not colliding with any truncation, we can continue to use it and still
		// won't run into a race later since any truncation that comes after will wait on this querier if it overlaps.
		shouldClose, getNew, newMint := db.head.IsQuerierCollidingWithTruncation(mint, maxt)
		if shouldClose {
			if err := headQuerier.Close(); err != nil {
				return nil, errors.Wrapf(err, "closing head querier %s", rh)
			}
			headQuerier = nil
		}
		if getNew {
			rh := NewRangeHead(db.head, newMint, maxt)
			headQuerier, err = NewBlockChunkQuerier(rh, newMint, maxt)
			if err != nil {
				return nil, errors.Wrapf(err, "open querier for head while getting new querier %s", rh)
			}
		}
		if headQuerier != nil {
			blockQueriers = []storage.ChunkQuerier{headQuerier}
		}
	}

	return storage.NewMergeChunkQuerier(blockQueriers, nil, storage.NewCompactingChunkSeriesMerger(storage.ChainedSeriesMerge)), nil
}

func (db *DB) ExemplarQuerier(ctx context.Context) (storage.ExemplarQuerier, error) {
	return db.head.exemplars.ExemplarQuerier(ctx)
}

func rangeForTimestamp(t, width int64) (maxt int64) {
	return (t/width)*width + width
}

// Delete implements deletion of metrics. It only has atomicity guarantees on a per-block basis.
func (db *DB) Delete(mint, maxt int64, ms ...*labels.Matcher) error {
	db.cmtx.Lock()
	defer db.cmtx.Unlock()

	var g errgroup.Group

	db.mtx.RLock()
	defer db.mtx.RUnlock()

	if db.head.OverlapsClosedInterval(mint, maxt) {
		g.Go(func() error {
			return db.head.Delete(mint, maxt, ms...)
		})
	}

	return g.Wait()
}

// CleanTombstones re-writes any blocks with tombstones.
func (db *DB) CleanTombstones() (err error) {

	return nil
}

func exponential(d, min, max time.Duration) time.Duration {
	d *= 2
	if d < min {
		d = min
	}
	if d > max {
		d = max
	}
	return d
}
