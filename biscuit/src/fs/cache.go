package fs

//import "fmt"
import "sync"
import "sync/atomic"

import "common"
import "hashtable"

// Fixed-size cache of objects. Main invariant: an object is in memory once so
// that threads see each other's updates.  The challenging case is that an
// object can be evicted only when no thread has a reference to the object.  To
// keep track of the references to an object, cache refcounts the references to
// an object.  The client of cache, must call Lookup/Done to ensure a correct
// refcount.
//
// It is a bummer that we refcnt, instead of relying on GC. n an alternate
// world, we would use finalizers on an object, and the GC would inform
// refcache_t that an object isn't in use anymore.  Refcache itself would use a
// weak reference to an object, so that the GC could collect the object, if it
// is low on memory.

type cstats_t struct {
	Nevict common.Counter_t
	Nhit   common.Counter_t
	Nadd   common.Counter_t
}

type Obj_t interface {
	Evict()
}

type Objref_t struct {
	Key     int
	Obj     Obj_t
	refcnt  int64
	Refnext *Objref_t
	Refprev *Objref_t
}

func MkObjref(obj Obj_t, key int) *Objref_t {
	e := &Objref_t{}
	e.Obj = obj
	e.Key = key
	e.refcnt = 1
	return e
}

func (ref *Objref_t) Refcnt() int64 {
	c := atomic.LoadInt64(&ref.refcnt)
	return c
}

func (ref *Objref_t) Up() {
	atomic.AddInt64(&ref.refcnt, 1)
}

func (ref *Objref_t) Down() int64 {
	v := atomic.AddInt64(&ref.refcnt, -1)
	if v < 0 {
		panic("Down")
	}
	return v
}

type cache_t struct {
	sync.Mutex
	cache     *hashtable.Hashtable_t
	objreflru objreflru_t
	stats     cstats_t
}

func mkCache(size int) *cache_t {
	c := &cache_t{}
	c.cache = hashtable.MkHash(size)
	return c
}

func (c *cache_t) Len() int {
	c.Lock()
	ret := 0 // len(c.cache)
	c.Unlock()
	return ret
}

func (c *cache_t) Lookup(key int, mkobj func(int) Obj_t) (*Objref_t, bool) {
	c.Lock()
	v, ok := c.cache.Get(key)
	if ok {
		e := v.(*Objref_t)
		// other threads may have a reference to this item and may
		// Ref{up,down}; the increment must therefore be atomic
		c.stats.Nhit.Inc()
		e.Up()
		c.objreflru.mkhead(e)
		c.Unlock()
		return e, false
	}
	e := MkObjref(mkobj(key), key)
	e.Obj = mkobj(key)
	c.cache.Put(key, e)
	c.objreflru.mkhead(e)
	c.stats.Nadd.Inc()
	c.Unlock()
	return e, true
}

func (c *cache_t) Remove(key int) {
	c.Lock()
	if v, ok := c.cache.Get(key); ok {
		e := v.(*Objref_t)
		cnt := e.Refcnt()
		if cnt < 0 {
			panic("Remove: negative refcnt")
		}
		if cnt == 0 {
			c.delete(e)
		} else {
			panic("Remove: refcnt > 0")
		}
	} else {
		panic("Remove: non existing")
	}
	c.Unlock()
}

func (c *cache_t) Stats() string {
	s := ""
	s += common.Stats2String(c.stats)
	return s
}

// evicts up-to half of the objects in the cache. returns the number of cache
// entries remaining.
func (c *cache_t) Evict_half() int {
	c.Lock()
	defer c.Unlock()

	upto := 0 // XXX len(c.cache)
	did := 0
	var back *Objref_t
	for p := c.objreflru.tail; p != nil && did < upto; p = back {
		back = p.Refprev
		if p.Refcnt() != 0 {
			continue
		}
		// imemnode with refcount of 0 must have non-zero links and
		// thus can be freed.  (in fact, they already have been freed)
		c.delete(p)
		// imemnode eviction acquires no locks and block eviction
		// acquires only a leaf lock (physmem lock). furthermore,
		// neither eviction blocks on IO, thus it is safe to evict here
		// with locks held.
		p.Obj.Evict()
		did++
	}
	return 0 // XXX len(c.cache)
}

func (c *cache_t) delete(o *Objref_t) {
	c.cache.Del(o.Key)
	c.objreflru.remove(o)
	c.stats.Nevict.Inc()
}

// LRU list of references
type objreflru_t struct {
	head *Objref_t
	tail *Objref_t
}

func (rl *objreflru_t) mkhead(ir *Objref_t) {
	if memfs {
		return
	}
	rl._mkhead(ir)
}

func (rl *objreflru_t) _mkhead(ir *Objref_t) {
	if rl.head == ir {
		return
	}
	rl._remove(ir)
	if rl.head != nil {
		rl.head.Refprev = ir
	}
	ir.Refnext = rl.head
	rl.head = ir
	if rl.tail == nil {
		rl.tail = ir
	}
}

func (rl *objreflru_t) _remove(ir *Objref_t) {
	if rl.tail == ir {
		rl.tail = ir.Refprev
	}
	if rl.head == ir {
		rl.head = ir.Refnext
	}
	if ir.Refprev != nil {
		ir.Refprev.Refnext = ir.Refnext
	}
	if ir.Refnext != nil {
		ir.Refnext.Refprev = ir.Refprev
	}
	ir.Refprev, ir.Refnext = nil, nil
}

func (rl *objreflru_t) remove(ir *Objref_t) {
	if memfs {
		return
	}
	rl._remove(ir)
}
