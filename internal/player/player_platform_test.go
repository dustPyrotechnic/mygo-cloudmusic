package player

import (
	"errors"
	"slices"
	"testing"
	"time"

	"netease-music-mygo/internal/netease"
)

// Reset drops the current song and queue so a platform switch cannot retain playback.
func TestResetDropsPlaybackContext(t *testing.T) {
	p := &Player{at: -1, shuffle: true, repeat: RepeatOne}
	p.Cue([]netease.Track{{ID: 1, Name: "一"}, {ID: 2, Name: "二"}})
	p.radio = "radio"
	p.feed = func(func([]netease.Track)) {}
	p.playing = true
	p.Reset()
	if p.Current() != nil || len(p.Queue()) != 0 || p.Radio() != "" || p.Playing() {
		t.Fatalf("reset retained playback: current=%v queue=%d radio=%q playing=%v", p.Current(), len(p.Queue()), p.Radio(), p.Playing())
	}
	if p.Shuffle() || p.Repeat() != RepeatOff {
		t.Fatalf("reset retained playback modes: shuffle=%v repeat=%v", p.Shuffle(), p.Repeat())
	}
}

// Appending a queue preserves the current songs and ignores duplicate IDs.
func TestAppendQueue(t *testing.T) {
	p := &Player{at: 0, queue: []netease.Track{{ID: 1}}, order: []int{0}, track: &netease.Track{ID: 1}}
	p.AppendQueue([]netease.Track{{ID: 1}, {ID: 2}})
	if got := p.Queue(); len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("queue = %+v", got)
	}
	if len(p.order) != 2 || p.order[1] != 1 {
		t.Fatalf("order = %+v", p.order)
	}
}

func TestManualTrackLoadFailureKeepsSelection(t *testing.T) {
	queue := []netease.Track{{ID: 1, Name: "before"}, {ID: 2, Name: "selected"}, {ID: 3, Name: "after"}}
	for _, tc := range []struct {
		name        string
		err         error
		selectTrack func(*Player)
	}{
		{"list", errors.New("stream request failed"), func(p *Player) { p.PlayQueue(queue, 1) }},
		{"shuffled list", errors.New("stream request failed"), func(p *Player) {
			p.shuffle = true
			p.PlayQueue(queue, 1)
		}},
		{"queue jump", netease.ErrUnavailable, func(p *Player) { p.Jump(1) }},
		{"next button", errors.New("stream request failed"), func(p *Player) { p.Next() }},
		{"restore", errors.New("stream request failed"), func(p *Player) {
			p.Restore(Session{Queue: queue, Order: []int{0, 1, 2}, At: 1})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ch := make(chan func(), 64)
			p := &Player{post: func(fn func()) { ch <- fn }, at: -1}
			p.Artwork = func(*netease.Track) ([]byte, error) { return nil, nil }
			p.ResolveLyrics = func(*netease.Track) (string, string, string, error) { return "", "", "", nil }
			var requested []int64
			p.ResolveStream = func(track *netease.Track, _ string) (*netease.Stream, error) {
				requested = append(requested, track.ID)
				return nil, tc.err
			}
			p.Cue(queue)
			tc.selectTrack(p)
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			for p.Loading() {
				select {
				case fn := <-ch:
					fn()
				case <-deadline.C:
					t.Fatal("selected track never finished loading")
				}
			}
			if p.Current().ID != 2 || !slices.Equal(requested, []int64{2}) {
				t.Fatalf("selection replaced after failure: current=%d requested=%v", p.Current().ID, requested)
			}
			if p.Playing() || p.Err() == "" {
				t.Fatalf("failed selection must stop with its error: playing=%v err=%q", p.Playing(), p.Err())
			}
			if tc.err != netease.ErrUnavailable && p.Err() != tc.err.Error() {
				t.Fatalf("selected track error lost: %q", p.Err())
			}
		})
	}
}

// Automatic playback stops after bounded consecutive failures rather than
// skipping through an entire unavailable library.
func TestAutomaticFailedLoadsStopAfterAFew(t *testing.T) {
	ch := make(chan func(), 64)
	p := &Player{post: func(fn func()) { ch <- fn }, at: -1}
	p.Artwork = func(*netease.Track) ([]byte, error) { return nil, nil }
	p.ResolveLyrics = func(*netease.Track) (string, string, string, error) { return "", "", "", nil }
	var requested []int64
	p.ResolveStream = func(track *netease.Track, _ string) (*netease.Stream, error) {
		requested = append(requested, track.ID)
		return nil, errors.New("gone")
	}
	queue := make([]netease.Track, 10)
	for i := range queue {
		queue[i] = netease.Track{ID: int64(i + 1), Name: "song", Duration: 1000}
	}
	p.Cue(queue)
	p.ended()
	for p.Loading() {
		select {
		case fn := <-ch:
			fn()
		case <-time.After(2 * time.Second):
			t.Fatal("still loading")
		}
	}
	if !slices.Equal(requested, []int64{2, 3, 4}) || p.Current().ID != 4 || p.Playing() || p.Err() != "gone" {
		t.Fatalf("automatic failure limit: requested=%v current=%d playing=%v err=%q", requested, p.Current().ID, p.Playing(), p.Err())
	}
}
