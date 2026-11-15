// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright (c) 2024 Matthew Penner

package filesystem

import (
	"math"
	"sync/atomic"
)

// diskQuota tracks the disk usage of a Filesystem against its limit. It was
// ported from the former internal/ufs Quota type when the filesystem moved to
// os.Root.
//
// NOTE: this is not a full quota filesystem. It only provides the utilities for
// tracking and checking usage; callers are responsible for accounting writes
// and deletions against it.
type diskQuota struct {
	// limit is the size limit of the filesystem.
	//
	// A limit of `-1` disables any write operation from being performed.
	// A limit of `0` disables any limit checking.
	limit atomic.Int64

	// usage is the current usage of the filesystem.
	//
	// If usage is set to `-1`, it hasn't been calculated yet.
	usage atomic.Int64
}

// Limit returns the limit of the filesystem.
func (q *diskQuota) Limit() int64 {
	return q.limit.Load()
}

// SetLimit sets the limit of the filesystem and returns the previous limit.
func (q *diskQuota) SetLimit(newLimit int64) int64 {
	return q.limit.Swap(newLimit)
}

// Usage returns the current usage of the filesystem.
func (q *diskQuota) Usage() int64 {
	return q.usage.Load()
}

// SetUsage sets the total usage of the filesystem and returns the previous usage.
func (q *diskQuota) SetUsage(newUsage int64) int64 {
	return q.usage.Swap(newUsage)
}

// Add adds `i` to the tracked usage total, saturating at math.MaxInt64 and
// clamping at zero instead of overflowing.
func (q *diskQuota) Add(i int64) int64 {
	for {
		usage := q.Usage()
		var next int64

		switch {
		case i > 0:
			if usage > math.MaxInt64-i {
				next = math.MaxInt64
			} else {
				next = usage + i
			}
		case i < 0:
			if i == math.MinInt64 || usage <= -i {
				next = 0
			} else {
				next = usage + i
			}
		default:
			return usage
		}

		if q.usage.CompareAndSwap(usage, next) {
			return next
		}
	}
}

// CanFit checks if the given size can fit in the filesystem without exceeding
// the limit of the filesystem.
func (q *diskQuota) CanFit(size int64) bool {
	limit := q.Limit()
	switch limit {
	case -1:
		// A limit of -1 means no write operations are allowed.
		return false
	case 0:
		// A limit of 0 means unlimited.
		return true
	}

	usage := q.Usage()
	if usage == -1 {
		// We don't know what the current usage is yet.
		return true
	}

	if size <= 0 {
		return true
	}

	if usage >= limit {
		return false
	}

	return size <= limit-usage
}
