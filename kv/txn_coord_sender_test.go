// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Spencer Kimball (spencer.kimball@gmail.com)

package kv

import (
	"bytes"
	"fmt"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/client"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/testutils"
	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/hlc"
	"github.com/cockroachdb/cockroach/util/leaktest"
	"github.com/cockroachdb/cockroach/util/stop"
)

// senderFn is a function that implements a Sender.
type senderFn func(context.Context, roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error)

// Send implements batch.Sender.
func (f senderFn) Send(ctx context.Context, ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
	return f(ctx, ba)
}

// teardownHeartbeats goes through the coordinator's active transactions and
// has the associated heartbeat tasks quit. This is useful for tests which
// don't finish transactions.
func teardownHeartbeats(tc *TxnCoordSender) {
	if r := recover(); r != nil {
		panic(r)
	}
	tc.Lock()
	for _, tm := range tc.txns {
		if tm.txnEnd != nil {
			close(tm.txnEnd)
		}
	}
	defer tc.Unlock()
}

// createTestDB creates a local test server and starts it. The caller
// is responsible for stopping the test server.
func createTestDB(t testing.TB) *LocalTestCluster {
	s := &LocalTestCluster{}
	s.Start(t)
	return s
}

// makeTS creates a new timestamp.
func makeTS(walltime int64, logical int32) roachpb.Timestamp {
	return roachpb.Timestamp{
		WallTime: walltime,
		Logical:  logical,
	}
}

// TestTxnCoordSenderAddRequest verifies adding a request creates a
// transaction metadata and adding multiple requests with same
// transaction ID updates the last update timestamp.
func TestTxnCoordSenderAddRequest(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()
	defer teardownHeartbeats(s.Sender)

	txn := client.NewTxn(*s.DB)

	// Put request will create a new transaction.
	if err := txn.Put(roachpb.Key("a"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	txnMeta, ok := s.Sender.txns[string(txn.Proto.ID)]
	if !ok {
		t.Fatal("expected a transaction to be created on coordinator")
	}
	if !txn.Proto.Writing {
		t.Fatal("txn is not marked as writing")
	}
	ts := atomic.LoadInt64(&txnMeta.lastUpdateNanos)

	// Advance time and send another put request. Lock the coordinator
	// to prevent a data race.
	s.Sender.Lock()
	s.Manual.Set(1)
	s.Sender.Unlock()
	if err := txn.Put(roachpb.Key("a"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	if len(s.Sender.txns) != 1 {
		t.Errorf("expected length of transactions map to be 1; got %d", len(s.Sender.txns))
	}
	txnMeta = s.Sender.txns[string(txn.Proto.ID)]
	if lu := atomic.LoadInt64(&txnMeta.lastUpdateNanos); ts >= lu || lu != s.Manual.UnixNano() {
		t.Errorf("expected last update time to advance; got %d", lu)
	}
}

// TestTxnCoordSenderBeginTransaction verifies that a command sent with a
// not-nil Txn with empty ID gets a new transaction initialized.
func TestTxnCoordSenderBeginTransaction(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()
	defer teardownHeartbeats(s.Sender)

	txn := client.NewTxn(*s.DB)

	// Put request will create a new transaction.
	key := roachpb.Key("key")
	txn.InternalSetPriority(10)
	txn.Proto.Isolation = roachpb.SNAPSHOT
	txn.Proto.Name = "test txn"
	if err := txn.Put(key, []byte("value")); err != nil {
		t.Fatal(err)
	}
	if txn.Proto.Name != "test txn" {
		t.Errorf("expected txn name to be %q; got %q", "test txn", txn.Proto.Name)
	}
	if txn.Proto.Priority != 10 {
		t.Errorf("expected txn priority 10; got %d", txn.Proto.Priority)
	}
	if !bytes.Equal(txn.Proto.Key, key) {
		t.Errorf("expected txn Key to match %q != %q", key, txn.Proto.Key)
	}
	if txn.Proto.Isolation != roachpb.SNAPSHOT {
		t.Errorf("expected txn isolation to be SNAPSHOT; got %s", txn.Proto.Isolation)
	}
}

// TestTxnCoordSenderBeginTransactionMinPriority verifies that when starting
// a new transaction, a non-zero priority is treated as a minimum value.
func TestTxnCoordSenderBeginTransactionMinPriority(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()
	defer teardownHeartbeats(s.Sender)

	txn := client.NewTxn(*s.DB)

	// Put request will create a new transaction.
	key := roachpb.Key("key")
	txn.InternalSetPriority(10)
	txn.Proto.Isolation = roachpb.SNAPSHOT
	txn.Proto.Priority = 11
	if err := txn.Put(key, []byte("value")); err != nil {
		t.Fatal(err)
	}
	if prio := txn.Proto.Priority; prio != 11 {
		t.Errorf("expected txn priority 11; got %d", prio)
	}
}

// TestTxnCoordSenderKeyRanges verifies that multiple requests to same or
// overlapping key ranges causes the coordinator to keep track only of
// the minimum number of ranges.
func TestTxnCoordSenderKeyRanges(t *testing.T) {
	defer leaktest.AfterTest(t)
	ranges := []struct {
		start, end roachpb.Key
	}{
		{roachpb.Key("a"), roachpb.Key(nil)},
		{roachpb.Key("a"), roachpb.Key(nil)},
		{roachpb.Key("aa"), roachpb.Key(nil)},
		{roachpb.Key("b"), roachpb.Key(nil)},
		{roachpb.Key("aa"), roachpb.Key("c")},
		{roachpb.Key("b"), roachpb.Key("c")},
	}

	s := createTestDB(t)
	defer s.Stop()
	defer teardownHeartbeats(s.Sender)

	txn := client.NewTxn(*s.DB)
	for _, rng := range ranges {
		if rng.end != nil {
			if err := txn.DelRange(rng.start, rng.end); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := txn.Put(rng.start, []byte("value")); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Verify that the transaction metadata contains only two entries
	// in its "keys" interval cache. "a" and range "aa"-"c".
	txnMeta, ok := s.Sender.txns[string(txn.Proto.ID)]
	if !ok {
		t.Fatalf("expected a transaction to be created on coordinator")
	}
	if txnMeta.keys.Len() != 2 {
		t.Errorf("expected 2 entries in keys interval cache; got %v", txnMeta.keys)
	}
}

// TestTxnCoordSenderMultipleTxns verifies correct operation with
// multiple outstanding transactions.
func TestTxnCoordSenderMultipleTxns(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()
	defer teardownHeartbeats(s.Sender)

	txn1 := client.NewTxn(*s.DB)
	txn2 := client.NewTxn(*s.DB)

	if err := txn1.Put(roachpb.Key("a"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := txn2.Put(roachpb.Key("b"), []byte("value")); err != nil {
		t.Fatal(err)
	}

	if len(s.Sender.txns) != 2 {
		t.Errorf("expected length of transactions map to be 2; got %d", len(s.Sender.txns))
	}
}

// TestTxnCoordSenderHeartbeat verifies periodic heartbeat of the
// transaction record.
func TestTxnCoordSenderHeartbeat(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()
	defer teardownHeartbeats(s.Sender)

	// Set heartbeat interval to 1ms for testing.
	s.Sender.heartbeatInterval = 1 * time.Millisecond

	initialTxn := client.NewTxn(*s.DB)
	if err := initialTxn.Put(roachpb.Key("a"), []byte("value")); err != nil {
		t.Fatal(err)
	}

	// Verify 3 heartbeats.
	var heartbeatTS roachpb.Timestamp
	for i := 0; i < 3; i++ {
		if err := util.IsTrueWithin(func() bool {
			ok, txn, err := getTxn(s.Sender, &initialTxn.Proto)
			if !ok || err != nil {
				return false
			}
			// Advance clock by 1ns.
			// Locking the TxnCoordSender to prevent a data race.
			s.Sender.Lock()
			s.Manual.Increment(1)
			s.Sender.Unlock()
			if heartbeatTS.Less(*txn.LastHeartbeat) {
				heartbeatTS = *txn.LastHeartbeat
				return true
			}
			return false
		}, 50*time.Millisecond); err != nil {
			t.Error("expected initial heartbeat within 50ms")
		}
	}
}

// getTxn fetches the requested key and returns the transaction info.
func getTxn(coord *TxnCoordSender, txn *roachpb.Transaction) (bool, *roachpb.Transaction, error) {
	hb := &roachpb.HeartbeatTxnRequest{
		Span: roachpb.Span{
			Key: txn.Key,
		},
	}
	reply, pErr := client.SendWrappedWith(coord, nil, roachpb.Header{
		Txn: txn,
	}, hb)
	if pErr != nil {
		return false, nil, pErr.GoError()
	}
	return true, reply.(*roachpb.HeartbeatTxnResponse).Txn, nil
}

func verifyCleanup(key roachpb.Key, coord *TxnCoordSender, eng engine.Engine, t *testing.T) {
	util.SucceedsWithin(t, 500*time.Millisecond, func() error {
		coord.Lock()
		l := len(coord.txns)
		coord.Unlock()
		if l != 0 {
			return fmt.Errorf("expected empty transactions map; got %d", l)
		}
		meta := &engine.MVCCMetadata{}
		ok, _, _, err := eng.GetProto(engine.MakeMVCCMetadataKey(key), meta)
		if err != nil {
			return fmt.Errorf("error getting MVCC metadata: %s", err)
		}
		if ok && meta.Txn != nil {
			return fmt.Errorf("found unexpected write intent: %s", meta)
		}
		return nil
	})
}

// TestTxnCoordSenderEndTxn verifies that ending a transaction
// sends resolve write intent requests and removes the transaction
// from the txns map.
func TestTxnCoordSenderEndTxn(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()

	// 4 cases: no deadline, past deadline, equal deadline, future deadline.
	for i := 0; i < 4; i++ {
		key := roachpb.Key("key: " + strconv.Itoa(i))
		txn := client.NewTxn(*s.DB)
		// Initialize the transaction
		if pErr := txn.Put(key, []byte("value")); pErr != nil {
			t.Fatal(pErr)
		}

		{
			var pErr *roachpb.Error
			switch i {
			case 0:
				// No deadline.
				pErr = txn.Commit()
			case 1:
				// Past deadline.
				pErr = txn.CommitBy(txn.Proto.Timestamp.Prev())
			case 2:
				// Equal deadline.
				pErr = txn.CommitBy(txn.Proto.Timestamp)
			case 3:
				// Future deadline.
				pErr = txn.CommitBy(txn.Proto.Timestamp.Next())
			}

			switch i {
			case 0:
				// No deadline.
				if pErr != nil {
					t.Error(pErr)
				}
			case 1:
				// Past deadline.
				if _, ok := pErr.GoError().(*roachpb.TransactionAbortedError); !ok {
					t.Errorf("expected TransactionAbortedError but got %T: %s", pErr, pErr)
				}
			case 2:
				// Equal deadline.
				if pErr != nil {
					t.Error(pErr)
				}
			case 3:
				// Future deadline.
				if pErr != nil {
					t.Error(pErr)
				}
			}
		}
		verifyCleanup(key, s.Sender, s.Eng, t)
	}
}

// TestTxnCoordSenderAddIntentOnError verifies that intents are tracked if
// the transaction is, even on error.
func TestTxnCoordSenderAddIntentOnError(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()

	// Create a transaction with intent at "a".
	key := roachpb.Key("x")
	txn := client.NewTxn(*s.DB)
	// Write so that the coordinator begins tracking this txn.
	if err := txn.Put("x", "y"); err != nil {
		t.Fatal(err)
	}
	pErr, ok := txn.CPut(key, []byte("x"), []byte("born to fail")).GoError().(*roachpb.ConditionFailedError)
	if !ok {
		t.Fatal(pErr)
	}
	s.Sender.Lock()
	intentSpans := s.Sender.txns[string(txn.Proto.ID)].intentSpans()
	expSpans := []roachpb.Span{{Key: key, EndKey: []byte("")}}
	equal := !reflect.DeepEqual(intentSpans, expSpans)
	s.Sender.Unlock()
	_ = txn.Rollback()
	if !equal {
		t.Fatalf("expected stored intents %v, got %v", expSpans, intentSpans)
	}
}

// TestTxnCoordSenderCleanupOnAborted verifies that if a txn receives a
// TransactionAbortedError, the coordinator cleans up the transaction.
func TestTxnCoordSenderCleanupOnAborted(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()

	// Create a transaction with intent at "a".
	key := roachpb.Key("a")
	txn1 := client.NewTxn(*s.DB)
	txn1.InternalSetPriority(1)
	if pErr := txn1.Put(key, []byte("value")); pErr != nil {
		t.Fatal(pErr)
	}

	// Push the transaction (by writing key "a" with higher priority) to abort it.
	txn2 := client.NewTxn(*s.DB)
	txn2.InternalSetPriority(2)
	if pErr := txn2.Put(key, []byte("value2")); pErr != nil {
		t.Fatal(pErr)
	}

	// Now end the transaction and verify we've cleanup up, even though
	// end transaction failed.
	pErr := txn1.Commit()
	switch pErr.GoError().(type) {
	case *roachpb.TransactionAbortedError:
		// Expected
	default:
		t.Fatalf("expected transaction aborted error; got %s", pErr)
	}
	if pErr := txn2.Commit(); pErr != nil {
		t.Fatal(pErr)
	}
	verifyCleanup(key, s.Sender, s.Eng, t)
}

// TestTxnCoordSenderGC verifies that the coordinator cleans up extant
// transactions after the lastUpdateNanos exceeds the timeout.
func TestTxnCoordSenderGC(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()

	// Set heartbeat interval to 1ms for testing.
	s.Sender.heartbeatInterval = 1 * time.Millisecond

	txn := client.NewTxn(*s.DB)
	if pErr := txn.Put(roachpb.Key("a"), []byte("value")); pErr != nil {
		t.Fatal(pErr)
	}

	// Now, advance clock past the default client timeout.
	// Locking the TxnCoordSender to prevent a data race.
	s.Sender.Lock()
	s.Manual.Set(defaultClientTimeout.Nanoseconds() + 1)
	s.Sender.Unlock()

	if err := util.IsTrueWithin(func() bool {
		// Locking the TxnCoordSender to prevent a data race.
		s.Sender.Lock()
		_, ok := s.Sender.txns[string(txn.Proto.ID)]
		s.Sender.Unlock()
		return !ok
	}, 50*time.Millisecond); err != nil {
		t.Error("expected garbage collection")
	}
}

// TestTxnCoordSenderTxnUpdatedOnError verifies that errors adjust the
// response transaction's timestamp and priority as appropriate.
func TestTxnCoordSenderTxnUpdatedOnError(t *testing.T) {
	defer leaktest.AfterTest(t)
	origTS := makeTS(123, 0)
	testCases := []struct {
		pErr             *roachpb.Error
		expEpoch         uint32
		expPri           int32
		expTS, expOrigTS roachpb.Timestamp
		nodeSeen         bool
	}{
		{
			// No error, so nothing interesting either.
			pErr:      nil,
			expEpoch:  0,
			expPri:    1,
			expTS:     origTS,
			expOrigTS: origTS,
		},
		{
			// On uncertainty error, new epoch begins and node is seen.
			// Timestamp moves ahead of the existing write.
			pErr: roachpb.NewError(&roachpb.ReadWithinUncertaintyIntervalError{
				NodeID:            1,
				ExistingTimestamp: origTS.Add(10, 10),
			}),
			expEpoch:  1,
			expPri:    1,
			expTS:     origTS.Add(10, 11),
			expOrigTS: origTS.Add(10, 11),
			nodeSeen:  true,
		},
		{
			// On abort, nothing changes but we get a new priority to use for
			// the next attempt.
			pErr: roachpb.NewError(&roachpb.TransactionAbortedError{
				Txn: roachpb.Transaction{
					Timestamp: origTS.Add(20, 10), Priority: 10,
				},
			}),
			expPri: 10,
		},
		{
			// On failed push, new epoch begins just past the pushed timestamp.
			// Additionally, priority ratchets up to just below the pusher's.
			pErr: roachpb.NewError(&roachpb.TransactionPushError{
				PusheeTxn: roachpb.Transaction{
					Timestamp: origTS.Add(10, 10),
					Priority:  int32(10)},
			}),
			expEpoch:  1,
			expPri:    9,
			expTS:     origTS.Add(10, 11),
			expOrigTS: origTS.Add(10, 11),
		},
		{
			// On retry, restart with new epoch, timestamp and priority.
			pErr: roachpb.NewError(&roachpb.TransactionRetryError{
				Txn: roachpb.Transaction{
					Timestamp: origTS.Add(10, 10), Priority: int32(10)},
			}),
			expEpoch:  1,
			expPri:    10,
			expTS:     origTS.Add(10, 10),
			expOrigTS: origTS.Add(10, 10),
		},
	}

	for i, test := range testCases {
		stopper := stop.NewStopper()

		manual := hlc.NewManualClock(origTS.WallTime)
		clock := hlc.NewClock(manual.UnixNano)
		clock.SetMaxOffset(20)

		ts := NewTxnCoordSender(senderFn(func(_ context.Context, ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
			var reply *roachpb.BatchResponse
			if test.pErr == nil {
				reply = ba.CreateReply()
			}
			return reply, test.pErr
		}), clock, false, nil, stopper)
		db := client.NewDB(ts)
		txn := client.NewTxn(*db)
		txn.InternalSetPriority(1)
		txn.Proto.Name = "test txn"
		key := roachpb.Key("test-key")
		_, pErr := txn.Get(key)
		teardownHeartbeats(ts)
		stopper.Stop()

		if reflect.TypeOf(test.pErr) != reflect.TypeOf(pErr) {
			t.Fatalf("%d: expected %T; got %T: %v", i, test.pErr, pErr, pErr)
		}
		if txn.Proto.Epoch != test.expEpoch {
			t.Errorf("%d: expected epoch = %d; got %d",
				i, test.expEpoch, txn.Proto.Epoch)
		}
		if txn.Proto.Priority != test.expPri {
			t.Errorf("%d: expected priority = %d; got %d",
				i, test.expPri, txn.Proto.Priority)
		}
		if !txn.Proto.Timestamp.Equal(test.expTS) {
			t.Errorf("%d: expected timestamp to be %s; got %s",
				i, test.expTS, txn.Proto.Timestamp)
		}
		if !txn.Proto.OrigTimestamp.Equal(test.expOrigTS) {
			t.Errorf("%d: expected orig timestamp to be %s + 1; got %s",
				i, test.expOrigTS, txn.Proto.OrigTimestamp)
		}
		if nodes := txn.Proto.CertainNodes.Nodes; (len(nodes) != 0) != test.nodeSeen {
			t.Errorf("%d: expected nodeSeen=%t, but list of hosts is %v",
				i, test.nodeSeen, nodes)
		}
	}
}

// TestTxnMultipleCoord checks that a coordinator uses the Writing flag to
// enforce that only one coordinator can be used for transactional writes.
func TestTxnMultipleCoord(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()

	for i, tc := range []struct {
		args    roachpb.Request
		writing bool
		ok      bool
	}{
		{roachpb.NewGet(roachpb.Key("a")), true, true},
		{roachpb.NewGet(roachpb.Key("a")), false, true},
		{roachpb.NewPut(roachpb.Key("a"), roachpb.Value{}), false, false}, // transactional write before begin
		{roachpb.NewPut(roachpb.Key("a"), roachpb.Value{}), true, false},  // must have switched coordinators
	} {
		txn := roachpb.NewTransaction("test", roachpb.Key("a"), 1, roachpb.SERIALIZABLE, s.Clock.Now(), s.Clock.MaxOffset().Nanoseconds())
		txn.Writing = tc.writing
		reply, pErr := client.SendWrappedWith(s.Sender, nil, roachpb.Header{
			Txn: txn,
		}, tc.args)
		if pErr == nil != tc.ok {
			t.Errorf("%d: %T (writing=%t): success_expected=%t, but got: %v",
				i, tc.args, tc.writing, tc.ok, pErr)
		}
		if pErr != nil {
			continue
		}

		txn = reply.Header().Txn
		// The transaction should come back rw if it started rw or if we just
		// wrote.
		isWrite := roachpb.IsTransactionWrite(tc.args)
		if (tc.writing || isWrite) != txn.Writing {
			t.Errorf("%d: unexpected writing state: %s", i, txn)
		}
		if !isWrite {
			continue
		}
		// Abort for clean shutdown.
		if _, pErr := client.SendWrappedWith(s.Sender, nil, roachpb.Header{
			Txn: txn,
		}, &roachpb.EndTransactionRequest{
			Commit: false,
		}); pErr != nil {
			t.Fatal(pErr)
		}
	}
}

// TestTxnCoordSenderSingleRoundtripTxn checks that a batch which completely
// holds the writing portion of a Txn (including EndTransaction) does not
// launch a heartbeat goroutine at all.
func TestTxnCoordSenderSingleRoundtripTxn(t *testing.T) {
	defer leaktest.AfterTest(t)
	stopper := stop.NewStopper()
	manual := hlc.NewManualClock(0)
	clock := hlc.NewClock(manual.UnixNano)
	clock.SetMaxOffset(20)

	ts := NewTxnCoordSender(senderFn(func(_ context.Context, ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		br := ba.CreateReply()
		br.Txn = ba.Txn.Clone()
		br.Txn.Writing = true
		return br, nil
	}), clock, false, nil, stopper)

	// Stop the stopper manually, prior to trying the transaction. This has the
	// effect of returning a NodeUnavailableError for any attempts at launching
	// a heartbeat goroutine.
	stopper.Stop()

	var ba roachpb.BatchRequest
	key := roachpb.Key("test")
	ba.Add(&roachpb.BeginTransactionRequest{Span: roachpb.Span{Key: key}})
	ba.Add(&roachpb.PutRequest{Span: roachpb.Span{Key: key}})
	ba.Add(&roachpb.EndTransactionRequest{})
	ba.Txn = &roachpb.Transaction{Name: "test"}
	_, pErr := ts.Send(context.Background(), ba)
	if pErr != nil {
		t.Fatal(pErr)
	}
}

// TestTxnCoordSenderErrorWithIntent validates that if a transactional request
// returns an error but also indicates a Writing transaction, the coordinator
// tracks it just like a successful request.
func TestTxnCoordSenderErrorWithIntent(t *testing.T) {
	defer leaktest.AfterTest(t)
	stopper := stop.NewStopper()
	manual := hlc.NewManualClock(0)
	clock := hlc.NewClock(manual.UnixNano)
	clock.SetMaxOffset(20)

	ts := NewTxnCoordSender(senderFn(func(_ context.Context, ba roachpb.BatchRequest) (*roachpb.BatchResponse, *roachpb.Error) {
		txn := ba.Txn.Clone()
		txn.Writing = true
		return nil, roachpb.NewError(roachpb.NewTransactionRetryError(txn))
	}), clock, false, nil, stopper)
	defer stopper.Stop()

	var ba roachpb.BatchRequest
	key := roachpb.Key("test")
	ba.Add(&roachpb.BeginTransactionRequest{Span: roachpb.Span{Key: key}})
	ba.Add(&roachpb.PutRequest{Span: roachpb.Span{Key: key}})
	ba.Add(&roachpb.EndTransactionRequest{})
	ba.Txn = &roachpb.Transaction{Name: "test"}
	if _, pErr := ts.Send(context.Background(), ba); !testutils.IsError(pErr.GoError(), "retry txn") {
		t.Fatalf("unexpected error: %v", pErr)
	}

	defer teardownHeartbeats(ts)
	ts.Lock()
	defer ts.Unlock()
	if len(ts.txns) != 1 {
		t.Fatalf("expected transaction to be tracked")
	}
}

// TestTxnCoordSenderReleaseTxnMeta verifies that TxnCoordSender releases the
// txnMetadata after the txn has committed succeed.
func TestTxnCoordSenderReleaseTxnMeta(t *testing.T) {
	defer leaktest.AfterTest(t)
	s := createTestDB(t)
	defer s.Stop()
	defer teardownHeartbeats(s.Sender)

	txn := client.NewTxn(*s.DB)
	ba := txn.NewBatch()
	ba.Put(roachpb.Key("a"), []byte("value"))
	ba.Put(roachpb.Key("b"), []byte("value"))
	if err := txn.CommitInBatch(ba); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.Sender.txns[string(txn.Proto.ID)]; ok {
		t.Fatal("expected TxnCoordSender has released the txn")
	}
}
