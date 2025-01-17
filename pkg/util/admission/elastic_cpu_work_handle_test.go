// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package admission

import (
	"testing"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/stretchr/testify/require"
)

func TestElasticCPUWorkHandle(t *testing.T) {
	overrideMu := struct {
		syncutil.Mutex
		running time.Duration
	}{}
	setRunning := func(running time.Duration) {
		overrideMu.Lock()
		defer overrideMu.Unlock()
		overrideMu.running = running
	}

	const allotment = 100 * time.Millisecond
	const zero = time.Duration(0)

	setRunning(zero)

	handle := newElasticCPUWorkHandle(allotment)
	handle.testingOverrideRunningTime = func() time.Duration {
		overrideMu.Lock()
		defer overrideMu.Unlock()
		return overrideMu.running
	}

	{ // Assert on zero values.
		require.Equal(t, 0, handle.itersSinceLastCheck)
		require.Equal(t, 0, handle.itersUntilCheck)
		require.Equal(t, zero, handle.runningTimeAtLastCheck)
		require.Equal(t, zero, handle.differenceWithAllottedAtLastCheck)
	}

	{ // Invoke once; we should see internal state primed for future iterations.
		overLimit, difference := handle.OverLimit()
		require.False(t, overLimit)
		require.Equal(t, allotment, difference)
		require.Equal(t, 0, handle.itersSinceLastCheck)
		require.Equal(t, 1, handle.itersUntilCheck)
		require.Equal(t, zero, handle.runningTimeAtLastCheck)
		require.Equal(t, allotment, handle.differenceWithAllottedAtLastCheck)
	}

	{ // Invoke while under the 1ms running duration. We should start doubling our itersUntilCheck count.
		setRunning(100 * time.Microsecond)
		expDifference := 99*time.Millisecond + 900*time.Microsecond

		overLimit, difference := handle.OverLimit()
		require.False(t, overLimit)
		require.Equal(t, expDifference, difference)
		require.Equal(t, 0, handle.itersSinceLastCheck)
		require.Equal(t, 2, handle.itersUntilCheck)
		require.Equal(t, 100*time.Microsecond, handle.runningTimeAtLastCheck)
		require.Equal(t, expDifference, handle.differenceWithAllottedAtLastCheck)

		_, _ = handle.OverLimit()
		require.Equal(t, 1, handle.itersSinceLastCheck) // see increase of +1
		require.Equal(t, 2, handle.itersUntilCheck)
		require.Equal(t, 100*time.Microsecond, handle.runningTimeAtLastCheck)
		require.Equal(t, expDifference, handle.differenceWithAllottedAtLastCheck)

		_, _ = handle.OverLimit()
		require.Equal(t, 0, handle.itersSinceLastCheck) // see reset of value
		require.Equal(t, 4, handle.itersUntilCheck)     // see doubling of value
		require.Equal(t, 100*time.Microsecond, handle.runningTimeAtLastCheck)
		require.Equal(t, expDifference, handle.differenceWithAllottedAtLastCheck)
	}

	{ // Cross the 1ms running mark. Loop until we observe as much.
		setRunning(time.Millisecond + 100*time.Microsecond) // set to 1.1 ms to estimate 4 iters since last check (at 100us)
		expDifference := 98*time.Millisecond + 900*time.Microsecond

		internalIters := 0
		for {
			internalIters++

			overLimit, _ := handle.OverLimit()
			require.False(t, overLimit)
			if expDifference != handle.differenceWithAllottedAtLastCheck {
				continue
			}

			if internalIters > 4 {
				t.Fatalf("exceeded expected internal iteration count of 4")
			}
			break
		}
		require.Equal(t, 4, internalIters)
		require.Equal(t, 0, handle.itersSinceLastCheck) // see reset of value
		require.Equal(t, 4, handle.itersUntilCheck)     // see value remain static
		require.Equal(t, time.Millisecond+100*time.Microsecond, handle.runningTimeAtLastCheck)
		require.Equal(t, expDifference, handle.differenceWithAllottedAtLastCheck)
	}

	{ // Ensure steady estimation (4 iters per ms of running time) if iteration duration is steady.
		for i := 1; i <= 10; i++ {
			setRunning(handle.runningTimeAtLastCheck + time.Millisecond)

			internalIters := 0
			for {
				internalIters++

				overLimit, _ := handle.OverLimit()
				require.False(t, overLimit)
				if handle.itersSinceLastCheck == 0 {
					break
				}

				if internalIters > 4 {
					t.Fatalf("exceeded expected internal iteration count of 4")
				}
			}

			require.Equal(t, 4, internalIters)
			require.Equal(t, 0, handle.itersSinceLastCheck) // see reset of value
			require.Equal(t, 4, handle.itersUntilCheck)     // see value remain static
			require.Equal(t, time.Duration((int64(i+1)*time.Millisecond.Nanoseconds())+(100*time.Microsecond.Nanoseconds())), handle.runningTimeAtLastCheck)
		}
	}

	{ // We'll double our estimates again if we observe more iterations in 1ms of running time.
		for i := 0; i < 4; i++ {
			overLimit, _ := handle.OverLimit()
			require.False(t, overLimit)
		}

		require.Equal(t, 0, handle.itersSinceLastCheck) // see reset of value
		require.Equal(t, 8, handle.itersUntilCheck)     // see value double again
	}

	{
		setRunning(allotment + 50*time.Microsecond)
		for i := 0; i < 7; i++ {
			overLimit, _ := handle.OverLimit()
			require.False(t, overLimit)
		}

		overLimit, difference := handle.OverLimit()
		require.True(t, overLimit)
		require.Equal(t, 50*time.Microsecond, difference)
	}
}
