// SPDX-License-Identifier: AGPL-3.0-only

package servers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/control/authapi"
	"github.com/dockwell/dockwell/internal/control/fleet"
	"github.com/dockwell/dockwell/internal/control/history"
	"github.com/dockwell/dockwell/internal/store"
)

type historySpec struct {
	span    time.Duration
	bucket  time.Duration
	primary int   // primary tier
	tiers   []int // tiers to read (preferred first)
}

func rangeSpec(r dockwellv1.HistoryRange) (historySpec, error) {
	switch r {
	case dockwellv1.HistoryRange_HISTORY_RANGE_1H:
		return historySpec{
			span:    time.Hour,
			bucket:  time.Minute,
			primary: store.HistoryMinute,
			tiers:   []int{store.HistoryMinute},
		}, nil
	case dockwellv1.HistoryRange_HISTORY_RANGE_6H:
		return historySpec{
			span:    6 * time.Hour,
			bucket:  2 * time.Minute,
			primary: store.HistoryMinute,
			tiers:   []int{store.HistoryMinute},
		}, nil
	case dockwellv1.HistoryRange_HISTORY_RANGE_UNSPECIFIED, dockwellv1.HistoryRange_HISTORY_RANGE_24H:
		return historySpec{
			span:    24 * time.Hour,
			bucket:  5 * time.Minute,
			primary: store.HistoryFiveMinute,
			tiers:   []int{store.HistoryFiveMinute, store.HistoryMinute},
		}, nil
	case dockwellv1.HistoryRange_HISTORY_RANGE_7D:
		return historySpec{
			span:    7 * 24 * time.Hour,
			bucket:  30 * time.Minute,
			primary: store.HistoryFiveMinute,
			tiers:   []int{store.HistoryFiveMinute, store.HistoryMinute, store.HistoryHour},
		}, nil
	case dockwellv1.HistoryRange_HISTORY_RANGE_30D:
		return historySpec{
			span:    30 * 24 * time.Hour,
			bucket:  3 * time.Hour,
			primary: store.HistoryHour,
			tiers:   []int{store.HistoryHour, store.HistoryFiveMinute},
		}, nil
	default:
		return historySpec{}, fmt.Errorf("unknown history range %v. Use 1 h, 6 h, 24 h, 7 d or 30 d", r)
	}
}

func tierWidth(tier int) time.Duration {
	switch tier {
	case store.HistoryMinute:
		return time.Minute
	case store.HistoryFiveMinute:
		return 5 * time.Minute
	case store.HistoryHour:
		return time.Hour
	default:
		return time.Minute
	}
}

func (s *Service) now() time.Time {
	return time.Now().UTC()
}

// scaleShare returns v * num / den without int64 overflow.
func scaleShare(v, num, den int64) int64 {
	if num == den || den <= 0 {
		return v
	}
	return (v/den)*num + ((v%den)*num)/den
}

// spreadRow places one stored row covering [rowStart, rowStart+rowWidth) into
// every output bucket of [since, until) that it overlaps whose minutes have not
// already been filled by a preferred finer tier. When rowWidth > bucket (e.g. a
// 1-hour fallback row on the 7 d range's 30-minute buckets), each uncovered
// overlapped bucket receives its proportional share of the row's sums and
// copies its peaks, avoiding both false "offline" gaps and double-counting.
func spreadRow(covered []bool, since, until, rowStart time.Time, rowWidth, bucket time.Duration, apply func(idx int, ovMins, wMins int64)) {
	rowEnd := rowStart.Add(rowWidth)
	if !rowEnd.After(since) || !rowStart.Before(until) {
		return
	}
	clampedStart := rowStart
	if clampedStart.Before(since) {
		clampedStart = since
	}
	clampedEnd := rowEnd
	if clampedEnd.After(until) {
		clampedEnd = until
	}
	nBuckets := int(until.Sub(since) / bucket)
	wMins := int64(rowWidth / time.Minute)
	if wMins <= 0 {
		return
	}
	firstIdx := int(clampedStart.Sub(since) / bucket)
	lastIdx := int(clampedEnd.Add(-time.Nanosecond).Sub(since) / bucket)
	for idx := max(firstIdx, 0); idx <= min(lastIdx, nBuckets-1); idx++ {
		bStart := since.Add(time.Duration(idx) * bucket)
		bEnd := bStart.Add(bucket)
		ovStart := clampedStart
		if bStart.After(ovStart) {
			ovStart = bStart
		}
		ovEnd := clampedEnd
		if bEnd.Before(ovEnd) {
			ovEnd = bEnd
		}
		if !ovEnd.After(ovStart) {
			continue
		}
		m0 := int(ovStart.Sub(since) / time.Minute)
		m1 := int(ovEnd.Sub(since) / time.Minute)
		if m0 < 0 || m1 > len(covered) || m1 <= m0 {
			continue
		}
		var uncovered int64
		for m := m0; m < m1; m++ {
			if !covered[m] {
				covered[m] = true
				uncovered++
			}
		}
		if uncovered == 0 {
			continue
		}
		apply(idx, uncovered, wMins)
	}
}

// effectiveBucketMs returns the effective duration in ms of [bStart, bEnd)
// clamped to [floorTime, now], with a 1 s minimum.
func effectiveBucketMs(bStart, bEnd, floorTime, now time.Time) int64 {
	effStart := bStart
	if !floorTime.IsZero() && floorTime.After(effStart) && floorTime.Before(bEnd) {
		effStart = floorTime
	}
	effEnd := bEnd
	if now.Before(effEnd) {
		effEnd = now
	}
	return max(effEnd.Sub(effStart).Milliseconds(), int64(1000))
}

// GetServerHistory returns downsampled server history on an even clock over the
// requested range, with per-disk and per-interface filtering and explicit gap states.
func (s *Service) GetServerHistory(ctx context.Context, req *connect.Request[dockwellv1.GetServerHistoryRequest]) (*connect.Response[dockwellv1.GetServerHistoryResponse], error) {
	if s.db == nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, errors.New("history storage is not configured on this control plane"), authapi.ReasonUnavailable)
	}
	srv, err := s.fleet.Get(req.Msg.GetId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there's no server "+req.Msg.GetId()+". List them with ListServers"))
	}
	if err != nil {
		return nil, err
	}
	spec, err := rangeSpec(req.Msg.GetRange())
	if err != nil {
		return nil, authapi.Invalid(err)
	}
	disk := req.Msg.GetDisk()
	if len(disk) > 64 {
		return nil, authapi.Invalid(errors.New("disk name is too long"))
	}
	iface := req.Msg.GetNetworkInterface()
	if len(iface) > 64 {
		return nil, authapi.Invalid(errors.New("network interface name is too long"))
	}

	now := s.now()
	until := now.Truncate(spec.bucket)
	if now.Sub(until) >= time.Minute {
		until = until.Add(spec.bucket)
	}
	since := until.Add(-spec.span)
	var joinedAt time.Time
	if srv.GetCreateTime() != nil {
		joinedAt = srv.GetCreateTime().AsTime().UTC()
	}

	var (
		rowsByTier     = make(map[int][]store.HistoryRow, len(spec.tiers))
		diskRowsByTier = make(map[int][]store.DiskHistoryRow, len(spec.tiers))
		netRowsByTier  = make(map[int][]store.NetHistoryRow, len(spec.tiers))
		disks          []string
		interfaces     []string
	)
	if err := s.db.View(ctx, func(tx *store.Tx) error {
		for _, tier := range spec.tiers {
			querySince := since.Truncate(tierWidth(tier))
			rows, err := tx.History(ctx, srv.GetId(), tier, querySince, until)
			if err != nil {
				return err
			}
			rowsByTier[tier] = rows
			if disk != "" {
				drows, err := tx.DiskHistory(ctx, srv.GetId(), disk, tier, querySince, until)
				if err != nil {
					return err
				}
				diskRowsByTier[tier] = drows
			}
			if iface != "" {
				nrows, err := tx.NetHistory(ctx, srv.GetId(), iface, tier, querySince, until)
				if err != nil {
					return err
				}
				netRowsByTier[tier] = nrows
			}
		}
		var err error
		if disks, err = tx.DiskHistoryDevices(ctx, srv.GetId(), spec.tiers, since, until); err != nil {
			return err
		}
		if interfaces, err = tx.NetHistoryInterfaces(ctx, srv.GetId(), spec.tiers, since, until); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	nBuckets := int(spec.span / spec.bucket)
	totalMins := int(spec.span / time.Minute)
	points := make([]*dockwellv1.ServerHistoryPoint, nBuckets)

	type bucketAgg struct {
		hasRow bool
		store.HistoryRow
		diskHas bool
		disk    store.DiskHistoryRow
		netHas  bool
		net     store.NetHistoryRow
	}
	buckets := make([]bucketAgg, nBuckets)
	covered := make([]bool, totalMins)
	diskCovered := make([]bool, totalMins)
	netCovered := make([]bool, totalMins)

	for _, tier := range spec.tiers {
		w := tierWidth(tier)
		tierRows := rowsByTier[tier]
		for i := range tierRows {
			r := &tierRows[i]
			spreadRow(covered, since, until, r.Start, w, spec.bucket, func(idx int, ovMins, wMins int64) {
				b := &buckets[idx]
				b.hasRow = true
				b.CoveredMs += scaleShare(r.CoveredMs, ovMins, wMins)
				b.CPUSum += scaleShare(r.CPUSum, ovMins, wMins)
				b.CPUMax = max(b.CPUMax, r.CPUMax)
				b.MemorySum += scaleShare(r.MemorySum, ovMins, wMins)
				b.MemoryMax = max(b.MemoryMax, r.MemoryMax)
				b.MemoryTotal = max(b.MemoryTotal, r.MemoryTotal)
				b.SwapSum += scaleShare(r.SwapSum, ovMins, wMins)
				b.SwapMax = max(b.SwapMax, r.SwapMax)
				b.LoadSum += scaleShare(r.LoadSum, ovMins, wMins)
				b.LoadMax = max(b.LoadMax, r.LoadMax)
				b.DiskSum += scaleShare(r.DiskSum, ovMins, wMins)
				b.DiskMax = max(b.DiskMax, r.DiskMax)
				b.RxBytes += scaleShare(r.RxBytes, ovMins, wMins)
				b.RxPeak = max(b.RxPeak, r.RxPeak)
				b.TxBytes += scaleShare(r.TxBytes, ovMins, wMins)
				b.TxPeak = max(b.TxPeak, r.TxPeak)
				b.ReadBytes += scaleShare(r.ReadBytes, ovMins, wMins)
				b.ReadPeak = max(b.ReadPeak, r.ReadPeak)
				b.WrittenBytes += scaleShare(r.WrittenBytes, ovMins, wMins)
				b.WrittenPeak = max(b.WrittenPeak, r.WrittenPeak)
				b.ReadOps += scaleShare(r.ReadOps, ovMins, wMins)
				b.ReadIOPSPeak = max(b.ReadIOPSPeak, r.ReadIOPSPeak)
				b.WriteOps += scaleShare(r.WriteOps, ovMins, wMins)
				b.WriteIOPSPeak = max(b.WriteIOPSPeak, r.WriteIOPSPeak)
			})
		}
		diskRows := diskRowsByTier[tier]
		for i := range diskRows {
			r := &diskRows[i]
			spreadRow(diskCovered, since, until, r.Start, w, spec.bucket, func(idx int, ovMins, wMins int64) {
				b := &buckets[idx]
				b.diskHas = true
				b.disk.CoveredMs += scaleShare(r.CoveredMs, ovMins, wMins)
				b.disk.ReadBytes += scaleShare(r.ReadBytes, ovMins, wMins)
				b.disk.ReadPeak = max(b.disk.ReadPeak, r.ReadPeak)
				b.disk.WrittenBytes += scaleShare(r.WrittenBytes, ovMins, wMins)
				b.disk.WrittenPeak = max(b.disk.WrittenPeak, r.WrittenPeak)
				b.disk.ReadOps += scaleShare(r.ReadOps, ovMins, wMins)
				b.disk.ReadIOPSPeak = max(b.disk.ReadIOPSPeak, r.ReadIOPSPeak)
				b.disk.WriteOps += scaleShare(r.WriteOps, ovMins, wMins)
				b.disk.WriteIOPSPeak = max(b.disk.WriteIOPSPeak, r.WriteIOPSPeak)
			})
		}
		netRows := netRowsByTier[tier]
		for i := range netRows {
			r := &netRows[i]
			spreadRow(netCovered, since, until, r.Start, w, spec.bucket, func(idx int, ovMins, wMins int64) {
				b := &buckets[idx]
				b.netHas = true
				b.net.CoveredMs += scaleShare(r.CoveredMs, ovMins, wMins)
				b.net.RxBytes += scaleShare(r.RxBytes, ovMins, wMins)
				b.net.RxPeak = max(b.net.RxPeak, r.RxPeak)
				b.net.TxBytes += scaleShare(r.TxBytes, ovMins, wMins)
				b.net.TxPeak = max(b.net.TxPeak, r.TxPeak)
			})
		}
	}

	for i := range nBuckets {
		bStart := since.Add(time.Duration(i) * spec.bucket)
		bEnd := bStart.Add(spec.bucket)
		b := &buckets[i]
		pt := &dockwellv1.ServerHistoryPoint{Time: timestamppb.New(bStart)}

		if !b.hasRow {
			if !joinedAt.IsZero() && !bEnd.After(joinedAt) {
				pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_BEFORE_JOIN
			} else {
				pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_NO_CONTROL_PLANE
			}
			points[i] = pt
			continue
		}
		effMs := effectiveBucketMs(bStart, bEnd, joinedAt, now)
		if b.CoveredMs <= 0 || b.CoveredMs*2 < effMs {
			pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_OFFLINE
			points[i] = pt
			continue
		}

		pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_COVERED
		cov := b.CoveredMs
		secs := float64(cov) / 1000.0
		pt.CpuPercent = float64(b.CPUSum) / float64(cov) / 1000.0
		pt.CpuPeakPercent = float64(b.CPUMax) / 1000.0
		pt.MemoryUsedBytes = uint64(max(b.MemorySum/cov, 0)) << 10
		pt.MemoryPeakBytes = uint64(max(b.MemoryMax, 0)) << 10
		pt.MemoryTotalBytes = uint64(max(b.MemoryTotal, 0)) << 10
		pt.SwapUsedBytes = uint64(max(b.SwapSum/cov, 0)) << 10
		pt.Load1 = float64(b.LoadSum) / float64(cov) / 1000.0
		pt.Load1Peak = float64(b.LoadMax) / 1000.0
		pt.DiskPercent = float64(b.DiskSum) / float64(cov) / 10.0

		if iface != "" {
			if b.netHas && b.net.CoveredMs > 0 && b.net.CoveredMs*2 >= effMs {
				nse := float64(b.net.CoveredMs) / 1000.0
				pt.NetworkRxBytesPerSecond = uint64(max(float64(b.net.RxBytes)/nse, 0))
				pt.NetworkRxPeakBytesPerSecond = uint64(max(b.net.RxPeak, 0))
				pt.NetworkTxBytesPerSecond = uint64(max(float64(b.net.TxBytes)/nse, 0))
				pt.NetworkTxPeakBytesPerSecond = uint64(max(b.net.TxPeak, 0))
			} else {
				pt.NetworkIoMissing = true
			}
		} else {
			pt.NetworkRxBytesPerSecond = uint64(max(float64(b.RxBytes)/secs, 0))
			pt.NetworkRxPeakBytesPerSecond = uint64(max(b.RxPeak, 0))
			pt.NetworkTxBytesPerSecond = uint64(max(float64(b.TxBytes)/secs, 0))
			pt.NetworkTxPeakBytesPerSecond = uint64(max(b.TxPeak, 0))
		}

		if disk != "" {
			if b.diskHas && b.disk.CoveredMs > 0 && b.disk.CoveredMs*2 >= effMs {
				dse := float64(b.disk.CoveredMs) / 1000.0
				pt.DiskReadBytesPerSecond = uint64(max(float64(b.disk.ReadBytes)/dse, 0))
				pt.DiskReadPeakBytesPerSecond = uint64(max(b.disk.ReadPeak, 0))
				pt.DiskWriteBytesPerSecond = uint64(max(float64(b.disk.WrittenBytes)/dse, 0))
				pt.DiskWritePeakBytesPerSecond = uint64(max(b.disk.WrittenPeak, 0))
				pt.DiskReadIops = float64(b.disk.ReadOps) / dse
				pt.DiskReadIopsPeak = float64(b.disk.ReadIOPSPeak) / 1000.0
				pt.DiskWriteIops = float64(b.disk.WriteOps) / dse
				pt.DiskWriteIopsPeak = float64(b.disk.WriteIOPSPeak) / 1000.0
			} else {
				pt.DiskIoMissing = true
			}
		} else {
			pt.DiskReadBytesPerSecond = uint64(max(float64(b.ReadBytes)/secs, 0))
			pt.DiskReadPeakBytesPerSecond = uint64(max(b.ReadPeak, 0))
			pt.DiskWriteBytesPerSecond = uint64(max(float64(b.WrittenBytes)/secs, 0))
			pt.DiskWritePeakBytesPerSecond = uint64(max(b.WrittenPeak, 0))
			pt.DiskReadIops = float64(b.ReadOps) / secs
			pt.DiskReadIopsPeak = float64(b.ReadIOPSPeak) / 1000.0
			pt.DiskWriteIops = float64(b.WriteOps) / secs
			pt.DiskWriteIopsPeak = float64(b.WriteIOPSPeak) / 1000.0
		}

		points[i] = pt
	}

	resp := &dockwellv1.GetServerHistoryResponse{
		Since:      timestamppb.New(since),
		Until:      timestamppb.New(until),
		Bucket:     durationpb.New(spec.bucket),
		Disks:      disks,
		Interfaces: interfaces,
		Points:     points,
	}
	if !joinedAt.IsZero() {
		resp.JoinedAt = timestamppb.New(joinedAt)
	}
	return connect.NewResponse(resp), nil
}

// GetContainerHistory returns downsampled CPU and memory history for one
// container on a server over the requested range.
func (s *Service) GetContainerHistory(ctx context.Context, req *connect.Request[dockwellv1.GetContainerHistoryRequest]) (*connect.Response[dockwellv1.GetContainerHistoryResponse], error) {
	if s.db == nil {
		return nil, authapi.ReasonError(connect.CodeUnavailable, errors.New("history storage is not configured on this control plane"), authapi.ReasonUnavailable)
	}
	srv, err := s.fleet.Get(req.Msg.GetServerId())
	if errors.Is(err, fleet.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("there's no server "+req.Msg.GetServerId()+". List them with ListServers"))
	}
	if err != nil {
		return nil, err
	}
	cid := req.Msg.GetContainerId()
	if !validContainerID(cid) {
		return nil, authapi.Invalid(errors.New("container_id must be a 64-character lowercase hex Docker container ID"))
	}
	spec, err := rangeSpec(req.Msg.GetRange())
	if err != nil {
		return nil, authapi.Invalid(err)
	}

	now := s.now()
	until := now.Truncate(spec.bucket)
	if now.Sub(until) >= time.Minute {
		until = until.Add(spec.bucket)
	}
	since := until.Add(-spec.span)
	var joinedAt, createdAt time.Time
	if srv.GetCreateTime() != nil {
		joinedAt = srv.GetCreateTime().AsTime().UTC()
	}
	if req.Msg.GetCreatedAt() != nil {
		createdAt = req.Msg.GetCreatedAt().AsTime().UTC()
	}

	contByTier := make(map[int][]store.ContainerHistoryRow, len(spec.tiers))
	srvByTier := make(map[int][]store.HistoryRow, len(spec.tiers))
	if err := s.db.View(ctx, func(tx *store.Tx) error {
		for _, tier := range spec.tiers {
			querySince := since.Truncate(tierWidth(tier))
			crows, err := tx.ContainerHistory(ctx, srv.GetId(), cid, tier, querySince, until)
			if err != nil {
				return err
			}
			contByTier[tier] = crows
			srows, err := tx.History(ctx, srv.GetId(), tier, querySince, until)
			if err != nil {
				return err
			}
			srvByTier[tier] = srows
		}
		return nil
	}); err != nil {
		return nil, err
	}

	nBuckets := int(spec.span / spec.bucket)
	totalMins := int(spec.span / time.Minute)
	type agg struct {
		hasCont   bool
		hasServer bool
		srvCovMs  int64
		store.ContainerHistoryRow
	}
	buckets := make([]agg, nBuckets)
	contCovered := make([]bool, totalMins)
	srvCovered := make([]bool, totalMins)

	anyContRow := false
	for _, tier := range spec.tiers {
		w := tierWidth(tier)
		srvRows := srvByTier[tier]
		for i := range srvRows {
			r := &srvRows[i]
			spreadRow(srvCovered, since, until, r.Start, w, spec.bucket, func(idx int, ovMins, wMins int64) {
				buckets[idx].hasServer = true
				buckets[idx].srvCovMs += scaleShare(r.CoveredMs, ovMins, wMins)
			})
		}
		contRows := contByTier[tier]
		for i := range contRows {
			r := &contRows[i]
			spreadRow(contCovered, since, until, r.Start, w, spec.bucket, func(idx int, ovMins, wMins int64) {
				b := &buckets[idx]
				b.hasCont = true
				anyContRow = true
				b.CoveredMs += scaleShare(r.CoveredMs, ovMins, wMins)
				b.CPUSum += scaleShare(r.CPUSum, ovMins, wMins)
				b.CPUMax = max(b.CPUMax, r.CPUMax)
				b.MemorySum += scaleShare(r.MemorySum, ovMins, wMins)
				b.MemoryMax = max(b.MemoryMax, r.MemoryMax)
				b.MemLimit = max(b.MemLimit, r.MemLimit)
			})
		}
	}

	untracked := false
	if s.history != nil {
		untracked = s.history.ContainerUntracked(srv.GetId(), cid)
	} else if srv.GetStats().GetContainersOmitted() > 0 && !anyContRow {
		untracked = true
	}

	contFloor := joinedAt
	if !createdAt.IsZero() && createdAt.After(contFloor) {
		contFloor = createdAt
	}

	points := make([]*dockwellv1.ContainerHistoryPoint, nBuckets)
	for i := range nBuckets {
		bStart := since.Add(time.Duration(i) * spec.bucket)
		bEnd := bStart.Add(spec.bucket)
		b := &buckets[i]
		pt := &dockwellv1.ContainerHistoryPoint{Time: timestamppb.New(bStart)}

		srvEffMs := effectiveBucketMs(bStart, bEnd, joinedAt, now)
		contEffMs := effectiveBucketMs(bStart, bEnd, contFloor, now)
		// Apply the same half-coverage rule as server charts (L6).
		if b.hasCont && b.CoveredMs > 0 && b.CoveredMs*2 >= contEffMs {
			cov := b.CoveredMs
			pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_COVERED
			pt.CpuPercent = float64(b.CPUSum) / float64(cov) / 1000.0
			pt.CpuPeakPercent = float64(b.CPUMax) / 1000.0
			pt.MemoryBytes = uint64(max(b.MemorySum/cov, 0)) << 10
			pt.MemoryPeakBytes = uint64(max(b.MemoryMax, 0)) << 10
			pt.MemoryLimitBytes = uint64(max(b.MemLimit, 0)) << 10
			points[i] = pt
			continue
		}
		switch {
		case !b.hasServer:
			if !joinedAt.IsZero() && !bEnd.After(joinedAt) {
				pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_BEFORE_JOIN
			} else {
				pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_NO_CONTROL_PLANE
			}
		case b.srvCovMs <= 0 || b.srvCovMs*2 < srvEffMs:
			// The server's agent itself had insufficient coverage in this bucket.
			pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_OFFLINE
		case !createdAt.IsZero() && !bEnd.After(createdAt):
			pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_BEFORE_CREATE
		case untracked && !b.hasCont:
			pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_NOT_TRACKED
		default:
			pt.State = dockwellv1.HistoryPointState_HISTORY_POINT_STATE_NOT_RUNNING
		}
		points[i] = pt
	}

	resp := &dockwellv1.GetContainerHistoryResponse{
		Since:          timestamppb.New(since),
		Until:          timestamppb.New(until),
		Bucket:         durationpb.New(spec.bucket),
		Points:         points,
		Untracked:      untracked,
		ContainerLimit: history.MaxContainersPerServer,
	}
	if !joinedAt.IsZero() {
		resp.JoinedAt = timestamppb.New(joinedAt)
	}
	return connect.NewResponse(resp), nil
}

func validContainerID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
