// Copyright 2015 The Cockroach Authors.
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
// Author: Peter Mattis (peter@cockroachlabs.com)
// Author: Andrei Matei (andreimatei1@gmail.com)

package sql

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/net/context"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/config"
	"github.com/cockroachdb/cockroach/pkg/gossip"
	"github.com/cockroachdb/cockroach/pkg/internal/client"
	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/security"
	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/retry"
	"github.com/cockroachdb/cockroach/pkg/util/stop"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
)

// TODO(pmattis): Periodically renew leases for tables that were used recently and
// for which the lease will expire soon.

var (
	// LeaseDuration is the mean duration a lease will be acquired for. The
	// actual duration is jittered in the range
	// [0.75,1.25]*LeaseDuration. Exported for testing purposes only.
	LeaseDuration = 5 * time.Minute
	// MinLeaseDuration is the minimum duration a lease will have remaining upon
	// acquisition. Exported for testing purposes only.
	MinLeaseDuration = time.Minute
)

type leaseState struct {
	id         sqlbase.ID
	version    sqlbase.DescriptorVersion
	expiration parser.DTimestamp
	// TODO(vivek): Remove this once TestTxnObeysLeaseExpiration is no longer
	// needed, or when it's rewritten.
	testingKnobs LeaseStoreTestingKnobs
}

// tableVersionState holds the state for a table version. This includes
// the lease information for a table version.
// TODO(vivek): A node only needs to manage lease information on what it
// thinks is the latest version for a table descriptor.
type tableVersionState struct {
	// This descriptor is immutable and can be shared by many goroutines.
	// Care must be taken to not modify it.
	sqlbase.TableDescriptor

	// mu protects refcount and invalid.
	mu       syncutil.Mutex
	refcount int
	// Set if the lease has been released and cannot be handed out any more.
	// The table name cache can have references to such tables because we
	// don't atomically releasing a lease and update the cache.
	invalid bool

	// A lease for the table version if needed. Normally a table descriptor is associated
	// with a lease. For a table descriptor at version "v" its expiration time is the
	// is the ModificationTime of the descriptor at version = v + 2. The expiration time
	// for the table is finalized as soon as version = v + 2 is written, henceforth not
	// requiring a lease at version = v.
	lease *leaseState
}

func (s *tableVersionState) String() string {
	return fmt.Sprintf("%d(%q) ver=%d:%d, refcount=%d", s.ID, s.Name, s.Version, s.lease.expiration.UnixNano(), s.refcount)
}

// Expiration returns the expiration time of the table version.
func (s *tableVersionState) Expiration() time.Time {
	return s.lease.expiration.Time
}

// hasSomeLifeLeft returns true if the lease has at least a minimum of
// lifetime left until expiration, and thus can be used.
func (s *tableVersionState) hasSomeLifeLeft(clock *hlc.Clock) bool {
	if s.lease.testingKnobs.CanUseExpiredLeases {
		return true
	}
	minDesiredExpiration := clock.Now().GoTime().Add(MinLeaseDuration)
	return s.lease.expiration.After(minDesiredExpiration)
}

func (s *tableVersionState) incRefcount() {
	s.mu.Lock()
	s.incRefcountLocked()
	s.mu.Unlock()
}

func (s *tableVersionState) incRefcountLocked() {
	if s.invalid {
		panic(fmt.Sprintf("trying to incRefcount on released lease: %+v", s))
	}
	s.refcount++
	log.VEventf(context.TODO(), 2, "tableVersionState.incRef: %s", s)
}

func (s *tableVersionState) expirationToHLC() hlc.Timestamp {
	return hlc.Timestamp{WallTime: s.Expiration().UnixNano()}
}

// LeaseStore implements the operations for acquiring and releasing leases and
// publishing a new version of a descriptor. Exported only for testing.
type LeaseStore struct {
	db     client.DB
	clock  *hlc.Clock
	nodeID *base.NodeIDContainer

	testingKnobs LeaseStoreTestingKnobs
	memMetrics   *MemoryMetrics
}

// jitteredLeaseDuration returns a randomly jittered duration from the interval
// [0.75 * leaseDuration, 1.25 * leaseDuration].
func jitteredLeaseDuration() time.Duration {
	return time.Duration(float64(LeaseDuration) * (0.75 + 0.5*rand.Float64()))
}

// acquire a lease on the most recent version of a table descriptor.
// If the lease cannot be obtained because the descriptor is in the process of
// being dropped, the error will be errTableDropped.
func (s LeaseStore) acquire(
	ctx context.Context,
	txn *client.Txn,
	tableID sqlbase.ID,
	minVersion sqlbase.DescriptorVersion,
	minExpirationTime parser.DTimestamp,
) (*tableVersionState, error) {
	table := &tableVersionState{lease: &leaseState{testingKnobs: s.testingKnobs}}
	expiration := time.Unix(0, s.clock.Now().WallTime).Add(jitteredLeaseDuration())
	expiration = expiration.Round(time.Microsecond)
	if !minExpirationTime.IsZero() && expiration.Before(minExpirationTime.Time) {
		expiration = minExpirationTime.Time
	}
	table.lease.expiration = parser.DTimestamp{Time: expiration}

	// Use the supplied (user) transaction to look up the descriptor because the
	// descriptor might have been created within the transaction.
	tableDesc, err := sqlbase.GetTableDescFromID(ctx, txn, tableID)
	if err != nil {
		return nil, err
	}
	if err := filterTableState(tableDesc); err != nil {
		return nil, err
	}
	tableDesc.MaybeUpgradeFormatVersion()
	// Once the descriptor is set it is immutable and care must be taken
	// to not modify it.
	table.TableDescriptor = *tableDesc
	table.lease.id = table.ID
	table.lease.version = table.Version

	// ValidateTable instead of Validate, even though we have a txn available,
	// so we don't block reads waiting for this table version.
	if err := table.ValidateTable(); err != nil {
		return nil, err
	}
	if table.Version < minVersion {
		return nil, errors.Errorf("version %d of table %d does not exist yet", minVersion, tableID)
	}

	// Insert the entry in the lease table in a separate transaction. This is
	// necessary because we want to ensure that the lease entry is added and the
	// transaction passed to acquire() might be aborted. The lease entry needs to
	// be added because we store the returned tableVersionState in local in-memory maps
	// and cannot handle the entry being reverted. This is safe because either
	// the descriptor we're acquiring the lease on existed prior to the acquire
	// transaction in which case acquiring the lease is kosher, or the descriptor
	// was created within the acquire transaction. The second case is more
	// subtle. We might create a lease entry for a table that doesn't exist, but
	// there is no harm in that as no other transaction will be attempting to
	// modify the descriptor and even if the descriptor is never created we'll
	// just have a dangling lease entry which will eventually get GC'd.
	err = s.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
		nodeID := s.nodeID.Get()
		if nodeID == 0 {
			panic("zero nodeID")
		}
		p := makeInternalPlanner("lease-insert", txn, security.RootUser, s.memMetrics)
		defer finishInternalPlanner(p)
		const insertLease = `INSERT INTO system.lease (descID, version, nodeID, expiration) ` +
			`VALUES ($1, $2, $3, $4)`
		lease := table.lease
		count, err := p.exec(
			ctx, insertLease, lease.id, int(lease.version), nodeID, &lease.expiration,
		)
		if err != nil {
			return err
		}
		if count != 1 {
			return errors.Errorf("%s: expected 1 result, found %d", insertLease, count)
		}
		return nil
	})
	return table, err
}

// Release a previously acquired table descriptor.
func (s LeaseStore) release(ctx context.Context, stopper *stop.Stopper, table *tableVersionState) {
	retryOptions := base.DefaultRetryOptions()
	retryOptions.Closer = stopper.ShouldQuiesce()
	firstAttempt := true
	for r := retry.Start(retryOptions); r.Next(); {
		// This transaction is idempotent.
		err := s.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
			log.VEventf(ctx, 2, "LeaseStore releasing lease %s", table)
			nodeID := s.nodeID.Get()
			if nodeID == 0 {
				panic("zero nodeID")
			}
			p := makeInternalPlanner("lease-release", txn, security.RootUser, s.memMetrics)
			defer finishInternalPlanner(p)
			const deleteLease = `DELETE FROM system.lease ` +
				`WHERE (descID, version, nodeID, expiration) = ($1, $2, $3, $4)`
			lease := table.lease
			count, err := p.exec(
				ctx, deleteLease, lease.id, int(lease.version), nodeID, &lease.expiration)
			if err != nil {
				return err
			}
			// We allow count == 0 after the first attempt.
			if count > 1 || (count == 0 && firstAttempt) {
				log.Warningf(ctx, "unexpected results while deleting lease %s: "+
					"expected 1 result, found %d", table, count)
			}
			return nil
		})
		if s.testingKnobs.LeaseReleasedEvent != nil {
			s.testingKnobs.LeaseReleasedEvent(table.TableDescriptor, err)
		}
		if err == nil {
			break
		}
		log.Warningf(ctx, "error releasing lease %q: %s", table, err)
		firstAttempt = false
	}
}

// WaitForOneVersion returns once there are no unexpired leases on the
// previous version of the table descriptor. It returns the current version.
// After returning there can only be versions of the descriptor >= to the
// returned version. Lease acquisition (see acquire()) maintains the
// invariant that no new leases for desc.Version-1 will be granted once
// desc.Version exists.
func (s LeaseStore) WaitForOneVersion(
	ctx context.Context, tableID sqlbase.ID, retryOpts retry.Options,
) (sqlbase.DescriptorVersion, error) {
	desc := &sqlbase.Descriptor{}
	descKey := sqlbase.MakeDescMetadataKey(tableID)
	var tableDesc *sqlbase.TableDescriptor
	for r := retry.Start(retryOpts); r.Next(); {
		// Get the current version of the table descriptor non-transactionally.
		//
		// TODO(pmattis): Do an inconsistent read here?
		if err := s.db.GetProto(context.TODO(), descKey, desc); err != nil {
			return 0, err
		}
		tableDesc = desc.GetTable()
		if tableDesc == nil {
			return 0, errors.Errorf("ID %d is not a table", tableID)
		}
		// Check to see if there are any leases that still exist on the previous
		// version of the descriptor.
		now := s.clock.Now()
		count, err := s.countLeases(ctx, tableDesc.ID, tableDesc.Version-1, now.GoTime())
		if err != nil {
			return 0, err
		}
		if count == 0 {
			break
		}
		log.Infof(context.TODO(), "publish (count leases): descID=%d name=%s version=%d count=%d",
			tableDesc.ID, tableDesc.Name, tableDesc.Version-1, count)
	}
	return tableDesc.Version, nil
}

var errDidntUpdateDescriptor = errors.New("didn't update the table descriptor")

// Publish updates a table descriptor. It also maintains the invariant that
// there are at most two versions of the descriptor out in the wild at any time
// by first waiting for all nodes to be on the current (pre-update) version of
// the table desc.
// The update closure is called after the wait, and it provides the new version
// of the descriptor to be written. In a multi-step schema operation, this
// update should perform a single step.
// The closure may be called multiple times if retries occur; make sure it does
// not have side effects.
// Returns the updated version of the descriptor.
func (s LeaseStore) Publish(
	ctx context.Context,
	tableID sqlbase.ID,
	update func(*sqlbase.TableDescriptor) error,
	logEvent func(*client.Txn) error,
) (*sqlbase.Descriptor, error) {
	errLeaseVersionChanged := errors.New("lease version changed")
	// Retry while getting errLeaseVersionChanged.
	for r := retry.Start(base.DefaultRetryOptions()); r.Next(); {
		// Wait until there are no unexpired leases on the previous version
		// of the table.
		expectedVersion, err := s.WaitForOneVersion(ctx, tableID, base.DefaultRetryOptions())
		if err != nil {
			return nil, err
		}

		desc := &sqlbase.Descriptor{}
		// There should be only one version of the descriptor, but it's
		// a race now to update to the next version.
		err = s.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
			descKey := sqlbase.MakeDescMetadataKey(tableID)

			// Re-read the current version of the table descriptor, this time
			// transactionally.
			if err := txn.GetProto(ctx, descKey, desc); err != nil {
				return err
			}
			tableDesc := desc.GetTable()
			if tableDesc == nil {
				return errors.Errorf("ID %d is not a table", tableID)
			}
			if expectedVersion != tableDesc.Version {
				// The version changed out from under us. Someone else must be
				// performing a schema change operation.
				if log.V(3) {
					log.Infof(ctx, "publish (version changed): %d != %d", expectedVersion, tableDesc.Version)
				}
				return errLeaseVersionChanged
			}

			// Run the update closure.
			version := tableDesc.Version
			if err := update(tableDesc); err != nil {
				return err
			}
			if version != tableDesc.Version {
				return errors.Errorf("updated version to: %d, expected: %d",
					tableDesc.Version, version)
			}

			tableDesc.Version++
			now := s.clock.Now()
			tableDesc.ModificationTime = now
			log.Infof(ctx, "publish: descID=%d (%s) version=%d mtime=%s",
				tableDesc.ID, tableDesc.Name, tableDesc.Version, now.GoTime())
			if err := tableDesc.ValidateTable(); err != nil {
				return err
			}

			// Write the updated descriptor.
			if err := txn.SetSystemConfigTrigger(); err != nil {
				return err
			}
			b := txn.NewBatch()
			b.Put(descKey, desc)
			if logEvent != nil {
				// If an event log is required for this update, ensure that the
				// descriptor change occurs first in the transaction. This is
				// necessary to ensure that the System configuration change is
				// gossiped. See the documentation for
				// transaction.SetSystemConfigTrigger() for more information.
				if err := txn.Run(ctx, b); err != nil {
					return err
				}
				if err := logEvent(txn); err != nil {
					return err
				}
				return txn.Commit(ctx)
			}
			// More efficient batching can be used if no event log message
			// is required.
			return txn.CommitInBatch(ctx, b)
		})

		switch err {
		case nil, errDidntUpdateDescriptor:
			return desc, nil
		case errLeaseVersionChanged:
			// will loop around to retry
		default:
			return nil, err
		}
	}

	panic("not reached")
}

// countLeases returns the number of unexpired leases for a particular version
// of a descriptor.
func (s LeaseStore) countLeases(
	ctx context.Context, descID sqlbase.ID, version sqlbase.DescriptorVersion, expiration time.Time,
) (int, error) {
	var count int
	err := s.db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
		p := makeInternalPlanner("leases-count", txn, security.RootUser, s.memMetrics)
		defer finishInternalPlanner(p)
		const countLeases = `SELECT COUNT(version) FROM system.lease ` +
			`WHERE descID = $1 AND version = $2 AND expiration > $3`
		values, err := p.QueryRow(ctx, countLeases, descID, int(version), expiration)
		if err != nil {
			return err
		}
		count = int(parser.MustBeDInt(values[0]))
		return nil
	})
	return count, err
}

// tableSet maintains an ordered set of tableVersionState objects. It supports
// addition and removal of elements, finding the table for a particular
// version, or finding the most recent table version.
type tableSet struct {
	// The lease state data is stored in a sorted slice ordered by <version,
	// expiration>. Ordering is maintained by insert and remove.
	data []*tableVersionState
}

func (l *tableSet) String() string {
	var buf bytes.Buffer
	for i, s := range l.data {
		if i > 0 {
			buf.WriteString(" ")
		}
		buf.WriteString(fmt.Sprintf("%d:%d", s.Version, s.Expiration().UnixNano()))
	}
	return buf.String()
}

func (l *tableSet) insert(s *tableVersionState) {
	i, match := l.findIndex(s.Version)
	if match {
		panic("unable to insert duplicate lease")
	}
	if i == len(l.data) {
		l.data = append(l.data, s)
		return
	}
	l.data = append(l.data, nil)
	copy(l.data[i+1:], l.data[i:])
	l.data[i] = s
}

func (l *tableSet) remove(s *tableVersionState) {
	i, match := l.findIndex(s.Version)
	if !match {
		panic(fmt.Sprintf("can't find lease to remove: %s", s))
	}
	l.data = append(l.data[:i], l.data[i+1:]...)
}

func (l *tableSet) find(version sqlbase.DescriptorVersion) *tableVersionState {
	if i, match := l.findIndex(version); match {
		return l.data[i]
	}
	return nil
}

func (l *tableSet) findIndex(version sqlbase.DescriptorVersion) (int, bool) {
	i := sort.Search(len(l.data), func(i int) bool {
		s := l.data[i]
		return s.Version >= version
	})
	if i < len(l.data) {
		s := l.data[i]
		if s.Version == version {
			return i, true
		}
	}
	return i, false
}

func (l *tableSet) findNewest(version sqlbase.DescriptorVersion) *tableVersionState {
	if len(l.data) == 0 {
		return nil
	}
	if version == 0 {
		// No explicitly version, return the newest lease of the latest version.
		return l.data[len(l.data)-1]
	}
	// Find the index of the first lease with version > targetVersion.
	i := sort.Search(len(l.data), func(i int) bool {
		return l.data[i].Version > version
	})
	if i == 0 {
		return nil
	}
	// i-1 is the index of the newest lease for the previous version (the version
	// we're looking for).
	s := l.data[i-1]
	if s.Version == version {
		return s
	}
	return nil
}

type tableState struct {
	id sqlbase.ID
	// The cache is updated every time we acquire or release a table.
	tableNameCache *tableNameCache
	stopper        *stop.Stopper
	// Protects both active and acquiring.
	mu syncutil.Mutex
	// The active leases for the table: sorted by their version and expiration
	// time. There may be more than one active lease when the system is
	// transitioning from one version of the descriptor to another or when the
	// node preemptively acquires a new lease for a version when the old lease
	// has not yet expired.
	active tableSet
	// A channel used to indicate whether a lease is actively being acquired.
	// nil if there is no lease acquisition in progress for the table. If
	// non-nil, the channel will be closed when lease acquisition completes.
	acquiring chan struct{}
	// Indicates that the table has been dropped, or is being dropped.
	// If set, leases are released from the store as soon as their refcount drops
	// to 0, as opposed to waiting until they expire.
	dropped bool
}

// acquire returns a lease at the specified version. The lease will have its
// refcount incremented, so the caller is responsible to call release() on it.
func (t *tableState) acquire(
	ctx context.Context, txn *client.Txn, version sqlbase.DescriptorVersion, m *LeaseManager,
) (*tableVersionState, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for {
		s := t.active.findNewest(version)
		if s != nil {
			if checkedTable := t.checkTable(s, version, m.clock); checkedTable != nil {
				return checkedTable, nil
			}
		} else if version != 0 {
			n := t.active.findNewest(0)
			if n != nil && version < n.Version {
				return nil, errors.Errorf("table %d unable to acquire lease on old version: %d < %d",
					t.id, version, n.Version)
			}
		}

		if err := t.acquireFromStoreLocked(ctx, txn, version, m); err != nil {
			return nil, err
		}
		// A new lease was added, so loop and perform the lookup again.
	}
}

// checkLease checks whether lease is eligible to be returned to a client which
// requested a lease at a specified version (version can also be 0).
// Returns the lease after having incremented its refcount if it's OK to give it
// to the client. Returns nil otherwise.
//
// t.mu needs to be locked
func (t *tableState) checkTable(
	table *tableVersionState, version sqlbase.DescriptorVersion, clock *hlc.Clock,
) *tableVersionState {
	// If a lease was requested for an old version of the descriptor,
	// return it even if there is only a short time left before it
	// expires, or even if it's expired. We can't renew this lease as doing so
	// would violate the invariant that we only get leases on the newest
	// version. The transaction will either finish before the lease expires or
	// it will abort, which is what will happen if we returned an error here.
	skipLifeCheck := version != 0 && table != t.active.findNewest(0)
	if !skipLifeCheck && !table.hasSomeLifeLeft(clock) {
		return nil
	}
	table.incRefcount()
	return table
}

// acquireFromStoreLocked acquires a new lease from the store and inserts it
// into the active set. t.mu must be locked.
func (t *tableState) acquireFromStoreLocked(
	ctx context.Context, txn *client.Txn, version sqlbase.DescriptorVersion, m *LeaseManager,
) error {
	// Ensure there is no lease acquisition in progress.
	if t.acquireWait() {
		// There was a lease acquisition in progress; accept the lease just
		// acquired.
		return nil
	}

	event := m.testingKnobs.LeaseStoreTestingKnobs.LeaseAcquiringEvent
	if event != nil {
		event(t.id, txn)
	}
	s, err := t.acquireNodeLease(ctx, txn, version, m, parser.DTimestamp{})
	if err != nil {
		return err
	}
	t.upsertLocked(ctx, s, m)
	return nil
}

// acquireFreshestFromStoreLocked acquires a new lease from the store and
// inserts it into the active set. It guarantees that the lease returned is
// the one acquired after the call is made. Use this if the lease we want to
// get needs to see some descriptor updates that we know happened recently
// (but that didn't cause the version to be incremented). E.g. if we suspect
// there's a new name for a table, the caller can insist on getting a lease
// reflecting this new name. Moreover, upon returning, the new lease is
// guaranteed to be the last lease in t.active (note that this is not
// generally guaranteed, as leases are assigned random expiration times).
//
// t.mu must be locked.
func (t *tableState) acquireFreshestFromStoreLocked(
	ctx context.Context, txn *client.Txn, version sqlbase.DescriptorVersion, m *LeaseManager,
) error {
	// Ensure there is no lease acquisition in progress.
	t.acquireWait()

	// Move forward to acquire a fresh table lease.

	// Set the min expiration time to guarantee that the lease acquired is the
	// last lease in t.active .
	minExpirationTime := parser.DTimestamp{}
	newestTable := t.active.findNewest(0)
	if newestTable != nil {
		minExpirationTime = parser.DTimestamp{
			Time: newestTable.lease.expiration.Add(time.Millisecond)}
	}

	s, err := t.acquireNodeLease(ctx, txn, version, m, minExpirationTime)
	if err != nil {
		return err
	}
	t.upsertLocked(ctx, s, m)
	return nil
}

// upsertLocked inserts a lease for a particular table version.
// If an existing lease exists for the table version, it releases
// the older lease and replaces it.
func (t *tableState) upsertLocked(ctx context.Context, table *tableVersionState, m *LeaseManager) {
	s := t.active.find(table.Version)
	if s == nil {
		t.active.insert(table)
		return
	}

	s.mu.Lock()
	table.mu.Lock()
	// subsume the refcount of the older lease.
	table.refcount += s.refcount
	s.refcount = 0
	s.invalid = true
	table.mu.Unlock()
	s.mu.Unlock()
	log.VEventf(ctx, 2, "replaced lease: %s with %s", s, table)
	t.removeTable(s, m)
	t.active.insert(table)
}

// releaseInactiveLeases releases the leases in t.active.data with refcount 0.
// t.mu must be locked.
func (t *tableState) releaseInactiveLeases(m *LeaseManager) {
	// A copy of t.active.data must be made since t.active.data will be changed
	// by `removeLease`.
	for _, table := range append([]*tableVersionState(nil), t.active.data...) {
		func() {
			table.mu.Lock()
			defer table.mu.Unlock()
			if table.refcount == 0 {
				t.removeTable(table, m)
			}
		}()
	}
}

// acquireWait waits until no lease acquisition is in progress. It returns
// true if it needed to wait.
func (t *tableState) acquireWait() bool {
	wait := t.acquiring != nil
	// Spin until no lease acquisition is in progress.
	for t.acquiring != nil {
		// We're called with mu locked, but need to unlock it while we wait
		// for the in-progress lease acquisition to finish.
		acquiring := t.acquiring
		t.mu.Unlock()
		<-acquiring
		t.mu.Lock()
	}
	return wait
}

// If the lease cannot be obtained because the descriptor is in the process of
// being dropped, the error will be errTableDropped.
// minExpirationTime, if not set to the zero value, will be used as a lower
// bound on the expiration of the new table. This can be used to eliminate the
// jitter in the expiration time, and guarantee that we get a lease that will be
// inserted at the end of the lease set (i.e. it will be returned by
// findNewest() from now on).
//
// t.mu needs to be locked.
func (t *tableState) acquireNodeLease(
	ctx context.Context,
	txn *client.Txn,
	minVersion sqlbase.DescriptorVersion,
	m *LeaseManager,
	minExpirationTime parser.DTimestamp,
) (*tableVersionState, error) {
	if m.isDraining() {
		return nil, errors.New("cannot acquire lease when draining")
	}

	// Notify when lease has been acquired.
	t.acquiring = make(chan struct{})
	defer func() {
		close(t.acquiring)
		t.acquiring = nil
	}()
	// We're called with mu locked, but need to unlock it during lease
	// acquisition.
	t.mu.Unlock()
	defer t.mu.Lock()
	table, err := m.LeaseStore.acquire(ctx, txn, t.id, minVersion, minExpirationTime)
	if err != nil {
		return nil, err
	}
	t.tableNameCache.insert(table)
	return table, nil
}

func (t *tableState) release(table sqlbase.TableDescriptor, m *LeaseManager) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.active.find(table.Version)
	if s == nil {
		return errors.Errorf("table %d version %d not found", table.ID, table.Version)
	}
	// Decrements the refcount and returns true if the lease has to be removed
	// from the store.
	decRefcount := func(s *tableVersionState) bool {
		// Figure out if we'd like to remove the lease from the store asap (i.e.
		// when the refcount drops to 0). If so, we'll need to mark the lease as
		// invalid.
		removeOnceDereferenced := m.LeaseStore.testingKnobs.RemoveOnceDereferenced ||
			// Release from the store if the table has been dropped; no leases
			// can be acquired any more.
			t.dropped ||
			// Release from the store if the LeaseManager is draining.
			m.isDraining() ||
			// Release from the store if the lease is not for the latest
			// version; only leases for the latest version can be acquired.
			s != t.active.findNewest(0)

		s.mu.Lock()
		defer s.mu.Unlock()
		s.refcount--
		log.VEventf(context.TODO(), 2, "release: %s", s)
		if s.refcount < 0 {
			panic(fmt.Sprintf("negative ref count: %s", s))
		}
		if s.refcount == 0 && removeOnceDereferenced {
			s.invalid = true
		}
		return s.invalid
	}
	if decRefcount(s) {
		t.removeTable(s, m)
	}
	return nil
}

// t.mu needs to be locked.
func (t *tableState) removeTable(table *tableVersionState, m *LeaseManager) {
	t.active.remove(table)
	t.tableNameCache.remove(table)

	ctx := context.TODO()
	if m.isDraining() {
		// Release synchronously to guarantee release before exiting.
		m.LeaseStore.release(ctx, t.stopper, table)
		return
	}

	// Release to the store asynchronously, without the tableState lock.
	if err := t.stopper.RunAsyncTask(
		ctx, "sql.tableState: releasing descriptor lease",
		func(ctx context.Context) {
			m.LeaseStore.release(ctx, t.stopper, table)
		}); err != nil {
		log.Warningf(ctx, "error: %s, not releasing lease: %q", err, table)
	}
}

// purgeOldLeases refreshes the leases on a table. Unused leases older than
// minVersion will be released.
// If dropped is set, minVersion is ignored; no lease is acquired and all
// existing unused leases are released. The table is further marked dropped,
// which will cause existing in-use leases to be eagerly released once
// they're not in use any more.
// If t has no active leases, nothing is done.
func (t *tableState) purgeOldLeases(
	ctx context.Context,
	db *client.DB,
	dropped bool,
	minVersion sqlbase.DescriptorVersion,
	m *LeaseManager,
) error {
	t.mu.Lock()
	empty := len(t.active.data) == 0
	t.mu.Unlock()
	if empty {
		// We don't currently have a lease on this table, so no need to refresh
		// anything.
		return nil
	}

	releaseInactives := func(drop bool) {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.dropped = drop
		t.releaseInactiveLeases(m)
	}

	if dropped {
		releaseInactives(dropped)
		return nil
	}

	// Acquire a lease on the table at a version >= minVersion
	// to maintain an active lease on the latest version, so that it
	// doesn't get released when releaseInactive() is called below.
	var table *tableVersionState
	err := db.Txn(ctx, func(ctx context.Context, txn *client.Txn) error {
		var err error
		table, err = t.acquire(ctx, txn, minVersion, m)
		return err
	})
	if dropped := err == errTableDropped; dropped || err == nil {
		releaseInactives(dropped)
		if table != nil {
			return t.release(table.TableDescriptor, m)
		}
		return nil
	}
	return err
}

// LeaseStoreTestingKnobs contains testing knobs.
type LeaseStoreTestingKnobs struct {
	// Called after a lease is removed from the store, with any operation error.
	// See LeaseRemovalTracker.
	LeaseReleasedEvent func(table sqlbase.TableDescriptor, err error)
	// Called just before a lease is about to be acquired by the store. Gives
	// access to the txn doing the acquiring.
	LeaseAcquiringEvent func(tableID sqlbase.ID, txn *client.Txn)
	// Called after a lease is acquired, with any operation error.
	LeaseAcquiredEvent func(table sqlbase.TableDescriptor, err error)
	// Allow the use of expired leases.
	CanUseExpiredLeases bool
	// RemoveOnceDereferenced forces leases to be removed
	// as soon as they are dereferenced.
	RemoveOnceDereferenced bool
}

// ModuleTestingKnobs is part of the base.ModuleTestingKnobs interface.
func (*LeaseStoreTestingKnobs) ModuleTestingKnobs() {}

var _ base.ModuleTestingKnobs = &LeaseStoreTestingKnobs{}

// LeaseManagerTestingKnobs contains test knobs.
type LeaseManagerTestingKnobs struct {
	// A callback called when a gossip update is received, before the leases are
	// refreshed. Careful when using this to block for too long - you can block
	// all the gossip users in the system.
	GossipUpdateEvent func(config.SystemConfig)
	// A callback called after the leases are refreshed as a result of a gossip update.
	TestingLeasesRefreshedEvent func(config.SystemConfig)

	LeaseStoreTestingKnobs LeaseStoreTestingKnobs
}

var _ base.ModuleTestingKnobs = &LeaseManagerTestingKnobs{}

// ModuleTestingKnobs is part of the base.ModuleTestingKnobs interface.
func (*LeaseManagerTestingKnobs) ModuleTestingKnobs() {}

type tableNameCacheKey struct {
	dbID                sqlbase.ID
	normalizeTabledName string
}

// tableNameCache represents a cache of table name -> lease mappings.
// The LeaseManager updates the cache every time a lease is acquired or released
// from the store. The cache maintains the newest lease for each table name.
// All methods are thread-safe.
type tableNameCache struct {
	mu     syncutil.Mutex
	tables map[tableNameCacheKey]*tableVersionState
}

// Resolves a (database ID, table name) to the table descriptor's ID. Returns
// a valid lease for the table with that name, if the name had been previously
// cached and the cache has a lease with at least some amount of life
// left in it. Returns nil otherwise.
// This method handles normalizing the table name.
// The lease's refcount is incremented before returning, so the caller is
// responsible for releasing it to the leaseManager.
func (c *tableNameCache) get(
	dbID sqlbase.ID, tableName string, clock *hlc.Clock,
) *tableVersionState {
	c.mu.Lock()
	table, ok := c.tables[makeTableNameCacheKey(dbID, tableName)]
	c.mu.Unlock()
	if !ok {
		return nil
	}
	table.mu.Lock()
	defer table.mu.Unlock()
	if !nameMatchesTable(table.TableDescriptor, dbID, tableName) {
		panic(fmt.Sprintf("Out of sync entry in the name cache. "+
			"Cache entry: %d.%q -> %d. Lease: %d.%q.",
			dbID, tableName, table.ID, table.ParentID, table.Name))
	}

	if !table.hasSomeLifeLeft(clock) {
		// Expired, or almost expired, table. Don't hand it out.
		return nil
	}
	if table.invalid {
		// This get() raced with a release operation. The leaseManager should remove
		// this cache entry soon.
		return nil
	}
	table.incRefcountLocked()
	return table
}

func (c *tableNameCache) insert(table *tableVersionState) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := makeTableNameCacheKey(table.ParentID, table.Name)
	existing, ok := c.tables[key]
	if !ok {
		c.tables[key] = table
		return
	}
	// If we already have a lease in the cache for this name, see if this one is
	// better (higher version or later expiration).
	if table.Version > existing.Version ||
		(table.Version == existing.Version && table.Expiration().After(existing.Expiration())) {
		// Overwrite the old table. The new one is better. From now on, we want
		// clients to use the new one.
		c.tables[key] = table
	}
}

func (c *tableNameCache) remove(table *tableVersionState) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := makeTableNameCacheKey(table.ParentID, table.Name)
	existing, ok := c.tables[key]
	if !ok {
		// Table for lease not found in table name cache. This can happen if we had
		// a more recent lease on the table in the tableNameCache, then the table
		// gets dropped, then the more recent lease is remove()d - which clears the
		// cache.
		return
	}
	// If this was the lease that the cache had for the table name, remove it.
	// If the cache had some other table, this remove is a no-op.
	if existing == table {
		delete(c.tables, key)
	}
}

func makeTableNameCacheKey(dbID sqlbase.ID, tableName string) tableNameCacheKey {
	return tableNameCacheKey{dbID, parser.ReNormalizeName(tableName)}
}

// LeaseManager manages acquiring and releasing per-table leases. It also
// handles resolving table names to descriptor IDs. The leases are managed
// internally with a table descriptor and expiration time exported by the
// API. The table descriptor acquired needs to be released. A transaction
// can use a table descriptor as long as its timestamp is within the
// validity window for the descriptor:
// descriptor.ModificationTime <= txn.Timestamp < expirationTime
//
// Exported only for testing.
//
// The locking order is:
// LeaseManager.mu > tableState.mu > tableNameCache.mu > tableVersionState.mu
type LeaseManager struct {
	LeaseStore
	mu struct {
		syncutil.Mutex
		tables map[sqlbase.ID]*tableState
	}

	draining atomic.Value

	// tableNames is a cache for name -> id mappings. A mapping for the cache
	// should only be used if we currently have an active lease on the respective
	// id; otherwise, the mapping may well be stale.
	// Not protected by mu.
	tableNames   tableNameCache
	testingKnobs LeaseManagerTestingKnobs
	stopper      *stop.Stopper
}

// NewLeaseManager creates a new LeaseManager.
//
// stopper is used to run async tasks. Can be nil in tests.
func NewLeaseManager(
	nodeID *base.NodeIDContainer,
	db client.DB,
	clock *hlc.Clock,
	testingKnobs LeaseManagerTestingKnobs,
	stopper *stop.Stopper,
	memMetrics *MemoryMetrics,
) *LeaseManager {
	lm := &LeaseManager{
		LeaseStore: LeaseStore{
			db:           db,
			clock:        clock,
			nodeID:       nodeID,
			testingKnobs: testingKnobs.LeaseStoreTestingKnobs,
			memMetrics:   memMetrics,
		},
		testingKnobs: testingKnobs,
		tableNames: tableNameCache{
			tables: make(map[tableNameCacheKey]*tableVersionState),
		},
		stopper: stopper,
	}

	lm.mu.Lock()
	lm.mu.tables = make(map[sqlbase.ID]*tableState)
	lm.mu.Unlock()

	lm.draining.Store(false)
	return lm
}

func nameMatchesTable(table sqlbase.TableDescriptor, dbID sqlbase.ID, tableName string) bool {
	return table.ParentID == dbID &&
		parser.ReNormalizeName(table.Name) == parser.ReNormalizeName(tableName)
}

// AcquireByName acquires a read lease for the specified table.
// The lease is grabbed for the most recent version of the descriptor that the
// lease manager knows about.
func (m *LeaseManager) AcquireByName(
	ctx context.Context, txn *client.Txn, dbID sqlbase.ID, tableName string,
) (sqlbase.TableDescriptor, hlc.Timestamp, error) {
	// Check if we have cached an ID for this name.
	tableVersion := m.tableNames.get(dbID, tableName, m.clock)
	if tableVersion != nil {
		return tableVersion.TableDescriptor, tableVersion.expirationToHLC(), nil
	}

	// We failed to find something in the cache, or what we found is not
	// guaranteed to be valid by the time we use it because we don't have a
	// lease with at least a bit of lifetime left in it. So, we do it the hard
	// way: look in the database to resolve the name, then acquire a new table.
	var err error
	tableID, err := m.resolveName(ctx, txn, dbID, tableName)
	if err != nil {
		return sqlbase.TableDescriptor{}, hlc.Timestamp{}, err
	}
	table, expiration, err := m.Acquire(ctx, txn, tableID, 0)
	if err != nil {
		return sqlbase.TableDescriptor{}, hlc.Timestamp{}, err
	}
	if !nameMatchesTable(table, dbID, tableName) {
		// We resolved name `tableName`, but the lease has a different name in it.
		// That can mean two things. Assume the table is being renamed from A to B.
		// a) `tableName` is A. The transaction doing the RENAME committed (so the
		// descriptor has been updated to B), but its schema changer has not
		// finished yet. B is the new name of the table, queries should use that. If
		// we already had a lease with name A, we would've allowed to use it (but we
		// don't, otherwise the cache lookup above would've given it to us).  Since
		// we don't, let's not allow A to be used, given that the lease now has name
		// B in it. It'd be sketchy to allow A to be used with an inconsistent name
		// in the table.
		//
		// b) `tableName` is B. Like in a), the transaction doing the RENAME
		// committed (so the descriptor has been updated to B), but its schema
		// change has not finished yet. We still had a valid lease with name A in
		// it. What to do, what to do? We could allow name B to be used, but who
		// knows what consequences that would have, since its not consistent with
		// the table. We could say "table B not found", but that means that, until
		// the next gossip update, this node would not service queries for this
		// table under the name B. That's no bueno, as B should be available to be
		// used immediately after the RENAME transaction has committed.
		// The problem is that we have a lease that we know is stale (the descriptor
		// in the DB doesn't necessarily have a new version yet, but it definitely
		// has a new name). So, lets force getting a fresh table.
		// This case (modulo the "committed" part) also applies when the txn doing a
		// RENAME had a lease on the old name, and then tries to use the new name
		// after the RENAME statement.
		//
		// How do we disambiguate between the a) and b)? We get a fresh lease on
		// the descriptor, as required by b), and then we'll know if we're trying to
		// resolve the current or the old name.
		//
		// TODO(vivek): check if the entire above comment is indeed true.
		if err := m.Release(table); err != nil {
			log.Warningf(ctx, "error releasing lease: %s", err)
		}
		table, expiration, err = m.acquireFreshestFromStore(ctx, txn, tableID)
		if err != nil {
			return sqlbase.TableDescriptor{}, hlc.Timestamp{}, err
		}
		if !nameMatchesTable(table, dbID, tableName) {
			// If the name we had doesn't match the newest descriptor in the DB, then
			// we're trying to use an old name.
			if err := m.Release(table); err != nil {
				log.Warningf(ctx, "error releasing lease: %s", err)
			}
			return sqlbase.TableDescriptor{}, hlc.Timestamp{}, sqlbase.ErrDescriptorNotFound
		}
	}
	return table, expiration, nil
}

// resolveName resolves a table name to a descriptor ID by looking in the
// database. If the mapping is not found, sqlbase.ErrDescriptorNotFound is returned.
func (m *LeaseManager) resolveName(
	ctx context.Context, txn *client.Txn, dbID sqlbase.ID, tableName string,
) (sqlbase.ID, error) {
	nameKey := tableKey{dbID, tableName}
	key := nameKey.Key()
	gr, err := txn.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if !gr.Exists() {
		return 0, sqlbase.ErrDescriptorNotFound
	}
	return sqlbase.ID(gr.ValueInt()), nil
}

// Acquire acquires a read lease for the specified table ID. If version is
// non-zero the lease is grabbed for the specified version. Otherwise it is
// grabbed for the most recent version of the descriptor that the lease manager
// knows about. It returns a table descriptor and an expiration time.
// A transaction is allowed to use the table descriptor as long as the txn
// timestamp < expiration time.
// TODO(andrei): move the tests that use this to the sql package and un-export
// it.
func (m *LeaseManager) Acquire(
	ctx context.Context, txn *client.Txn, tableID sqlbase.ID, version sqlbase.DescriptorVersion,
) (sqlbase.TableDescriptor, hlc.Timestamp, error) {
	t := m.findTableState(tableID, true)
	table, err := t.acquire(ctx, txn, version, m)
	if m.LeaseStore.testingKnobs.LeaseAcquiredEvent != nil {
		m.LeaseStore.testingKnobs.LeaseAcquiredEvent(table.TableDescriptor, err)
	}
	if err != nil {
		return sqlbase.TableDescriptor{}, hlc.Timestamp{}, err
	}
	return table.TableDescriptor, table.expirationToHLC(), nil
}

// acquireFreshestFromStore acquires a new lease from the store. The returned
// table is guaranteed to have a version of the descriptor at least as recent as
// the time of the call (i.e. if we were in the process of acquiring a lease
// already, that lease is not good enough). The expiration time is returned
// along with the table descriptor.
func (m *LeaseManager) acquireFreshestFromStore(
	ctx context.Context, txn *client.Txn, tableID sqlbase.ID,
) (sqlbase.TableDescriptor, hlc.Timestamp, error) {
	t := m.findTableState(tableID, true)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.acquireFreshestFromStoreLocked(
		ctx, txn, 0 /* version */, m,
	); err != nil {
		return sqlbase.TableDescriptor{}, hlc.Timestamp{}, err
	}
	table := t.active.findNewest(0)
	if table == nil {
		panic("no lease in active set after having just acquired one")
	}
	table.incRefcount()
	return table.TableDescriptor, table.expirationToHLC(), nil
}

// Release releases a previously acquired table.
func (m *LeaseManager) Release(desc sqlbase.TableDescriptor) error {
	t := m.findTableState(desc.ID, false /* create */)
	if t == nil {
		return errors.Errorf("table %d not found", desc.ID)
	}
	// TODO(pmattis): Can/should we delete from LeaseManager.tables if the
	// tableState becomes empty?
	// TODO(andrei): I think we never delete from LeaseManager.tables... which
	// could be bad if a lot of tables keep being created. I looked into cleaning
	// up a bit, but it seems tricky to do with the current locking which is split
	// between LeaseManager and tableState.
	return t.release(desc, m)
}

func (m *LeaseManager) isDraining() bool {
	return m.draining.Load().(bool)
}

// SetDraining (when called with 'true') removes all inactive leases. Any leases
// that are active will be removed once the lease's reference count drops to 0.
func (m *LeaseManager) SetDraining(drain bool) {
	m.draining.Store(drain)
	if !drain {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.mu.tables {
		t.mu.Lock()
		t.releaseInactiveLeases(m)
		t.mu.Unlock()
	}
}

// If create is set, cache and stopper need to be set as well.
func (m *LeaseManager) findTableState(tableID sqlbase.ID, create bool) *tableState {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.mu.tables[tableID]
	if t == nil && create {
		t = &tableState{id: tableID, tableNameCache: &m.tableNames, stopper: m.stopper}
		m.mu.tables[tableID] = t
	}
	return t
}

// RefreshLeases starts a goroutine that refreshes the lease manager
// leases for tables received in the latest system configuration via gossip.
func (m *LeaseManager) RefreshLeases(s *stop.Stopper, db *client.DB, gossip *gossip.Gossip) {
	ctx := context.TODO()
	s.RunWorker(ctx, func(ctx context.Context) {
		descKeyPrefix := keys.MakeTablePrefix(uint32(sqlbase.DescriptorTable.ID))
		gossipUpdateC := gossip.RegisterSystemConfigChannel()
		for {
			select {
			case <-gossipUpdateC:
				cfg, _ := gossip.GetSystemConfig()
				if m.testingKnobs.GossipUpdateEvent != nil {
					m.testingKnobs.GossipUpdateEvent(cfg)
				}
				// Read all tables and their versions
				if log.V(2) {
					log.Info(ctx, "received a new config; will refresh leases")
				}

				// Loop through the configuration to find all the tables.
				for _, kv := range cfg.Values {
					if !bytes.HasPrefix(kv.Key, descKeyPrefix) {
						continue
					}
					// Attempt to unmarshal config into a table/database descriptor.
					var descriptor sqlbase.Descriptor
					if err := kv.Value.GetProto(&descriptor); err != nil {
						log.Warningf(ctx, "%s: unable to unmarshal descriptor %v", kv.Key, kv.Value)
						continue
					}
					switch union := descriptor.Union.(type) {
					case *sqlbase.Descriptor_Table:
						table := union.Table
						table.MaybeUpgradeFormatVersion()
						if err := table.ValidateTable(); err != nil {
							log.Errorf(ctx, "%s: received invalid table descriptor: %v", kv.Key, table)
							continue
						}
						if log.V(2) {
							log.Infof(ctx, "%s: refreshing lease table: %d (%s), version: %d, dropped: %t",
								kv.Key, table.ID, table.Name, table.Version, table.Dropped())
						}
						// Try to refresh the table lease to one >= this version.
						if t := m.findTableState(table.ID, false /* create */); t != nil {
							if err := t.purgeOldLeases(
								ctx, db, table.Dropped(), table.Version, m); err != nil {
								log.Warningf(ctx, "error purging leases for table %d(%s): %s",
									table.ID, table.Name, err)
							}
						}
					case *sqlbase.Descriptor_Database:
						// Ignore.
					}
				}
				if m.testingKnobs.TestingLeasesRefreshedEvent != nil {
					m.testingKnobs.TestingLeasesRefreshedEvent(cfg)
				}

			case <-s.ShouldStop():
				return
			}
		}
	})
}
