package renterutil

import (
	"strings"
	"sync"

	"github.com/pkg/errors"
	"gitlab.com/NebulousLabs/Sia/types"
	"lukechampine.com/us/hostdb"
	"lukechampine.com/us/renter"
	"lukechampine.com/us/renter/proto"
)

var errNoHost = errors.New("no record of that host")

type lockedHost struct {
	s   *proto.Session
	err error
	mu  *sync.Mutex
}

// A HostSet is a collection of renter-host protocol sessions.
type HostSet struct {
	sessions      map[hostdb.HostPublicKey]lockedHost
	hkr           renter.HostKeyResolver
	currentHeight types.BlockHeight
}

// Close closes all of the Downloaders in the set.
func (set *HostSet) Close() error {
	for _, ls := range set.sessions {
		ls.mu.Lock()
		if ls.s != nil {
			ls.s.Close()
		}
	}
	return nil
}

func (set *HostSet) acquire(host hostdb.HostPublicKey) (*proto.Session, error) {
	ls, ok := set.sessions[host]
	if !ok {
		return nil, errNoHost
	}
	ls.mu.Lock()
	if err := ls.err; err != nil {
		ls.mu.Unlock()
		return nil, err
	}
	return ls.s, nil
}

func (set *HostSet) release(host hostdb.HostPublicKey) {
	set.sessions[host].mu.Unlock()
}

// AddHost adds a host to the set, using the provided contract to establish a
// protocol session. If a session cannot be established, the error is returned,
// but the host is still added to the set, and the error is exposed via the
// acquire method.
func (set *HostSet) AddHost(c proto.ContractEditor) error {
	hostKey := c.Revision().HostKey()
	var s *proto.Session
	hostIP, err := set.hkr.ResolveHostKey(hostKey)
	if err != nil {
		err = errors.Wrapf(err, "%v: could not resolve host key", hostKey.ShortKey())
	} else {
		s, err = proto.NewSession(hostIP, c, set.currentHeight)
		err = errors.Wrapf(err, "%v", hostKey.ShortKey())
	}
	set.sessions[hostKey] = lockedHost{s: s, err: err, mu: new(sync.Mutex)}
	return err
}

// NewHostSet creates an empty HostSet using the provided resolver and current
// height.
func NewHostSet(hkr renter.HostKeyResolver, currentHeight types.BlockHeight) *HostSet {
	return &HostSet{
		hkr:           hkr,
		currentHeight: currentHeight,
		sessions:      make(map[hostdb.HostPublicKey]lockedHost),
	}
}

// DownloadChunkShards downloads the shards of chunkIndex from hosts in
// parallel. shardLen is the length of the first non-nil shard.
//
// The shards returned by DownloadChunkShards are only valid until the next
// call to Sector on the shard's corresponding proto.Downloader.
func DownloadChunkShards(hosts []*renter.ShardDownloader, chunkIndex int64, minShards int, cancel <-chan struct{}) (shards [][]byte, shardLen int, stats []DownloadStatsUpdate, err error) {
	errNoHost := errors.New("no downloader for this host")
	type result struct {
		shardIndex int
		shard      []byte
		stats      DownloadStatsUpdate
		err        error
	}
	// spawn minShards goroutines that receive download requests from
	// reqChan and send responses to resChan.
	reqChan := make(chan int, minShards)
	resChan := make(chan result, minShards)
	var wg sync.WaitGroup
	reqIndex := 0
	for ; reqIndex < minShards; reqIndex++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for shardIndex := range reqChan {
				res := result{shardIndex: shardIndex}
				host := hosts[shardIndex]
				if host == nil {
					res.err = errNoHost
				} else {
					res.shard, res.err = host.DownloadAndDecrypt(chunkIndex)
					res.err = errors.Wrap(res.err, host.HostKey().ShortKey())
					res.stats = DownloadStatsUpdate{
						Host:  host.HostKey(),
						Stats: host.Downloader.LastDownloadStats(),
					}
				}
				resChan <- res
			}
		}()
		// prepopulate reqChan with first minShards shards
		reqChan <- reqIndex
	}
	// make sure all goroutines exit before returning
	defer func() {
		close(reqChan)
		wg.Wait()
	}()

	// collect the results of each shard download, appending successful
	// downloads to goodRes and failed downloads to badRes. If a download
	// fails, send the next untried shard index. Break as soon as we have
	// minShards successful downloads or if the number of failures makes it
	// impossible to recover the chunk.
	var goodRes, badRes []result
	for len(goodRes) < minShards && len(badRes) <= len(hosts)-minShards {
		select {
		case <-cancel:
			return nil, 0, nil, ErrCanceled

		case res := <-resChan:
			if res.err == nil {
				goodRes = append(goodRes, res)
			} else {
				badRes = append(badRes, res)
				if reqIndex < len(hosts) {
					reqChan <- reqIndex
					reqIndex++
				}
			}
		}
	}
	if len(goodRes) < minShards {
		var errStrings []string
		for _, r := range badRes {
			if r.err != errNoHost {
				errStrings = append(errStrings, r.err.Error())
			}
		}
		return nil, 0, nil, errors.New("too many hosts did not supply their shard:\n" + strings.Join(errStrings, "\n"))
	}

	shards = make([][]byte, len(hosts))
	stats = make([]DownloadStatsUpdate, 0, len(goodRes))
	for _, r := range goodRes {
		shards[r.shardIndex] = r.shard
		stats = append(stats, r.stats)
	}

	// determine shardLen
	for _, s := range shards {
		if len(s) > 0 {
			shardLen = len(s)
			break
		}
	}

	// allocate space for missing shards, in case the caller wants to
	// reconstruct them
	for i := range shards {
		if len(shards[i]) == 0 {
			shards[i] = make([]byte, 0, shardLen)
		}
	}

	return shards, shardLen, stats, nil
}
