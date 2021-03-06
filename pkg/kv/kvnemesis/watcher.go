// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package kvnemesis

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/kv"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage/engine"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/span"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

// ClosedTimestampTargetInterval allows for setting the closed timestamp target
// interval.
type ClosedTimestampTargetInterval interface {
	Set(time.Duration) error
	ResetToDefault() error
}

// Watcher slurps all changes that happen to some span of kvs using RangeFeed.
type Watcher struct {
	ct ClosedTimestampTargetInterval
	mu struct {
		syncutil.Mutex
		kvs             *Engine
		frontierWaiters map[hlc.Timestamp][]chan error
	}
	cancel   func()
	g        ctxgroup.Group
	frontier *span.Frontier
}

// Watch starts a new Watcher over the given span of kvs. See Watcher.
func Watch(
	ctx context.Context, db *client.DB, ct ClosedTimestampTargetInterval, dataSpan roachpb.Span,
) (*Watcher, error) {
	w := &Watcher{
		ct:       ct,
		frontier: span.MakeFrontier(dataSpan),
	}
	w.mu.kvs = MakeEngine()
	w.mu.frontierWaiters = make(map[hlc.Timestamp][]chan error)
	ctx, w.cancel = context.WithCancel(ctx)
	w.g = ctxgroup.WithContext(ctx)

	sender := db.NonTransactionalSender()
	ds := sender.(*client.CrossRangeTxnWrapperSender).Wrapped().(*kv.DistSender)

	startTs := db.Clock().Now()
	eventC := make(chan *roachpb.RangeFeedEvent, 128)
	w.g.GoCtx(func(ctx context.Context) error {
		return ds.RangeFeed(ctx, dataSpan, startTs, true /* withDiff */, eventC)
	})
	w.g.GoCtx(func(ctx context.Context) error {
		return w.processEvents(ctx, eventC)
	})

	// Make sure the RangeFeed has started up, else we might lose some events.
	if err := w.WaitForFrontier(ctx, startTs); err != nil {
		_ = w.Finish()
		return nil, err
	}

	return w, nil
}

// Finish tears down the Watcher and returns all the kvs it has ingested. It may
// be called multiple times, though not concurrently.
func (w *Watcher) Finish() *Engine {
	if w.cancel == nil {
		// Finish was already called.
		return w.mu.kvs
	}
	w.cancel()
	w.cancel = nil
	// Only WaitForFrontier cares about errors.
	_ = w.g.Wait()
	return w.mu.kvs
}

// WaitForFrontier blocks until all kv changes <= the given timestamp are
// guaranteed to have been ingested.
func (w *Watcher) WaitForFrontier(ctx context.Context, ts hlc.Timestamp) (retErr error) {
	log.Infof(ctx, `watcher waiting for %s`, ts)
	if err := w.ct.Set(1 * time.Millisecond); err != nil {
		return err
	}
	defer func() {
		if err := w.ct.ResetToDefault(); err != nil {
			retErr = errors.WithSecondaryError(retErr, err)
		}
	}()
	resultCh := make(chan error, 1)
	w.mu.Lock()
	w.mu.frontierWaiters[ts] = append(w.mu.frontierWaiters[ts], resultCh)
	w.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-resultCh:
		return err
	}
}

func (w *Watcher) processEvents(ctx context.Context, eventC chan *roachpb.RangeFeedEvent) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event := <-eventC:
			switch e := event.GetValue().(type) {
			case *roachpb.RangeFeedError:
				return e.Error.GoError()
			case *roachpb.RangeFeedValue:
				log.Infof(ctx, `rangefeed Put %s %s -> %s (prev %s)`,
					e.Key, e.Value.Timestamp, e.Value.PrettyPrint(), e.PrevValue.PrettyPrint())
				w.mu.Lock()
				// TODO(dan): If the exact key+ts is put into kvs more than once, the
				// Engine will keep the last. This matches our txn semantics (if a key
				// is written in a transaction more than once, only the last is kept)
				// but it means that we'll won't catch it if we violate those semantics.
				// Consider first doing a Get and somehow failing if this exact key+ts
				// has previously been put with a different value.
				w.mu.kvs.Put(engine.MVCCKey{Key: e.Key, Timestamp: e.Value.Timestamp}, e.Value.RawBytes)
				prevTs := e.Value.Timestamp.Prev()
				prevValue := w.mu.kvs.Get(e.Key, prevTs)

				// RangeFeed doesn't send the timestamps of the previous values back
				// because changefeeds don't need them. It would likely be easy to
				// implement, but would add unnecessary allocations in changefeeds,
				// which don't need them. This means we'd want to make it an option in
				// the request, which seems silly to do for only this test.
				prevValue.Timestamp = hlc.Timestamp{}
				prevValueMismatch := !prevValue.Equal(e.PrevValue)
				var engineContents string
				if prevValueMismatch {
					engineContents = w.mu.kvs.DebugPrint("  ")
				}
				w.mu.Unlock()

				if prevValueMismatch {
					log.Infof(ctx, "rangefeed mismatch\n%s", engineContents)
					panic(errors.Errorf(
						`expected (%s, %s) previous value %s got: %s`, e.Key, prevTs, prevValue, e.PrevValue))
				}
			case *roachpb.RangeFeedCheckpoint:
				if w.frontier.Forward(e.Span, e.ResolvedTS) {
					frontier := w.frontier.Frontier()
					w.mu.Lock()
					log.Infof(ctx, `watcher reached frontier %s lagging by %s`,
						frontier, timeutil.Now().Sub(frontier.GoTime()))
					for ts, chs := range w.mu.frontierWaiters {
						if frontier.Less(ts) {
							continue
						}
						log.Infof(ctx, `watcher notifying %s`, ts)
						delete(w.mu.frontierWaiters, ts)
						for _, ch := range chs {
							ch <- nil
						}
					}
					w.mu.Unlock()
				}
			}
		}
	}
}
