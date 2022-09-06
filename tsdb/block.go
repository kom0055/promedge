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

package tsdb

import (
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/index"

	"github.com/kom0055/promedge/tsdb/tombstones"
)

// IndexReader provides reading access of serialized index data.
type IndexReader interface {
	// Symbols return an iterator over sorted string symbols that may occur in
	// series' labels and indices. It is not safe to use the returned strings
	// beyond the lifetime of the index reader.
	Symbols() index.StringIter

	// SortedLabelValues returns sorted possible label values.
	SortedLabelValues(name string, matchers ...*labels.Matcher) ([]string, error)

	// LabelValues returns possible label values which may not be sorted.
	LabelValues(name string, matchers ...*labels.Matcher) ([]string, error)

	// Postings returns the postings list iterator for the label pairs.
	// The Postings here contain the offsets to the series inside the index.
	// Found IDs are not strictly required to point to a valid Series, e.g.
	// during background garbage collections. Input values must be sorted.
	Postings(name string, values ...string) (index.Postings, error)

	// SortedPostings returns a postings list that is reordered to be sorted
	// by the label set of the underlying series.
	SortedPostings(index.Postings) index.Postings

	// Series populates the given labels and chunk metas for the series identified
	// by the reference.
	// Returns storage.ErrNotFound if the ref does not resolve to a known series.
	Series(ref storage.SeriesRef, lset *labels.Labels, chks *[]chunks.Meta) error

	// LabelNames returns all the unique label names present in the index in sorted order.
	LabelNames(matchers ...*labels.Matcher) ([]string, error)

	// LabelValueFor returns label value for the given label name in the series referred to by ID.
	// If the series couldn't be found or the series doesn't have the requested label a
	// storage.ErrNotFound is returned as error.
	LabelValueFor(id storage.SeriesRef, label string) (string, error)

	// LabelNamesFor returns all the label names for the series referred to by IDs.
	// The names returned are sorted.
	LabelNamesFor(ids ...storage.SeriesRef) ([]string, error)

	// Close releases the underlying resources of the reader.
	Close() error
}

// ChunkReader provides reading access of serialized time series data.
type ChunkReader interface {
	// Chunk returns the series data chunk with the given reference.
	Chunk(ref chunks.ChunkRef) (chunkenc.Chunk, error)

	// Close releases all underlying resources of the reader.
	Close() error
}

// BlockReader provides reading access to a data block.
type BlockReader interface {
	// Index returns an IndexReader over the block's data.
	Index() (IndexReader, error)

	// Chunks returns a ChunkReader over the block's data.
	Chunks() (ChunkReader, error)

	// Tombstones returns a tombstones.Reader over the block's deleted data.
	Tombstones() (tombstones.Reader, error)

	// Size returns the number of bytes that the block takes up on disk.
	Size() int64
}

func clampInterval(a, b, mint, maxt int64) (int64, int64) {
	if a < mint {
		a = mint
	}
	if b > maxt {
		b = maxt
	}
	return a, b
}
