package tsdb

import "github.com/prometheus/prometheus/tsdb/chunkenc"

type buddyChunk interface {
	chunkenc.Chunk
	GetCurr() chunkenc.Chunk
}

type buddy struct {
	prev, curr chunkenc.Chunk
}

func newBuddy() buddyChunk {

	return &buddy{
		prev: chunkenc.NewXORChunk(),
		curr: chunkenc.NewXORChunk(),
	}
}

func newBuddyWithPrev(prev chunkenc.Chunk) buddyChunk {
	if prev == nil {
		prev = chunkenc.NewXORChunk()
	}

	return &buddy{
		prev: prev,
		curr: chunkenc.NewXORChunk(),
	}
}

func (b *buddy) Bytes() []byte {
	return b.curr.Bytes()
}

func (b *buddy) Encoding() chunkenc.Encoding {
	return chunkenc.EncXOR
}

func (b *buddy) Appender() (chunkenc.Appender, error) {
	return b.curr.Appender()
}

func (b *buddy) Iterator(_ chunkenc.Iterator) chunkenc.Iterator {
	return &buddyIterator{
		prev:   b.prev.Iterator(nil),
		curr:   b.curr.Iterator(nil),
		isCurr: false,
	}
}

func (b *buddy) NumSamples() int {
	return b.prev.NumSamples() + b.curr.NumSamples()
}

func (b *buddy) Compact() {
	b.curr.Compact()
}

func (b *buddy) GetCurr() chunkenc.Chunk {
	return b.curr
}

type buddyIterator struct {
	prev, curr chunkenc.Iterator
	isCurr     bool
}

func (b buddyIterator) Next() bool {
	if b.isCurr {
		return b.curr.Next()
	}
	if b.prev.Next() {
		return true
	}
	b.isCurr = true
	return b.curr.Next()
}

func (b buddyIterator) Seek(t int64) bool {
	if b.isCurr {
		return b.curr.Seek(t)
	}
	if b.prev.Seek(t) {
		return true
	}

	b.isCurr = true
	return b.curr.Seek(t)
}

func (b buddyIterator) At() (int64, float64) {
	if b.isCurr {
		return b.curr.At()
	}
	return b.prev.At()
}

func (b buddyIterator) Err() error {
	if b.isCurr {
		return b.curr.Err()
	}
	return b.prev.Err()
}
