package spannerclient

import (
	"context"
	"time"

	"cloud.google.com/go/spanner"
)

type SpannerClientMock struct {
	SingleMock       func() ReadOnlyTransaction
	DatabaseNameMock func() string
	RefreshMock      func(ctx context.Context, dbURI string) error
	ApplyMock        func(ctx context.Context, ms []*spanner.Mutation, opts ...spanner.ApplyOption) (commitTimestamp time.Time, err error)
}

func (scm SpannerClientMock) Refresh(ctx context.Context, dbURI string) error {
	return scm.RefreshMock(ctx, dbURI)
}

type ReadOnlyTransactionMock struct {
	QueryMock func(ctx context.Context, stmt spanner.Statement) RowIterator
}

type RowIteratorMock struct {
	NextMock func() (*spanner.Row, error)
	StopMock func()
}

func (scm SpannerClientMock) Single() ReadOnlyTransaction {
	return scm.SingleMock()
}

func (scm SpannerClientMock) DatabaseName() string {
	return scm.DatabaseNameMock()
}

func (scm SpannerClientMock) Apply(ctx context.Context, ms []*spanner.Mutation, opts ...spanner.ApplyOption) (commitTimestamp time.Time, err error) {
	return scm.ApplyMock(ctx, ms, opts...)
}

func (rom ReadOnlyTransactionMock) Query(ctx context.Context, stmt spanner.Statement) RowIterator {
	return rom.QueryMock(ctx, stmt)
}

func (rim RowIteratorMock) Next() (*spanner.Row, error) {
	return rim.NextMock()
}

func (rim RowIteratorMock) Stop() {
	rim.StopMock()
}
