package memdb

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/t3rm1n4l/memdb/skiplist"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"path"
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

var (
	ErrMaxSnapshotsLimitReached = fmt.Errorf("Maximum snapshots limit reached")
)

type KeyCompare func([]byte, []byte) int

type VisitorCallback func(*Item, int) error

type ItemEntry struct {
	itm *Item
	n   *skiplist.Node
}

func (e *ItemEntry) Item() *Item {
	return e.itm
}

func (e *ItemEntry) Node() *skiplist.Node {
	return e.n
}

type ItemCallback func(*ItemEntry)

type FileType int

const (
	encodeBufSize = 4
	readerBufSize = 10000
)

const (
	ForestdbFile FileType = iota
	RawdbFile
)

const gcchanBufSize = 256

var (
	dbInstances      *skiplist.Skiplist
	dbInstancesCount int64
)

func init() {
	dbInstances = skiplist.New()
}

func CompareMemDB(this unsafe.Pointer, that unsafe.Pointer) int {
	thisItem := (*MemDB)(this)
	thatItem := (*MemDB)(that)

	return int(thisItem.id - thatItem.id)
}

func DefaultConfig() Config {
	var cfg Config
	cfg.SetKeyComparator(defaultKeyCmp)
	cfg.SetFileType(RawdbFile)
	return cfg
}

func newInsertCompare(keyCmp KeyCompare) skiplist.CompareFn {
	return func(this, that unsafe.Pointer) int {
		var v int
		thisItem := (*Item)(this)
		thatItem := (*Item)(that)
		if v = keyCmp(thisItem.Bytes(), thatItem.Bytes()); v == 0 {
			v = int(thisItem.bornSn) - int(thatItem.bornSn)
		}

		return v
	}
}

func newIterCompare(keyCmp KeyCompare) skiplist.CompareFn {
	return func(this, that unsafe.Pointer) int {
		thisItem := (*Item)(this)
		thatItem := (*Item)(that)
		return keyCmp(thisItem.Bytes(), thatItem.Bytes())
	}
}

func newExistCompare(keyCmp KeyCompare) skiplist.CompareFn {
	return func(this, that unsafe.Pointer) int {
		thisItem := (*Item)(this)
		thatItem := (*Item)(that)
		if thisItem.deadSn != 0 || thatItem.deadSn != 0 {
			return 1
		}
		return keyCmp(thisItem.Bytes(), thatItem.Bytes())
	}
}

func defaultKeyCmp(this, that []byte) int {
	return bytes.Compare(this, that)
}

//
//compare item,sn
type Writer struct {
	rand   *rand.Rand
	buf    *skiplist.ActionBuffer
	iter   *skiplist.Iterator
	gchead *skiplist.Node
	gctail *skiplist.Node
	next   *Writer
	*MemDB
}

func (w *Writer) Put(x *Item) {
	w.Put2(x)
}

func (w *Writer) Put2(x *Item) (n *skiplist.Node) {
	var success bool
	x.bornSn = w.getCurrSn()
	n, success = w.store.Insert2(unsafe.Pointer(x), w.insCmp, w.existCmp, w.buf, w.rand.Float32)
	if success {
		atomic.AddInt64(&w.count, 1)
	}
	return
}

// Find first item, seek until dead=0, mark dead=sn
func (w *Writer) Delete(x *Item) (success bool) {
	_, success = w.Delete2(x)
	return
}

func (w *Writer) Delete2(x *Item) (n *skiplist.Node, success bool) {
	n = w.GetNode(x)
	if n != nil {
		success = w.DeleteNode(n)
	}

	return
}

func (w *Writer) DeleteNode(x *skiplist.Node) (success bool) {
	defer func() {
		if success {
			atomic.AddInt64(&w.count, -1)
		}
	}()

	sn := w.getCurrSn()
	gotItem := (*Item)(x.Item())
	if gotItem.bornSn == sn {
		success = w.store.DeleteNode(x, w.insCmp, w.buf)
		return
	}

	success = atomic.CompareAndSwapUint32(&gotItem.deadSn, 0, sn)
	if success {
		if w.gctail == nil {
			w.gctail = x
			w.gchead = w.gctail
		} else {
			w.gctail.GClink = x
			w.gctail = x
		}
	}
	return
}

func (w *Writer) Get(x *Item) *Item {
	n := w.GetNode(x)
	if n != nil {
		return (*Item)(n.Item())
	}
	return nil
}

func (w *Writer) GetNode(x *Item) *skiplist.Node {
	var curr *skiplist.Node
	found := w.iter.Seek(unsafe.Pointer(x))
	if !found {
		return nil
	}

	// Seek until most recent item for key is found
	curr = w.iter.GetNode()
	for {
		w.iter.Next()
		if !w.iter.Valid() {
			break
		}
		next := w.iter.GetNode()
		nxtItm := next.Item()
		currItm := curr.Item()
		if w.iterCmp(nxtItm, currItm) != 0 {
			break
		}

		curr = next
	}

	currItm := (*Item)(curr.Item())
	if currItm.deadSn != 0 {
		return nil
	}

	return curr
}

type Config struct {
	keyCmp   KeyCompare
	insCmp   skiplist.CompareFn
	iterCmp  skiplist.CompareFn
	existCmp skiplist.CompareFn

	ignoreItemSize bool

	fileType FileType
}

func (cfg *Config) SetKeyComparator(cmp KeyCompare) {
	cfg.keyCmp = cmp
	cfg.insCmp = newInsertCompare(cmp)
	cfg.iterCmp = newIterCompare(cmp)
	cfg.existCmp = newExistCompare(cmp)
}

func (cfg *Config) SetFileType(t FileType) error {
	switch t {
	case ForestdbFile, RawdbFile:
	default:
		return errors.New("Invalid format")
	}

	cfg.fileType = t
	return nil
}

func (cfg *Config) IgnoreItemSize() {
	cfg.ignoreItemSize = true
}

type MemDB struct {
	id           int
	store        *skiplist.Skiplist
	currSn       uint32
	snapshots    *skiplist.Skiplist
	gcsnapshots  *skiplist.Skiplist
	isGCRunning  int32
	lastGCSn     uint32
	leastUnrefSn uint32
	count        int64

	wlist  *Writer
	gcchan chan *skiplist.Node

	Config
}

func NewWithConfig(cfg Config) *MemDB {
	m := &MemDB{
		store:       skiplist.New(),
		snapshots:   skiplist.New(),
		gcsnapshots: skiplist.New(),
		currSn:      1,
		Config:      cfg,
		gcchan:      make(chan *skiplist.Node, gcchanBufSize),
		id:          int(atomic.AddInt64(&dbInstancesCount, 1)),
	}

	m.initSizeFuns()
	buf := dbInstances.MakeBuf()
	defer dbInstances.FreeBuf(buf)
	dbInstances.Insert(unsafe.Pointer(m), CompareMemDB, buf)

	return m

}

func (m *MemDB) initSizeFuns() {
	m.snapshots.SetItemSizeFunc(SnapshotSize)
	m.gcsnapshots.SetItemSizeFunc(SnapshotSize)
	if !m.ignoreItemSize {
		m.store.SetItemSizeFunc(ItemSize)
	}
}

func New() *MemDB {
	return NewWithConfig(DefaultConfig())
}

func (m *MemDB) MemoryInUse() int64 {
	return m.store.MemoryInUse() + m.snapshots.MemoryInUse() + m.gcsnapshots.MemoryInUse()
}

func (m *MemDB) Close() {
	close(m.gcchan)
	buf := dbInstances.MakeBuf()
	defer dbInstances.FreeBuf(buf)
	dbInstances.Delete(unsafe.Pointer(m), CompareMemDB, buf)
}

func (m *MemDB) getCurrSn() uint32 {
	return atomic.LoadUint32(&m.currSn)
}

func (m *MemDB) setLeastUnrefSn() {
	buf := m.snapshots.MakeBuf()
	defer m.snapshots.FreeBuf(buf)
	iter := m.snapshots.NewIterator(CompareSnapshot, buf)
	iter.SeekFirst()
	if iter.Valid() {
		snap := (*Snapshot)(iter.Get())
		atomic.StoreUint32(&m.leastUnrefSn, snap.sn-1)
	}
}

func (m *MemDB) getLeastUnrefSn() uint32 {
	return atomic.LoadUint32(&m.leastUnrefSn)
}

func (m *MemDB) NewWriter() *Writer {
	buf := m.store.MakeBuf()

	w := &Writer{
		rand:  rand.New(rand.NewSource(int64(rand.Int()))),
		buf:   buf,
		iter:  m.store.NewIterator(m.iterCmp, buf),
		next:  m.wlist,
		MemDB: m,
	}

	m.wlist = w

	go m.collectionWorker()

	return w
}

type Snapshot struct {
	sn       uint32
	refCount int32
	db       *MemDB
	count    int64

	gclist *skiplist.Node
}

func SnapshotSize(p unsafe.Pointer) int {
	s := (*Snapshot)(p)
	return int(unsafe.Sizeof(s.sn) + unsafe.Sizeof(s.refCount) + unsafe.Sizeof(s.db) +
		unsafe.Sizeof(s.count) + unsafe.Sizeof(s.gclist))
}

func (s Snapshot) Count() int64 {
	return s.count
}

func (s *Snapshot) Encode(buf []byte, w io.Writer) error {
	l := 4
	if len(buf) < l {
		return ErrNotEnoughSpace
	}

	binary.BigEndian.PutUint32(buf[0:4], s.sn)
	if _, err := w.Write(buf[0:4]); err != nil {
		return err
	}

	return nil

}

func (s *Snapshot) Decode(buf []byte, r io.Reader) error {
	if _, err := io.ReadFull(r, buf[0:4]); err != nil {
		return err
	}
	s.sn = binary.BigEndian.Uint32(buf[0:4])
	return nil
}

func (s *Snapshot) Open() bool {
	if atomic.LoadInt32(&s.refCount) == 0 {
		return false
	}
	atomic.AddInt32(&s.refCount, 1)
	return true
}

func (s *Snapshot) Close() {
	newRefcount := atomic.AddInt32(&s.refCount, -1)
	if newRefcount == 0 {
		buf := s.db.snapshots.MakeBuf()
		defer s.db.snapshots.FreeBuf(buf)

		// Move from live snapshot list to dead list
		s.db.snapshots.Delete(unsafe.Pointer(s), CompareSnapshot, buf)
		s.db.gcsnapshots.Insert(unsafe.Pointer(s), CompareSnapshot, buf)
		s.db.setLeastUnrefSn()
		if atomic.CompareAndSwapInt32(&s.db.isGCRunning, 0, 1) {
			go s.db.GC()
		}
	}
}

func (s *Snapshot) NewIterator() *Iterator {
	return s.db.NewIterator(s)
}

func CompareSnapshot(this, that unsafe.Pointer) int {
	thisItem := (*Snapshot)(this)
	thatItem := (*Snapshot)(that)

	return int(thisItem.sn) - int(thatItem.sn)
}

func (m *MemDB) NewSnapshot() (*Snapshot, error) {
	buf := m.snapshots.MakeBuf()
	defer m.snapshots.FreeBuf(buf)

	snap := &Snapshot{db: m, sn: m.getCurrSn(), refCount: 1, count: m.ItemsCount()}
	m.snapshots.Insert(unsafe.Pointer(snap), CompareSnapshot, buf)
	newSn := atomic.AddUint32(&m.currSn, 1)
	if newSn == math.MaxUint32 {
		return nil, ErrMaxSnapshotsLimitReached
	}

	// Stitch all local gclists from all writers to create snapshot gclist
	var head, tail *skiplist.Node

	for w := m.wlist; w != nil; w = w.next {
		if tail == nil {
			head = w.gchead
			tail = w.gctail
		} else if w.gchead != nil {
			tail.GClink = w.gchead
			tail = w.gctail
		}

		w.gchead = nil
		w.gctail = nil
	}

	snap.gclist = head

	return snap, nil
}

type Iterator struct {
	snap *Snapshot
	iter *skiplist.Iterator
	buf  *skiplist.ActionBuffer
}

func (it *Iterator) skipUnwanted() {
loop:
	if !it.iter.Valid() {
		return
	}
	itm := (*Item)(it.iter.Get())
	if itm.bornSn > it.snap.sn || (itm.deadSn > 0 && itm.deadSn <= it.snap.sn) {
		it.iter.Next()
		goto loop
	}
}

func (it *Iterator) SeekFirst() {
	it.iter.SeekFirst()
	it.skipUnwanted()
}

func (it *Iterator) Seek(itm *Item) {
	it.iter.Seek(unsafe.Pointer(itm))
	it.skipUnwanted()
}

func (it *Iterator) Valid() bool {
	return it.iter.Valid()
}

func (it *Iterator) Get() *Item {
	return (*Item)(it.iter.Get())
}

func (it *Iterator) GetNode() *skiplist.Node {
	return it.iter.GetNode()
}

func (it *Iterator) Next() {
	it.iter.Next()
	it.skipUnwanted()
}

func (it *Iterator) Close() {
	it.snap.Close()
	it.snap.db.store.FreeBuf(it.buf)
}

func (m *MemDB) NewIterator(snap *Snapshot) *Iterator {
	if !snap.Open() {
		return nil
	}
	buf := snap.db.store.MakeBuf()
	return &Iterator{
		snap: snap,
		iter: m.store.NewIterator(m.iterCmp, buf),
		buf:  buf,
	}
}

func (m *MemDB) ItemsCount() int64 {
	return atomic.LoadInt64(&m.count)
}

func (m *MemDB) collectionWorker() {
	buf := m.store.MakeBuf()
	defer m.store.FreeBuf(buf)

	for gclist := range m.gcchan {
		for n := gclist; n != nil; n = n.GClink {
			m.store.DeleteNode(n, m.insCmp, buf)
		}
	}
}

func (m *MemDB) collectDead(sn uint32) {
	buf1 := m.snapshots.MakeBuf()
	buf2 := m.snapshots.MakeBuf()
	defer m.snapshots.FreeBuf(buf1)
	defer m.snapshots.FreeBuf(buf2)
	iter := m.gcsnapshots.NewIterator(CompareSnapshot, buf1)
	iter.SeekFirst()
	for ; iter.Valid(); iter.Next() {
		node := iter.GetNode()
		sn := (*Snapshot)(node.Item())
		if sn.sn > m.getLeastUnrefSn() {
			return
		}

		m.gcchan <- sn.gclist
		m.gcsnapshots.DeleteNode(node, CompareSnapshot, buf2)
	}
}

func (m *MemDB) GC() {
	buf := m.snapshots.MakeBuf()
	defer m.snapshots.FreeBuf(buf)

	sn := m.getLeastUnrefSn()
	if sn != m.lastGCSn && sn > 0 {
		m.lastGCSn = sn
		m.collectDead(m.lastGCSn)
	}

	atomic.CompareAndSwapInt32(&m.isGCRunning, 1, 0)
}

func (m *MemDB) GetSnapshots() []*Snapshot {
	var snaps []*Snapshot
	buf := m.snapshots.MakeBuf()
	defer m.snapshots.FreeBuf(buf)
	iter := m.snapshots.NewIterator(CompareSnapshot, buf)
	iter.SeekFirst()
	for ; iter.Valid(); iter.Next() {
		snaps = append(snaps, (*Snapshot)(iter.Get()))
	}

	return snaps
}

func (m *MemDB) Visitor(snap *Snapshot, callb VisitorCallback, shards int, concurrency int) error {
	var wg sync.WaitGroup

	var iters []*Iterator
	var lastNodes []*skiplist.Node

	wch := make(chan int)

	iters = append(iters, m.NewIterator(snap))
	iters[0].SeekFirst()
	pivots := m.store.GetRangeSplitItems(shards)
	for _, p := range pivots {
		iter := m.NewIterator(snap)
		iter.Seek((*Item)(p))

		if iter.Valid() && (len(lastNodes) == 0 || iter.GetNode() != lastNodes[len(lastNodes)-1]) {
			iters = append(iters, iter)
			lastNodes = append(lastNodes, iter.GetNode())
		} else {
			iter.Close()
		}
	}

	lastNodes = append(lastNodes, nil)
	errors := make([]error, len(iters))

	// Run workers
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(wg *sync.WaitGroup) {
			defer wg.Done()

			for shard := range wch {
			loop:
				for itr := iters[shard]; itr.Valid(); itr.Next() {
					if itr.GetNode() == lastNodes[shard] {
						break loop
					}
					if err := callb(itr.Get(), shard); err != nil {
						errors[shard] = err
						return
					}
				}
			}
		}(&wg)
	}

	// Provide work and wait
	for shard := 0; shard < len(iters); shard++ {
		wch <- shard
	}
	close(wch)

	wg.Wait()

	for _, itr := range iters {
		itr.Close()
	}

	for _, err := range errors {
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *MemDB) StoreToDisk(dir string, snap *Snapshot, concurr int, itmCallback ItemCallback) error {
	var err error
	datadir := path.Join(dir, "data")
	os.MkdirAll(datadir, 0755)
	shards := runtime.NumCPU()

	writers := make([]FileWriter, shards)
	files := make([]string, shards)
	defer func() {
		for _, w := range writers {
			if w != nil {
				w.Close()
			}
		}
	}()

	for shard := 0; shard < shards; shard++ {
		w := newFileWriter(m.fileType)
		file := fmt.Sprintf("shard-%d", shard)
		datafile := path.Join(datadir, file)
		if err := w.Open(datafile); err != nil {
			return err
		}

		writers[shard] = w
		files[shard] = file
	}

	visitorCallback := func(itm *Item, shard int) error {
		w := writers[shard]
		if err := w.WriteItem(itm); err != nil {
			return err
		}

		if itmCallback != nil {
			itmCallback(&ItemEntry{itm: itm, n: nil})
		}

		return nil
	}

	if err = m.Visitor(snap, visitorCallback, shards, concurr); err == nil {
		bs, _ := json.Marshal(files)
		ioutil.WriteFile(path.Join(datadir, "files.json"), bs, 0660)
	}

	return err
}

func (m *MemDB) LoadFromDisk(dir string, concurr int, callb ItemCallback) (*Snapshot, error) {
	var wg sync.WaitGroup
	datadir := path.Join(dir, "data")
	var files []string

	if bs, err := ioutil.ReadFile(path.Join(datadir, "files.json")); err != nil {
		return nil, err
	} else {
		json.Unmarshal(bs, &files)
	}

	var nodeCallb skiplist.NodeCallback
	wchan := make(chan int)
	b := skiplist.NewBuilder()
	segments := make([]*skiplist.Segment, len(files))
	readers := make([]FileReader, len(files))
	errors := make([]error, len(files))

	if callb != nil {
		nodeCallb = func(n *skiplist.Node) {
			callb(&ItemEntry{itm: (*Item)(n.Item()), n: n})
		}
	}

	defer func() {
		for _, r := range readers {
			if r != nil {
				r.Close()
			}
		}
	}()

	for i, file := range files {
		segments[i] = b.NewSegment()
		segments[i].SetNodeCallback(nodeCallb)
		r := newFileReader(m.fileType)
		datafile := path.Join(datadir, file)
		if err := r.Open(datafile); err != nil {
			return nil, err
		}

		readers[i] = r
	}

	for i := 0; i < concurr; i++ {
		wg.Add(1)
		go func(wg *sync.WaitGroup) {
			defer wg.Done()

			for shard := range wchan {
				r := readers[shard]
			loop:
				for {
					itm, err := r.ReadItem()
					if err != nil {
						errors[shard] = err
						return
					}

					if itm == nil {
						break loop
					}
					segments[shard].Add(unsafe.Pointer(itm))
				}
			}
		}(&wg)
	}

	for i, _ := range files {
		wchan <- i
	}
	close(wchan)
	wg.Wait()

	for _, err := range errors {
		if err != nil {
			return nil, err
		}
	}

	m.store = b.Assemble(segments...)
	stats := m.store.GetStats()
	m.count = int64(stats.NodeCount)
	snap, _ := m.NewSnapshot()
	return snap, nil
}

func (m *MemDB) DumpStats() string {
	return m.store.GetStats().String()
}

func MemoryInUse() (sz int64) {
	buf := dbInstances.MakeBuf()
	defer dbInstances.FreeBuf(buf)
	iter := dbInstances.NewIterator(CompareMemDB, buf)
	for iter.SeekFirst(); iter.Valid(); iter.Next() {
		db := (*MemDB)(iter.Get())
		sz += db.MemoryInUse()
	}

	return
}
