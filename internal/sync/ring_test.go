package sync

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// emptySnapshot is a full-snapshot function over an empty task list.
func emptySnapshot() Delta { return newDelta() }

func TestRingEvictsAfterDepth(t *testing.T) {
	r := NewRing()
	for i := int64(1); i <= RingDepth+1; i++ {
		r.Append(Delta{RID: i})
	}

	if _, ok := r.Since(1); ok {
		t.Fatal("rid 1 was evicted by 301 appends, but Since still reports a hit")
	}
	d, ok := r.Since(2)
	if !ok {
		t.Fatal("rid 2 is the oldest retained entry, want a hit")
	}
	if d.RID != RingDepth+1 {
		t.Fatalf("coalesced rid = %d, want %d", d.RID, RingDepth+1)
	}
}

func TestSinceCoalesces(t *testing.T) {
	r := NewRing()
	r.Append(Delta{RID: 1}) // the rid the reader stands on
	r.Append(Delta{RID: 2, Tasks: map[string]json.RawMessage{
		"tsk_a": json.RawMessage(`{"progress":0.5}`),
		"tsk_b": json.RawMessage(`{"state":"downloading"}`),
	}})
	r.Append(Delta{RID: 3, Tasks: map[string]json.RawMessage{
		"tsk_a": json.RawMessage(`{"progress":0.9}`),
	}, Stats: Stats{SpeedDown: 7}})

	d, ok := r.Since(1)
	if !ok {
		t.Fatal("rid 1 is inside the ring, want a hit")
	}
	if got := string(d.Tasks["tsk_a"]); got != `{"progress":0.9}` {
		t.Fatalf(`tsk_a = %s, want the newest entry {"progress":0.9}`, got)
	}
	if _, ok := d.Tasks["tsk_b"]; !ok {
		t.Fatal("tsk_b was only in the older delta but must survive coalescing")
	}
	if d.Stats.SpeedDown != 7 {
		t.Fatalf("stats.speed_down = %d, want the newest entry's 7", d.Stats.SpeedDown)
	}
	if d.RID != 3 {
		t.Fatalf("coalesced rid = %d, want 3", d.RID)
	}
}

func TestSinceRemovalBeatsUpdate(t *testing.T) {
	r := NewRing()
	r.Append(Delta{RID: 1})
	r.Append(Delta{RID: 2, Tasks: map[string]json.RawMessage{
		"tsk_a": json.RawMessage(`{"progress":0.5}`),
	}})
	r.Append(Delta{RID: 3, TasksRemoved: []string{"tsk_a"}})

	d, ok := r.Since(1)
	if !ok {
		t.Fatal("rid 1 is inside the ring, want a hit")
	}
	if _, ok := d.Tasks["tsk_a"]; ok {
		t.Fatal("tsk_a is present in both Tasks and TasksRemoved, and must resolve to removed")
	}
	if len(d.TasksRemoved) != 1 || d.TasksRemoved[0] != "tsk_a" {
		t.Fatalf("tasks_removed = %v, want [tsk_a]", d.TasksRemoved)
	}
}

func TestEvictedRIDForcesFullUpdate(t *testing.T) {
	h := NewHub()
	for i := 0; i <= RingDepth; i++ {
		h.Publish(Delta{})
	}

	if _, ok := h.ring.Since(1); ok {
		t.Fatal("rid 1 was evicted from the ring")
	}

	full := Delta{Tasks: map[string]json.RawMessage{
		"tsk_everything": json.RawMessage(`{"state":"completed"}`),
	}}
	seedCh, cancel := h.Subscribe(context.Background(), "1", func() Delta { return full })
	defer cancel()

	seed := <-seedCh
	if !seed.FullUpdate || !seed.SeqGap {
		t.Fatalf("full_update=%v seq_gap=%v, want both true for an evicted rid", seed.FullUpdate, seed.SeqGap)
	}
	if _, ok := seed.Tasks["tsk_everything"]; !ok {
		t.Fatal("the seed must carry the entire snapshot's task list")
	}
	if seed.RID != RingDepth+1 {
		t.Fatalf("seed rid = %d, want the current rid %d", seed.RID, RingDepth+1)
	}
}

func TestSubscribeLastEventIDCoalescedDiff(t *testing.T) {
	h := NewHub()
	h.Publish(Delta{Tasks: map[string]json.RawMessage{ // rid 1, base
		"tsk_a": json.RawMessage(`{"state":"downloading"}`),
	}})
	h.Publish(Delta{Tasks: map[string]json.RawMessage{ // rid 2
		"tsk_b": json.RawMessage(`{"state":"seeding"}`),
	}})
	h.Publish(Delta{})                                 // rid 3
	h.Publish(Delta{})                                 // rid 4
	h.Publish(Delta{Tasks: map[string]json.RawMessage{ // rid 5
		"tsk_a": json.RawMessage(`{"progress":0.5}`),
	}})
	h.Publish(Delta{TasksRemoved: []string{"tsk_b"}})  // rid 6
	h.Publish(Delta{Tasks: map[string]json.RawMessage{ // rid 7
		"tsk_a": json.RawMessage(`{"progress":0.9}`),
	}, Stats: Stats{Active: 2}})

	seedCh, cancel := h.Subscribe(context.Background(), "5", emptySnapshot)
	defer cancel()

	seed := <-seedCh
	if seed.FullUpdate || seed.SeqGap {
		t.Fatalf("full_update=%v seq_gap=%v, want both false on a ring hit", seed.FullUpdate, seed.SeqGap)
	}
	if seed.RID != 7 {
		t.Fatalf("seed rid = %d, want 7", seed.RID)
	}
	if got := string(seed.Tasks["tsk_a"]); got != `{"progress":0.9}` {
		t.Fatalf(`tsk_a = %s, want the newest entry {"progress":0.9}`, got)
	}
	if _, ok := seed.Tasks["tsk_b"]; ok {
		t.Fatal("tsk_b was removed after rid 5 and must not be in the diff")
	}
	if len(seed.TasksRemoved) != 1 || seed.TasksRemoved[0] != "tsk_b" {
		t.Fatalf("tasks_removed = %v, want [tsk_b]", seed.TasksRemoved)
	}
	if seed.Stats.Active != 2 {
		t.Fatalf("stats.active = %d, want the newest entry's 2", seed.Stats.Active)
	}
}

func TestSubscribeWithoutLastEventIDForcesFullUpdate(t *testing.T) {
	h := NewHub()
	h.Publish(Delta{Tasks: map[string]json.RawMessage{
		"tsk_a": json.RawMessage(`{"state":"downloading"}`),
	}})

	full := Delta{Tasks: map[string]json.RawMessage{
		"tsk_a": json.RawMessage(`{"state":"downloading","progress":0.4}`),
	}}

	for _, lastEventID := range []string{"", "not-a-number"} {
		seedCh, cancel := h.Subscribe(context.Background(), lastEventID, func() Delta { return full })
		seed := <-seedCh
		cancel()

		if !seed.FullUpdate || !seed.SeqGap {
			t.Fatalf("lastEventID %q: full_update=%v seq_gap=%v, want both true", lastEventID, seed.FullUpdate, seed.SeqGap)
		}
		if _, ok := seed.Tasks["tsk_a"]; !ok {
			t.Fatalf("lastEventID %q: the seed must carry the entire task list", lastEventID)
		}
	}
}

func TestPublishCoalescesWithinWindow(t *testing.T) {
	h := NewHub()
	seedCh, cancel := h.Subscribe(context.Background(), "", emptySnapshot)
	defer cancel()
	<-seedCh // drain the seed so only deliveries remain observable

	h.Publish(Delta{Tasks: map[string]json.RawMessage{"tsk_a": json.RawMessage(`{"progress":0.1}`)}})
	h.Publish(Delta{Tasks: map[string]json.RawMessage{"tsk_a": json.RawMessage(`{"progress":0.2}`)}})
	h.Publish(Delta{Tasks: map[string]json.RawMessage{"tsk_b": json.RawMessage(`{"state":"seeding"}`)}})

	// The first delta leaves immediately: no window was open.
	select {
	case d := <-seedCh:
		if d.RID != 1 {
			t.Fatalf("first delivery rid = %d, want 1", d.RID)
		}
	case <-time.After(publishInterval):
		t.Fatal("the first delivery never arrived")
	}

	// The window's close coalesces rids 2 and 3 into one message.
	select {
	case d := <-seedCh:
		if d.RID != 3 {
			t.Fatalf("coalesced rid = %d, want 3", d.RID)
		}
		if got := string(d.Tasks["tsk_a"]); got != `{"progress":0.2}` {
			t.Fatalf(`tsk_a = %s, want the newest entry {"progress":0.2}`, got)
		}
		if _, ok := d.Tasks["tsk_b"]; !ok {
			t.Fatal("the coalesced delivery lost tsk_b")
		}
	case <-time.After(3 * publishInterval):
		t.Fatal("the coalesced delivery never arrived")
	}

	select {
	case d := <-seedCh:
		t.Fatalf("unexpected extra delivery: %+v", d)
	default:
	}
}

func TestSlowSubscriberIsClosed(t *testing.T) {
	h := NewHub()
	seedCh, cancel := h.Subscribe(context.Background(), "", emptySnapshot)
	defer cancel()
	// The seed is never read: the buffer must fill and evict the subscriber.

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBuffer+1; i++ {
			// Rewind the window so every Publish fans out synchronously.
			h.mu.Lock()
			h.lastSend = time.Now().Add(-2 * publishInterval)
			h.mu.Unlock()
			h.Publish(Delta{})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish never returned: fan-out blocked on a slow subscriber")
	}

	if n := h.Clients(); n != 0 {
		t.Fatalf("Clients = %d, want 0 after the slow subscriber was evicted", n)
	}
	// Buffered deliveries may remain in a closed channel; drain to the close.
	for {
		_, open := <-seedCh
		if !open {
			break
		}
	}
}

func TestClientsCount(t *testing.T) {
	h := NewHub()
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	_, cancel1 := h.Subscribe(ctx, "", emptySnapshot)
	_, cancel2 := h.Subscribe(context.Background(), "", emptySnapshot)
	if n := h.Clients(); n != 2 {
		t.Fatalf("Clients = %d, want 2", n)
	}

	// Cancelling the context removes the first subscriber via its watcher.
	cancelCtx()
	deadline := time.Now().Add(time.Second)
	for h.Clients() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := h.Clients(); n != 1 {
		t.Fatalf("Clients = %d, want 1 after ctx cancellation", n)
	}

	cancel2()
	cancel2() // idempotent
	if n := h.Clients(); n != 0 {
		t.Fatalf("Clients = %d, want 0 after every cancel", n)
	}

	_ = cancel1 // a cancel after removal is a no-op
	if n := h.Clients(); n != 0 {
		t.Fatalf("Clients = %d, want 0 after a redundant cancel", n)
	}
}

func TestDeltaJSONFieldNames(t *testing.T) {
	d := Delta{
		RID:          42,
		Tasks:        map[string]json.RawMessage{"tsk_1": json.RawMessage(`{"progress":0.4137}`)},
		TasksRemoved: []string{},
		Stats:        Stats{SpeedDown: 2097152, SpeedUp: 131072, Active: 3, Queued: 11},
	}

	encoded, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"rid":42,"full_update":false,"tasks":{"tsk_1":{"progress":0.4137}},` +
		`"tasks_removed":[],"stats":{"speed_down":2097152,"speed_up":131072,"active":3,"queued":11},` +
		`"seq_gap":false}`
	if string(encoded) != want {
		t.Fatalf("marshalled delta\n got %s\nwant %s", encoded, want)
	}

	// The optional category fields appear only when set.
	d.Categories = map[string]json.RawMessage{"linux": json.RawMessage(`{"save_path":"/data"}`)}
	d.CategoriesRemoved = []string{"iso"}
	encoded, err = json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, name := range []string{"rid", "full_update", "tasks", "tasks_removed",
		"categories", "categories_removed", "stats", "seq_gap"} {
		if _, ok := keys[name]; !ok {
			t.Fatalf("key %q missing from %s", name, encoded)
		}
	}
	if len(keys) != 8 {
		t.Fatalf("got %d keys, want exactly the 8 known ones: %s", len(keys), encoded)
	}
}
