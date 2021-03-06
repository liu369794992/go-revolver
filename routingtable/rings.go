package routingtable

import (
	"math/rand"
	"sync"
	"time"

	"github.com/enzoh/go-logging"
	"gx/ipfs/QmPgDWmTmuzvP7QE5zwo1TmjbJme9pmZHNujB2453jkCTr/go-libp2p-peerstore"
	"gx/ipfs/QmXYjuNuxVzXKJCfWasQk1RqkhVLDM9jtUKhqc2WPQmFSB/go-libp2p-peer"
)

// LatencyProbeFn is a function that accepts a peer ID and returns a latency.
type LatencyProbeFn func(peer.ID) (time.Duration, error)

// RingsConfig configures a Ring-based routing table
type RingsConfig struct {
	RingsCount          int
	BaseLatency         time.Duration
	LatencyGrowthFactor float64
	SampleSize          int
	SamplePeriod        time.Duration
	// A function for retrieving the up-to-date latency information for a given
	// peer.
	LatencyProbFn LatencyProbeFn

	Logger logging.Logger
}

// ringsRoutingTable is a RoutingTable based on latency rings.
type ringsRoutingTable struct {
	sync.RWMutex

	// The config
	conf RingsConfig

	// A set of known peers
	peers map[peer.ID]bool

	// The rings
	rings []*ring

	// The latency range of the rings.  Specifically, for the nth ring, the
	// latency range is [latRanges[n], latRanges[n+1]).
	latRanges []time.Duration

	// For storing latency info
	metrics peerstore.Metrics

	// Shutdown signal
	shutdown chan struct{}
}

// A ring stores a list of peers within a certain latency range
type ring struct {
	peers []peer.ID
}

// Add a peer to the ring
func (r *ring) Add(pid peer.ID) {
	r.peers = append(r.peers, pid)
}

// Remove a peer from the ring
func (r *ring) Remove(pid peer.ID) {
	for i, peer := range r.peers {
		if peer == pid {
			r.peers = append(r.peers[:i], r.peers[i+1:]...)
		}
	}
}

// Return `count` random peers in the ring, except for those in the `exclude`
// list.
func (r *ring) Recommend(count int, exclude map[peer.ID]bool) []peer.ID {
	var recommended []peer.ID

	perm := rand.Perm(len(r.peers))
	for i := 0; i < count && i < len(perm); i++ {
		pid := r.peers[perm[i]]
		if !exclude[pid] {
			recommended = append(recommended, pid)
		}
	}

	return recommended
}

// NewDefaultRingsConfig creates a RingsConfig with default parameters.
func NewDefaultRingsConfig(probe LatencyProbeFn) RingsConfig {
	return RingsConfig{
		RingsCount:          8,
		BaseLatency:         8 * time.Millisecond,
		LatencyGrowthFactor: 2,
		SampleSize:          16,
		SamplePeriod:        30 * time.Second,
		LatencyProbFn:       probe,
	}
}

// NewRingsRoutingTable creates a RoutingTable with the given config.
func NewRingsRoutingTable(conf RingsConfig) RoutingTable {
	// Construct the latency ranges
	// The first element is always going to be 0.
	latRanges := []time.Duration{time.Duration(0)}
	k := conf.BaseLatency
	for i := 1; i < conf.RingsCount; i++ {
		latRanges = append(latRanges, k)
		k = time.Duration(float64(k) * conf.LatencyGrowthFactor)
	}

	var rings []*ring
	for i := 0; i < conf.RingsCount; i++ {
		rings = append(rings, &ring{})
	}

	r := &ringsRoutingTable{
		conf:      conf,
		rings:     rings,
		peers:     make(map[peer.ID]bool),
		metrics:   peerstore.NewMetrics(),
		latRanges: latRanges,
		shutdown:  make(chan struct{}),
	}

	// Periodically refresh latency and re-balance rings until explicitly shut
	// down.
	go func() {
		select {
		case <-time.After(r.conf.SamplePeriod):
			r.refreshLatency()
			r.populateRings()
		case <-r.shutdown:
			return
		}
	}()

	return r
}

// refreshLatency picks a random subset of peers and refresh their latency
// information.  The rings are then re-populated.
func (r *ringsRoutingTable) refreshLatency() {
	var pids []peer.ID
	var peerCount int
	func() {
		r.RLock()
		defer r.RUnlock()

		// Get a list of all peers
		for pid := range r.peers {
			pids = append(pids, pid)
		}
		peerCount = len(pids)
	}()

	// Get a random sample of the peers
	var sample []peer.ID
	perm := rand.Perm(peerCount)
	for i := 0; i < r.conf.SampleSize && i < peerCount; i++ {
		sample = append(sample, pids[perm[i]])
	}

	for _, pid := range sample {
		latency, err := r.conf.LatencyProbFn(pid)
		if err != nil {
			r.conf.Logger.Errorf("error probing latency of peer %v", pid)
		} else {
			func() {
				r.Lock()
				defer r.Unlock()
				r.metrics.RecordLatency(pid, latency)
			}()
		}
	}
}

// populateRings puts peers into the rings.
func (r *ringsRoutingTable) populateRings() {
	r.Lock()
	defer r.Unlock()

	var rings []*ring
	for i := 0; i < r.conf.RingsCount; i++ {
		rings = append(rings, &ring{})
	}

	for pid := range r.peers {
		latency := r.metrics.LatencyEWMA(pid)
		// Find the ring that the peer belongs to
		for i := len(r.latRanges) - 1; i >= 0; i-- {
			if latency > r.latRanges[i] {
				rings[i].Add(pid)
				break
			}
		}
	}

	r.rings = rings
}

func (r *ringsRoutingTable) Add(pid peer.ID) {
	// Do nothing if we already know about this peer.
	if func() bool {
		r.RLock()
		defer r.RUnlock()
		return r.peers[pid]
	}() {
		return
	}

	// Otherwise, ping it and record latency info.
	// Note how we don't want to hold the lock while pinging it.
	latency, err := r.conf.LatencyProbFn(pid)
	if err != nil {
		r.conf.Logger.Errorf("Error probing peer %s", pid)
		return
	}

	r.Lock()
	defer r.Unlock()

	r.metrics.RecordLatency(pid, latency)
	r.peers[pid] = true
}

func (r *ringsRoutingTable) Remove(pid peer.ID) {
	r.Lock()
	defer r.Unlock()
	delete(r.peers, pid)
	for _, ring := range r.rings {
		ring.Remove(pid)
	}
	// TODO: remove the peer from the metrics store too
}

func (r *ringsRoutingTable) Recommend(count int, excludeList []peer.ID) []peer.ID {
	r.RLock()
	defer r.RUnlock()

	// Use a set to make exclusion faster/nicer
	exclude := make(map[peer.ID]bool)
	for _, pid := range excludeList {
		exclude[pid] = true
	}

	// Compute how many nodes we want from each ring
	nodesFromRing := make([]int, r.conf.RingsCount)

	// TODO: if count is less than the number of rings, we actually want to
	// select from rings that are evenly spaced out.  For instance, if count is
	// 3 and we have 9 rings, then we want to select from the 0th, the 3rd, and
	// the 6th ring.
	var j int // index for ring
	for i := 0; i < count; i++ {
		nodesFromRing[j]++
		j++
		if j >= r.conf.RingsCount {
			// Reset index
			j = 0
		}
	}

	var recommended []peer.ID
	for i, count := range nodesFromRing {
		recommended = append(recommended, r.rings[i].Recommend(count, exclude)...)
	}

	// It's possible that some rings are so under-populated that they are not
	// able to provide as many nodes as we requested.  We fill the rest of the
	// recommendation with random samples.
	if len(recommended) < count {
		// Add to the exclude set so we don't recommend a peer twice
		for _, pid := range recommended {
			exclude[pid] = true
		}

		recommended = append(recommended, r.sample(count-len(recommended), exclude)...)
	}

	return recommended
}

// Return a random sample of peers, except those in the `exclude` set
func (r *ringsRoutingTable) sample(count int, exclude map[peer.ID]bool) []peer.ID {
	var peers []peer.ID
	for pid := range r.peers {
		peers = append(peers, pid)
	}

	var sample []peer.ID
	perm := rand.Perm(len(peers))
	for i := 0; i < count && i < len(perm); i++ {
		pid := peers[perm[i]]
		if !exclude[pid] {
			sample = append(sample, pid)
		}
	}
	return sample
}

func (r *ringsRoutingTable) Size() int {
	return len(r.peers)
}

func (r *ringsRoutingTable) Shutdown() {
	close(r.shutdown)
}
