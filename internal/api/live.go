package api

import (
	"context"
	"errors"
	"sync"
	"time"

	"mhvtl-control/internal/changer"
	"mhvtl-control/internal/config"
	"mhvtl-control/internal/validate"
)

// liveTTL is how long one READ ELEMENT STATUS of a library serves the
// dashboard's views (GET /libraries, /libraries/{id}/media, /config, the
// slots route and POST /config/validate), so a refresh reads each changer
// once. Each read can consume the changer's POWER-ON unit attention, which
// is one flag per vtllibrary, not per initiator (check_reset, mhvtl
// usr/vtllib.c:620-628, called at usr/cmd/vtllibrary.c:227 and raised at
// :1702, upstream master d65d25f), and so hide the reset from backup hosts
// (plan, fact 13). Moves this daemon makes drop the library's read
// (forget); moves a backup application makes show after the TTL.
const liveTTL = 5 * time.Second

// errNoChangers is the live error of every library when changer access is
// not configured.
var errNoChangers = errors.New("changer access not configured")

// liveCache shares the live inventory of each library among the read-only
// views: one read at a time per library, served for liveTTL, with
// concurrent callers waiting for the read in progress. Failures are cached
// too, so a missing changer is not probed on every call. A cached inventory
// is shared and must not be modified.
type liveCache struct {
	ttl time.Duration // liveTTL when zero

	mu   sync.Mutex
	libs map[int]*liveRead
}

// liveRead is one READ ELEMENT STATUS of a library. inv, err and at are set
// before done is closed.
type liveRead struct {
	done   chan struct{}
	serial string // the changer is found by its serial: another is another changer
	at     time.Time
	inv    changer.Inventory
	err    error
}

// get returns the inventory of lib, from a read still fresh or in progress,
// else from read. A caller waiting for another's read gives up when ctx
// ends; read itself runs to the end.
func (c *liveCache) get(ctx context.Context, lib config.Library, read func() (changer.Inventory, error)) (changer.Inventory, error) {
	ttl := c.ttl
	if ttl == 0 {
		ttl = liveTTL
	}
	c.mu.Lock()
	if c.libs == nil {
		c.libs = map[int]*liveRead{}
	}
	e := c.libs[lib.ID]
	if e != nil {
		select {
		case <-e.done:
			if time.Since(e.at) >= ttl || e.serial != lib.Serial {
				e = nil
			}
		default: // in progress: wait for it, unless it is another changer's
			if e.serial != lib.Serial {
				e = nil
			}
		}
	}
	if e != nil {
		c.mu.Unlock()
		select {
		case <-e.done:
			return e.inv, e.err
		case <-ctx.Done():
			return changer.Inventory{}, ctx.Err()
		}
	}
	e = &liveRead{done: make(chan struct{}), serial: lib.Serial}
	c.libs[lib.ID] = e
	c.mu.Unlock()

	// Waiters must never hang: done is closed even when read panics. The
	// panic is not cached, and goes on up to recoverMW.
	panicked := true
	defer func() {
		if panicked {
			e.err = errors.New("reading the changer failed: internal error")
			c.mu.Lock()
			if c.libs[lib.ID] == e {
				delete(c.libs, lib.ID)
			}
			c.mu.Unlock()
		}
		e.at = time.Now()
		close(e.done)
	}()
	e.inv, e.err = read()
	panicked = false
	return e.inv, e.err
}

// forget drops library id's read, so the next view reads the changer again.
// Callers that move cartridges call it after the move, while they still
// hold the library's lock.
func (c *liveCache) forget(id int) {
	c.mu.Lock()
	delete(c.libs, id)
	c.mu.Unlock()
}

// liveInventory reads lib's changer through the live cache.
func (s *Server) liveInventory(ctx context.Context, lib config.Library) (changer.Inventory, error) {
	if s.cfg.Changers == nil {
		return changer.Inventory{}, errNoChangers
	}
	return s.live.get(ctx, lib, func() (changer.Inventory, error) { return s.cfg.Changers.Inventory(lib) })
}

// liveInventories reads the changers of libs in parallel, through the live
// cache. A library whose changer cannot be read is missing from invs, with
// the reason in errs.
func (s *Server) liveInventories(ctx context.Context, libs []config.Library) (invs map[int]changer.Inventory, errs map[int]string) {
	invs, errs = map[int]changer.Inventory{}, map[int]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, l := range libs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inv, err := s.liveInventory(ctx, l)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[l.ID] = err.Error()
				return
			}
			invs[l.ID] = inv
		}()
	}
	wg.Wait()
	return invs, errs
}

// readLive is liveInventories as placements.
func (s *Server) readLive(ctx context.Context, libs []config.Library) (live map[int]validate.Placement, errs map[int]string) {
	invs, errs := s.liveInventories(ctx, libs)
	live = map[int]validate.Placement{}
	for id, inv := range invs {
		live[id] = placementOf(inv)
	}
	return live, errs
}

// placementOf maps where inv holds cartridges, by element number.
func placementOf(inv changer.Inventory) validate.Placement {
	p := validate.Placement{Slots: map[int]string{}, MAPs: map[int]string{}, Drives: map[int]string{}}
	for _, sl := range inv.Slots {
		if sl.Full && sl.Barcode != "" {
			p.Slots[sl.Number] = sl.Barcode
		}
	}
	for _, sl := range inv.MAPs {
		if sl.Full && sl.Barcode != "" {
			p.MAPs[sl.Number] = sl.Barcode
		}
	}
	for _, d := range inv.Drives {
		if d.Full && d.Barcode != "" {
			p.Drives[d.Number] = d.Barcode
		}
	}
	return p
}
