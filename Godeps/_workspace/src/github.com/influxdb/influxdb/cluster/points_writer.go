package cluster

import (
	"errors"
	"expvar"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/influxdb/influxdb"
	"github.com/influxdb/influxdb/meta"
	"github.com/influxdb/influxdb/models"
	"github.com/influxdb/influxdb/tsdb"
)

// ConsistencyLevel represent a required replication criteria before a write can
// be returned as successful
type ConsistencyLevel int

// The statistics generated by the "write" mdoule
const (
	statWriteReq            = "req"
	statPointWriteReq       = "point_req"
	statPointWriteReqLocal  = "point_req_local"
	statPointWriteReqRemote = "point_req_remote"
	statWriteOK             = "write_ok"
	statWritePartial        = "write_partial"
	statWriteTimeout        = "write_timeout"
	statWriteErr            = "write_error"
	statWritePointReqHH     = "point_req_hh"
)

const (
	// ConsistencyLevelAny allows for hinted hand off, potentially no write happened yet
	ConsistencyLevelAny ConsistencyLevel = iota

	// ConsistencyLevelOne requires at least one data node acknowledged a write
	ConsistencyLevelOne

	// ConsistencyLevelOne requires a quorum of data nodes to acknowledge a write
	ConsistencyLevelQuorum

	// ConsistencyLevelAll requires all data nodes to acknowledge a write
	ConsistencyLevelAll
)

var (
	// ErrTimeout is returned when a write times out.
	ErrTimeout = errors.New("timeout")

	// ErrPartialWrite is returned when a write partially succeeds but does
	// not meet the requested consistency level.
	ErrPartialWrite = errors.New("partial write")

	// ErrWriteFailed is returned when no writes succeeded.
	ErrWriteFailed = errors.New("write failed")

	// ErrInvalidConsistencyLevel is returned when parsing the string version
	// of a consistency level.
	ErrInvalidConsistencyLevel = errors.New("invalid consistency level")
)

func ParseConsistencyLevel(level string) (ConsistencyLevel, error) {
	switch strings.ToLower(level) {
	case "any":
		return ConsistencyLevelAny, nil
	case "one":
		return ConsistencyLevelOne, nil
	case "quorum":
		return ConsistencyLevelQuorum, nil
	case "all":
		return ConsistencyLevelAll, nil
	default:
		return 0, ErrInvalidConsistencyLevel
	}
}

// PointsWriter handles writes across multiple local and remote data nodes.
type PointsWriter struct {
	mu           sync.RWMutex
	closing      chan struct{}
	WriteTimeout time.Duration
	Logger       *log.Logger

	MetaStore interface {
		NodeID() uint64
		Database(name string) (di *meta.DatabaseInfo, err error)
		RetentionPolicy(database, policy string) (*meta.RetentionPolicyInfo, error)
		CreateShardGroupIfNotExists(database, policy string, timestamp time.Time) (*meta.ShardGroupInfo, error)
		ShardOwner(shardID uint64) (string, string, *meta.ShardGroupInfo)
	}

	TSDBStore interface {
		CreateShard(database, retentionPolicy string, shardID uint64) error
		WriteToShard(shardID uint64, points []models.Point) error
	}

	ShardWriter interface {
		WriteShard(shardID, ownerID uint64, points []models.Point) error
	}

	HintedHandoff interface {
		WriteShard(shardID, ownerID uint64, points []models.Point) error
	}

	statMap *expvar.Map
}

// NewPointsWriter returns a new instance of PointsWriter for a node.
func NewPointsWriter() *PointsWriter {
	return &PointsWriter{
		closing:      make(chan struct{}),
		WriteTimeout: DefaultWriteTimeout,
		Logger:       log.New(os.Stderr, "[write] ", log.LstdFlags),
		statMap:      influxdb.NewStatistics("write", "write", nil),
	}
}

// ShardMapping contains a mapping of a shards to a points.
type ShardMapping struct {
	Points map[uint64][]models.Point  // The points associated with a shard ID
	Shards map[uint64]*meta.ShardInfo // The shards that have been mapped, keyed by shard ID
}

// NewShardMapping creates an empty ShardMapping
func NewShardMapping() *ShardMapping {
	return &ShardMapping{
		Points: map[uint64][]models.Point{},
		Shards: map[uint64]*meta.ShardInfo{},
	}
}

// MapPoint maps a point to shard
func (s *ShardMapping) MapPoint(shardInfo *meta.ShardInfo, p models.Point) {
	points, ok := s.Points[shardInfo.ID]
	if !ok {
		s.Points[shardInfo.ID] = []models.Point{p}
	} else {
		s.Points[shardInfo.ID] = append(points, p)
	}
	s.Shards[shardInfo.ID] = shardInfo
}

func (w *PointsWriter) Open() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing == nil {
		w.closing = make(chan struct{})
	}
	return nil
}

func (w *PointsWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closing != nil {
		close(w.closing)
		w.closing = nil
	}
	return nil
}

// MapShards maps the points contained in wp to a ShardMapping.  If a point
// maps to a shard group or shard that does not currently exist, it will be
// created before returning the mapping.
func (w *PointsWriter) MapShards(wp *WritePointsRequest) (*ShardMapping, error) {

	// holds the start time ranges for required shard groups
	timeRanges := map[time.Time]*meta.ShardGroupInfo{}

	rp, err := w.MetaStore.RetentionPolicy(wp.Database, wp.RetentionPolicy)
	if err != nil {
		return nil, err
	}
	if rp == nil {
		return nil, influxdb.ErrRetentionPolicyNotFound(wp.RetentionPolicy)
	}

	for _, p := range wp.Points {
		timeRanges[p.Time().Truncate(rp.ShardGroupDuration)] = nil
	}

	// holds all the shard groups and shards that are required for writes
	for t := range timeRanges {
		sg, err := w.MetaStore.CreateShardGroupIfNotExists(wp.Database, wp.RetentionPolicy, t)
		if err != nil {
			return nil, err
		}
		timeRanges[t] = sg
	}

	mapping := NewShardMapping()
	for _, p := range wp.Points {
		sg := timeRanges[p.Time().Truncate(rp.ShardGroupDuration)]
		sh := sg.ShardFor(p.HashID())
		mapping.MapPoint(&sh, p)
	}
	return mapping, nil
}

// WritePoints writes across multiple local and remote data nodes according the consistency level.
func (w *PointsWriter) WritePoints(p *WritePointsRequest) error {
	w.statMap.Add(statWriteReq, 1)
	w.statMap.Add(statPointWriteReq, int64(len(p.Points)))

	if p.RetentionPolicy == "" {
		db, err := w.MetaStore.Database(p.Database)
		if err != nil {
			return err
		} else if db == nil {
			return influxdb.ErrDatabaseNotFound(p.Database)
		}
		p.RetentionPolicy = db.DefaultRetentionPolicy
	}

	shardMappings, err := w.MapShards(p)
	if err != nil {
		return err
	}

	// Write each shard in it's own goroutine and return as soon
	// as one fails.
	ch := make(chan error, len(shardMappings.Points))
	for shardID, points := range shardMappings.Points {
		go func(shard *meta.ShardInfo, database, retentionPolicy string, points []models.Point) {
			ch <- w.writeToShard(shard, p.Database, p.RetentionPolicy, p.ConsistencyLevel, points)
		}(shardMappings.Shards[shardID], p.Database, p.RetentionPolicy, points)
	}

	for range shardMappings.Points {
		select {
		case <-w.closing:
			return ErrWriteFailed
		case err := <-ch:
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// writeToShards writes points to a shard and ensures a write consistency level has been met.  If the write
// partially succeeds, ErrPartialWrite is returned.
func (w *PointsWriter) writeToShard(shard *meta.ShardInfo, database, retentionPolicy string,
	consistency ConsistencyLevel, points []models.Point) error {
	// The required number of writes to achieve the requested consistency level
	required := len(shard.Owners)
	switch consistency {
	case ConsistencyLevelAny, ConsistencyLevelOne:
		required = 1
	case ConsistencyLevelQuorum:
		required = required/2 + 1
	}

	// response channel for each shard writer go routine
	type AsyncWriteResult struct {
		Owner meta.ShardOwner
		Err   error
	}
	ch := make(chan *AsyncWriteResult, len(shard.Owners))

	for _, owner := range shard.Owners {
		go func(shardID uint64, owner meta.ShardOwner, points []models.Point) {
			if w.MetaStore.NodeID() == owner.NodeID {
				w.statMap.Add(statPointWriteReqLocal, int64(len(points)))

				err := w.TSDBStore.WriteToShard(shardID, points)
				// If we've written to shard that should exist on the current node, but the store has
				// not actually created this shard, tell it to create it and retry the write
				if err == tsdb.ErrShardNotFound {
					err = w.TSDBStore.CreateShard(database, retentionPolicy, shardID)
					if err != nil {
						ch <- &AsyncWriteResult{owner, err}
						return
					}
					err = w.TSDBStore.WriteToShard(shardID, points)
				}
				ch <- &AsyncWriteResult{owner, err}
				return
			}

			w.statMap.Add(statPointWriteReqRemote, int64(len(points)))
			err := w.ShardWriter.WriteShard(shardID, owner.NodeID, points)
			if err != nil && tsdb.IsRetryable(err) {
				// The remote write failed so queue it via hinted handoff
				w.statMap.Add(statWritePointReqHH, int64(len(points)))
				hherr := w.HintedHandoff.WriteShard(shardID, owner.NodeID, points)

				// If the write consistency level is ANY, then a successful hinted handoff can
				// be considered a successful write so send nil to the response channel
				// otherwise, let the original error propogate to the response channel
				if hherr == nil && consistency == ConsistencyLevelAny {
					ch <- &AsyncWriteResult{owner, nil}
					return
				}
			}
			ch <- &AsyncWriteResult{owner, err}

		}(shard.ID, owner, points)
	}

	var wrote int
	timeout := time.After(w.WriteTimeout)
	var writeError error
	for range shard.Owners {
		select {
		case <-w.closing:
			return ErrWriteFailed
		case <-timeout:
			w.statMap.Add(statWriteTimeout, 1)
			// return timeout error to caller
			return ErrTimeout
		case result := <-ch:
			// If the write returned an error, continue to the next response
			if result.Err != nil {
				w.statMap.Add(statWriteErr, 1)
				w.Logger.Printf("write failed for shard %d on node %d: %v", shard.ID, result.Owner.NodeID, result.Err)

				// Keep track of the first error we see to return back to the client
				if writeError == nil {
					writeError = result.Err
				}
				continue
			}

			wrote += 1

			// We wrote the required consistency level
			if wrote >= required {
				w.statMap.Add(statWriteOK, 1)
				return nil
			}
		}
	}

	if wrote > 0 {
		w.statMap.Add(statWritePartial, 1)
		return ErrPartialWrite
	}

	if writeError != nil {
		return fmt.Errorf("write failed: %v", writeError)
	}

	return ErrWriteFailed
}