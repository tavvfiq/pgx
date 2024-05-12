package pgxpool_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgxpool"
)

type testTracer struct {
	traceAcquireStart func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireStartData) context.Context
	traceAcquireEnd   func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireEndData)
	traceReleaseStart func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceReleaseStartData) context.Context
	traceReleaseEnd   func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceReleaseEndData)
}

type ctxKey string

func (tt *testTracer) TraceAcquireStart(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireStartData) context.Context {
	if tt.traceAcquireStart != nil {
		return tt.traceAcquireStart(ctx, pool, data)
	}
	return ctx
}

func (tt *testTracer) TraceAcquireEnd(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
	if tt.traceAcquireEnd != nil {
		tt.traceAcquireEnd(ctx, pool, data)
	}
}

func (tt *testTracer) TraceReleaseStart(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceReleaseStartData) context.Context {
	if tt.traceReleaseStart != nil {
		return tt.traceReleaseStart(ctx, pool, data)
	}
	return ctx
}

func (tt *testTracer) TraceReleaseEnd(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceReleaseEndData) {
	if tt.traceReleaseEnd != nil {
		tt.traceReleaseEnd(ctx, pool, data)
	}
}

func (tt *testTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (tt *testTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
}

func TestTraceAcquire(t *testing.T) {
	t.Parallel()

	tracer := &testTracer{}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	config, err := pgxpool.ParseConfig(os.Getenv("PGX_TEST_DATABASE"))
	require.NoError(t, err)
	config.ConnConfig.Tracer = tracer

	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	defer pool.Close()

	traceAcquireStartCalled := false
	tracer.traceAcquireStart = func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireStartData) context.Context {
		traceAcquireStartCalled = true
		require.NotNil(t, pool)
		return context.WithValue(ctx, ctxKey("fromTraceAcquireStart"), "foo")
	}

	traceAcquireEndCalled := false
	tracer.traceAcquireEnd = func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
		traceAcquireEndCalled = true
		require.Equal(t, "foo", ctx.Value(ctxKey("fromTraceAcquireStart")))
		require.NotNil(t, pool)
		require.NotNil(t, data.Conn)
		require.NoError(t, data.Err)
	}

	c, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer c.Release()
	require.True(t, traceAcquireStartCalled)
	require.True(t, traceAcquireEndCalled)

	traceAcquireStartCalled = false
	traceAcquireEndCalled = false
	tracer.traceAcquireEnd = func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceAcquireEndData) {
		traceAcquireEndCalled = true
		require.NotNil(t, pool)
		require.Nil(t, data.Conn)
		require.Error(t, data.Err)
	}

	ctx, cancel = context.WithCancel(ctx)
	cancel()
	_, err = pool.Acquire(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, traceAcquireStartCalled)
	require.True(t, traceAcquireEndCalled)
}

func TestTraceRelease(t *testing.T) {
	t.Parallel()

	tracer := &testTracer{}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	config, err := pgxpool.ParseConfig(os.Getenv("PGX_TEST_DATABASE"))
	require.NoError(t, err)
	config.ConnConfig.Tracer = tracer

	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	defer pool.Close()

	traceReleaseStartCalled := false
	tracer.traceReleaseStart = func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceReleaseStartData) context.Context {
		traceReleaseStartCalled = true
		require.NotNil(t, pool)
		require.NotNil(t, data.Conn)
		return context.WithValue(ctx, ctxKey("fromTraceReleaseStart"), "foo")
	}

	traceReleaseEndCalled := false
	tracer.traceReleaseEnd = func(ctx context.Context, pool *pgxpool.Pool, data pgxpool.TraceReleaseEndData) {
		traceReleaseEndCalled = true
		require.Equal(t, "foo", ctx.Value(ctxKey("fromTraceReleaseStart")))
		require.NotNil(t, pool)
	}

	c, err := pool.Acquire(ctx)
	require.NoError(t, err)
	c.Release()
	require.True(t, traceReleaseStartCalled)
	require.True(t, traceReleaseEndCalled)
}
