package progress

import (
	"strconv"

	"github.com/geofox/publisher/internal/store"
)

// FromStorePost builds a terminal Snapshot from a saved post, for replaying to a
// subscriber that connects after the post already finished. Store relay statuses
// ("ok"/"failed"/"skipped") map straight through; a segmented (threaded) target
// gets a "thread k/n" detail.
func FromStorePost(p *store.Post) Snapshot {
	s := Snapshot{PostID: p.ID, Status: p.Status}
	for _, tg := range p.Targets {
		ps := PlatformState{
			Platform: tg.Platform,
			Status:   tg.Status,
			URL:      tg.RemoteURL,
		}
		if n := len(tg.Segments); n > 1 {
			done := 0
			for _, sg := range tg.Segments {
				if sg.Status == "success" {
					done++
				}
			}
			ps.Detail = "thread " + strconv.Itoa(done) + "/" + strconv.Itoa(n)
		}
		for _, rr := range tg.Relays {
			ps.Relays = append(ps.Relays, RelayState{URL: rr.URL, Status: rr.Status, Message: rr.Message})
		}
		s.Platforms = append(s.Platforms, ps)
	}
	return s
}
