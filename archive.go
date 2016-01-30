// Copyright 2016 Stellar Development Foundation and contributors. Licensed
// under the Apache License, Version 2.0. See the COPYING file at the root
// of this distribution or at http://www.apache.org/licenses/LICENSE-2.0

package archivist

import (
	"io"
	"io/ioutil"
	"path"
	"encoding/json"
	"regexp"
	"strconv"
	"net/url"
	"errors"
	"log"
	"bytes"
	"fmt"
	"sync"
	"strings"
)

const hexPrefixPat = "/[0-9a-f]{2}/[0-9a-f]{2}/[0-9a-f]{2}/"
const rootHASPath = ".well-known/stellar-history.json"
const concurrency = 32

type ConnectOptions struct {
	S3Region string
}

type ArchiveBackend interface {
	GetFile(path string) (io.ReadCloser, error)
	PutFile(path string, in io.ReadCloser) error
	ListFiles(path string) (chan string, error)
}

func Categories() []string {
	return []string{ "history", "ledger", "transactions", "results", "scp"}
}

func categoryExt(n string) string {
	if n == "history" {
		return "json"
	} else {
		return "xdr.gz"
	}
}

func categoryRequired(n string) bool {
	return n != "scp"
}

type Archive struct {
	mutex sync.Mutex
	checkpointFiles map[string](map[uint32]bool)
	allBuckets map[Hash]bool
	referencedBuckets map[Hash]bool
	missingBuckets int
	backend ArchiveBackend
}

func (a *Archive) GetPathHAS(path string) (HistoryArchiveState, error) {
	var has HistoryArchiveState
	rdr, err := a.backend.GetFile(path)
	if err != nil {
		return has, err
	}
	dec := json.NewDecoder(rdr)
	err = dec.Decode(&has)
	return has, err
}

func (a *Archive) PutPathHAS(path string, has HistoryArchiveState) error {
	buf, err := json.MarshalIndent(has, "", "    ")
	if err != nil {
		return err
	}
	return a.backend.PutFile(path,
		ioutil.NopCloser(bytes.NewReader(buf)))
}

func CategoryCheckpointPath(cat string, chk uint32) string {
	ext := categoryExt(cat)
	pre := CheckpointPrefix(chk).Path()
	return path.Join(cat, pre, fmt.Sprintf("%s-%8.8x.%s", cat, chk, ext))
}

func BucketPath(bucket Hash) string {
	pre := HashPrefix(bucket)
	return path.Join("bucket", pre.Path(), fmt.Sprintf("bucket-%s.xdr.gz", bucket))
}

func (a *Archive) GetRootHAS() (HistoryArchiveState, error) {
	return a.GetPathHAS(rootHASPath)
}

func (a *Archive) GetCheckpointHAS(chk uint32) (HistoryArchiveState, error) {
	return a.GetPathHAS(CategoryCheckpointPath("history", chk))
}

func (a *Archive) PutCheckpointHAS(chk uint32, has HistoryArchiveState) error {
	return a.PutPathHAS(CategoryCheckpointPath("history", chk), has)
}

func (a *Archive) PutRootHAS(has HistoryArchiveState) error {
	return a.PutPathHAS(rootHASPath, has)
}

func (a *Archive) ListBucket(dp DirPrefix) (chan string, error) {
	return a.backend.ListFiles(path.Join("bucket", dp.Path()))
}

func (a *Archive) ListAllBuckets() (chan string, error) {
	return a.backend.ListFiles("bucket")
}

func (a *Archive) ListAllBucketHashes() (chan Hash, error) {
	sch, err := a.backend.ListFiles("bucket")
	if err != nil {
		return nil, err
	}
	ch := make(chan Hash, 1000)
	rx := regexp.MustCompile("bucket" + hexPrefixPat + "bucket-([0-9a-f]{64})\\.xdr\\.gz$")
	go func() {
		for s := range sch {
			m := rx.FindStringSubmatch(s)
			if m != nil {
				ch <- MustDecodeHash(m[1])
			}
		}
		close(ch)
	}()
	return ch, nil
}

func (a *Archive) ListCategoryCheckpoints(cat string, pth string) (chan uint32, error) {
	ext := categoryExt(cat)
	rx := regexp.MustCompile(cat + hexPrefixPat + cat +
		"-([0-9a-f]{8})\\." + regexp.QuoteMeta(ext) + "$")
	sch, err := a.backend.ListFiles(path.Join(cat, pth))
	if err != nil {
		return nil, err
	}
	ch := make(chan uint32, 1000)
	go func() {
		for s := range sch {
			m := rx.FindStringSubmatch(s)
			if m != nil {
				i, e := strconv.ParseUint(m[1], 16, 32)
				if e == nil {
					ch <- uint32(i)
				}
			}
		}
		close(ch)
	}()
	return ch, nil
}

func Connect(u string, opts *ConnectOptions) (*Archive, error) {
	arch := Archive{
		checkpointFiles:make(map[string](map[uint32]bool)),
		allBuckets:make(map[Hash]bool),
		referencedBuckets:make(map[Hash]bool),
	}
	for _, cat := range Categories() {
		arch.checkpointFiles[cat] = make(map[uint32]bool)
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return &arch, err
	}
	pth := parsed.Path
	if parsed.Scheme == "s3" {
		// Inside s3, all paths start _without_ the leading /
		if len(pth) > 0 && pth[0] == '/' {
			pth = pth[1:]
		}
		arch.backend = MakeS3Backend(parsed.Host, pth, opts)
	} else if parsed.Scheme == "file" {
		pth = path.Join(parsed.Host, pth)
		arch.backend = MakeFsBackend(pth)
	} else if parsed.Scheme == "mock" {
		arch.backend = MakeMockBackend()
	} else {
		err = errors.New("unknown URL scheme: '" + parsed.Scheme + "'")
	}
	return &arch, err
}

func MustConnect(u string, opts *ConnectOptions) *Archive {
	arch, err := Connect(u, opts)
	if err != nil {
		log.Fatal(err)
	}
	return arch
}

func copyPath(src *Archive, dst *Archive, pth string) error {
	rdr, err := src.backend.GetFile(pth)
	if err != nil {
		return err
	}
	return dst.backend.PutFile(pth, rdr)
}

func Mirror(src *Archive, dst *Archive, rng Range) error {
	rootHAS, e := src.GetRootHAS()
	if e != nil {
		return e
	}

	rng = rng.Clamp(rootHAS.Range())

	log.Printf("copying range %s\n", rng)

	// Make a bucket-fetch map that shows which buckets are
	// already-being-fetched
	bucketFetch := make(map[Hash]bool)
	var bucketFetchMutex sync.Mutex

	tick := make(chan bool)
	go func() {
		k := 0
		sz := rng.Size()
		for range tick {
			k++
			if k & 0xff == 0 {
				bucketFetchMutex.Lock()
				log.Printf("Copied %d/%d checkpoints (%f%%), %d buckets",
					k, sz, 100.0 * float64(k)/float64(sz), len(bucketFetch))
				bucketFetchMutex.Unlock()
			}
		}
	}()


	var wg sync.WaitGroup
	checkpoints := rng.Checkpoints()
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			for {
				ix, ok := <- checkpoints
				if !ok {
					break
				}
				has, e := src.GetCheckpointHAS(ix)
				if e != nil {
					log.Fatal(e)
				}
				for _, bucket := range has.Buckets() {
					alreadyFetching := false
					bucketFetchMutex.Lock()
					_, alreadyFetching = bucketFetch[bucket]
					if !alreadyFetching {
						bucketFetch[bucket] = true
					}
					bucketFetchMutex.Unlock()
					if !alreadyFetching {
						pth := BucketPath(bucket)
						if e = copyPath(src, dst, pth); e != nil {
							log.Fatal(e)
						}
					}
				}
				e = dst.PutCheckpointHAS(ix, has)
				if e != nil {
					log.Fatal(e)
				}
				for _, cat := range Categories() {
					pth := CategoryCheckpointPath(cat, ix)
					if e = copyPath(src, dst, pth); e != nil {
						log.Fatal(e)
					}
				}
				tick <- true
			}
			wg.Done()
		}()
	}

	wg.Wait()
	e = dst.PutRootHAS(rootHAS)
	close(tick)
	return e
}

func Repair(src *Archive, dst *Archive, rng Range) error {
	state, e := dst.GetRootHAS()
	if e != nil {
		return e
	}
	rng = rng.Clamp(state.Range())

	log.Printf("Starting scan for repair")
	e = dst.ScanCheckpoints(rng)
	if e != nil {
		return e
	}

	log.Printf("Examining checkpoint files for gaps")
	missingCheckpointFiles := dst.CheckCheckpointFilesMissing(rng)

	repairedHistory := false
	for cat, missing := range missingCheckpointFiles {
		for _, chk := range missing {
			pth := CategoryCheckpointPath(cat, chk)
			log.Printf("Repairing %s", pth)
			if e = copyPath(src, dst, pth); e != nil {
				log.Fatal(e)
			}
			if cat == "history" {
				repairedHistory = true
			}
		}
	}

	if repairedHistory {
		log.Printf("Re-running checkpoing-file scan, for bucket repair")
		dst.ClearCachedInfo()
		e = dst.ScanCheckpoints(rng)
		if e != nil {
			return e
		}
	}

	e = dst.ScanBuckets()
	if e != nil {
		return e
	}

	log.Printf("Examining buckets referenced by checkpoints")
	missingBuckets := dst.CheckBucketsMissing()


	for bkt, _ := range missingBuckets {
		pth := BucketPath(bkt)
		log.Printf("Repairing %s", pth)
		if e = copyPath(src, dst, pth); e != nil {
			log.Fatal(e)
		}
	}

	return nil
}

func (arch* Archive) ClearCachedInfo() {
	arch.mutex.Lock()
	defer arch.mutex.Unlock()
	for _, cat := range Categories() {
		arch.checkpointFiles[cat] = make(map[uint32]bool)
	}
	arch.allBuckets = make(map[Hash]bool)
	arch.referencedBuckets = make(map[Hash]bool)
}

func (arch* Archive) ReportCheckpointStats() {
	arch.mutex.Lock()
	defer arch.mutex.Unlock()
	s := make([]string, 0)
	for _, cat := range Categories() {
		tab := arch.checkpointFiles[cat]
		s = append(s, fmt.Sprintf("%d %s", len(tab), cat))
	}
	log.Printf("Archive: %s", strings.Join(s, ", "))
}

func (arch* Archive) ReportBucketStats() {
	arch.mutex.Lock()
	defer arch.mutex.Unlock()
	log.Printf("Archive: %d buckets total, %d referenced",
		len(arch.allBuckets), len(arch.referencedBuckets))
}

func (arch *Archive) NoteCheckpointFile(cat string, chk uint32, present bool) {
	arch.mutex.Lock()
	defer arch.mutex.Unlock()
	arch.checkpointFiles[cat][chk] = present
}

func (arch *Archive) NoteExistingBucket(bucket Hash) {
	arch.mutex.Lock()
	defer arch.mutex.Unlock()
	arch.allBuckets[bucket] = true
}

func (arch *Archive) NoteReferencedBucket(bucket Hash) {
	arch.mutex.Lock()
	defer arch.mutex.Unlock()
	arch.referencedBuckets[bucket] = true
}

type scanCheckpointReq struct {
	category string
	pathprefix string
}

func (arch *Archive) ScanCheckpoints(rng Range) error {
	state, e := arch.GetRootHAS()
	if e != nil {
		return e
	}
	rng = rng.Clamp(state.Range())

	log.Printf("Scanning checkpoint files in range: %s", rng)

	errs := make(chan error, 10000)
	tick := make(chan bool)
	go func() {
		k := 0
		for range tick {
			k++
			if k & 0xfff == 0 {
				arch.ReportCheckpointStats()
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(concurrency)

	req := make(chan scanCheckpointReq)

	cats := Categories()
	go func() {
		for _, cat := range cats {
			for _, pth := range RangePaths(rng) {
				req <- scanCheckpointReq{category:cat, pathprefix:pth}
			}
		}
		close(req)
	}()

	for i := 0; i < concurrency; i++ {
		go func() {
			for {
				r, ok := <-req
				if !ok {
					break
				}
				ch, e := arch.ListCategoryCheckpoints(r.category, r.pathprefix)
				errs <- e
				for n := range ch {
					tick <- true
					arch.NoteCheckpointFile(r.category, n, true)
				}
			}
			wg.Done()
		}()
	}

	wg.Wait()
	close(tick)
	log.Printf("Checkpoint files scanned")
	close(errs)
	arch.ReportCheckpointStats()
	for e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (arch *Archive) Scan(rng Range) error {
	e := arch.ScanCheckpoints(rng)
	if e != nil {
		return e
	}
	return arch.ScanBuckets()
}

func (arch *Archive) CheckCheckpointFilesMissing(rng Range) map[string][]uint32 {
	arch.mutex.Lock()
	defer arch.mutex.Unlock()
	missing := make(map[string][]uint32)
	for _, cat := range Categories() {
		missing[cat] = make([]uint32, 0)
		for ix := range rng.Checkpoints() {
			_, ok := arch.checkpointFiles[cat][ix]
			if !ok {
				missing[cat] = append(missing[cat], ix)
			}
		}
	}
	return missing
}


func (arch* Archive) CheckBucketsMissing() map[Hash]bool {
	arch.mutex.Lock()
	defer arch.mutex.Unlock()
	missing := make(map[Hash]bool)
	for k, _ := range arch.referencedBuckets {
		_, ok := arch.allBuckets[k]
		if !ok {
			missing[k] = true
		}
	}
	return missing
}

func (arch *Archive) ScanBuckets() error {

	// Extract the set of checkpoints we have HASs for, to scan.
	arch.mutex.Lock()
	hists := arch.checkpointFiles["history"]
	seqs := make([]uint32, 0, len(hists))
	for k, present := range hists {
		if present {
			seqs = append(seqs, k)
		}
	}
	arch.mutex.Unlock()

	log.Printf("Scanning all buckets, and those referenced by range")

	// We're going to wait on 1 GR for all-bucket-listing +
	// 'concurrency' GRs for reading HAS files for referenced buckets
	var wg sync.WaitGroup
	wg.Add(concurrency + 1)

	tick := make(chan bool)
	go func() {
		k := 0
		for range tick {
			k++
			if k & 0xfff == 0 {
				arch.ReportBucketStats()
			}
		}
	}()

	// Start a goroutine listing all the buckets in the archive.
	// This is lengthy, but it's generally much faster than
	// doing thousands of individual bucket probes.
	allBuckets, e := arch.ListAllBucketHashes()
	if e != nil {
		close(tick)
		return e
	}
	go func() {
		for b := range allBuckets {
			arch.NoteExistingBucket(b)
			tick <- true
		}
		wg.Done()
	}()


	// Make a bunch of goroutines that pull each HAS and enumerate
	// its buckets into a channel. These are the _referenced_ buckets.
	req := make(chan uint32)
	go func() {
		for _, seq := range seqs {
			req <- seq
		}
		close(req)
	}()
	for i := 0; i < concurrency; i++ {
		go func() {
			for {
				ix, ok := <- req
				if !ok {
					break
				}
				has, e := arch.GetCheckpointHAS(ix)
				if e != nil {
					log.Fatal(e)
				}
				for _, bucket := range has.Buckets() {
					arch.NoteReferencedBucket(bucket)
				}
				tick <- true
			}
			wg.Done()
		}()
	}

	wg.Wait()
	arch.ReportBucketStats()
	close(tick)
	return nil
}

func (arch *Archive) ReportMissing(rng Range) error {

	state, e := arch.GetRootHAS()
	if e != nil {
		return e
	}
	rng = rng.Clamp(state.Range())

	log.Printf("Examining checkpoint files for gaps")
	missingCheckpointFiles := arch.CheckCheckpointFilesMissing(rng)
	log.Printf("Examining buckets referenced by checkpoints")
	missingBuckets := arch.CheckBucketsMissing()

	missingCheckpoints := false
	for cat, missing := range missingCheckpointFiles {
		s := make([]string, 0)
		if !categoryRequired(cat) {
			continue
		}
		for _, m := range missing {
			s = append(s, fmt.Sprintf("0x%8.8x", m))
		}
		if len(missing) != 0 {
			missingCheckpoints = true
			log.Printf("Missing %s: %s", cat, strings.Join(s, ", "))
		}
	}

	if !missingCheckpoints {
		log.Printf("No checkpoint files missing in range %s", rng)
	}

	for bucket, _ := range missingBuckets {
		log.Printf("Missing bucket: %s", bucket)
	}

	if len(missingBuckets) == 0 {
		log.Printf("No missing buckets referenced in range %s", rng)
	}

	return nil
}