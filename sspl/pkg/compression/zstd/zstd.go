//go:build mutagensspl

// Copyright (c) 2023-present Mutagen IO, Inc.
//
// This program is free software: you can redistribute it and/or modify it under
// the terms of the Server Side Public License, version 1, as published by
// MongoDB, Inc.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
// FOR A PARTICULAR PURPOSE. See the Server Side Public License for more
// details.
//
// You should have received a copy of the Server Side Public License along with
// this program. If not, see
// <http://www.mongodb.com/licensing/server-side-public-license>.

package zstd

import (
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/mutagen-io/mutagen/pkg/stream"
)

// NewDecompressor creates a new Zstandard decompressor that reads from the
// specified stream. Decoding is single-goroutine with low-memory buffering:
// these decompressors live on session control streams, sit mostly idle, and
// retain their state for the life of the connection, so per-stream state
// matters more than throughput.
func NewDecompressor(compressed io.Reader) io.ReadCloser {
	// Create the decompressor. We check for errors, but we don't include them
	// as part of the interface because they can only occur with an invalid
	// decompressor configuration (which can't occur with these fixed options).
	decompressor, err := zstd.NewReader(compressed,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
	)
	if err != nil {
		panic("Zstandard decompressor construction failed")
	}

	// Adapt the decompressor to the expected interface.
	return decompressor.IOReadCloser()
}

// NewCompressor creates a new Zstandard compressor that writes to the
// specified stream. Encoding is single-goroutine with a 1 MiB window rather
// than the default GOMAXPROCS-wide encoder state and 8 MiB window: these
// compressors live on session control streams, sit mostly idle, and retain
// their state for the life of the connection, so per-stream state matters
// more than throughput. The smaller window costs some compression ratio on
// large transfers (initial snapshots); steady-state deltas are far smaller
// than the window either way.
func NewCompressor(compressed io.Writer) stream.WriteFlushCloser {
	// Create the compressor. We check for errors, but we don't include them as
	// part of the interface because they can only occur with an invalid
	// compressor configuration (which can't occur with these fixed options).
	compressor, err := zstd.NewWriter(compressed,
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(1<<20),
	)
	if err != nil {
		panic("Zstandard compressor construction failed")
	}

	// Success.
	return compressor
}
