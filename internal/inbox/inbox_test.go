package inbox

import "testing"

func TestAppendTakeAndCount(t *testing.T) {
	s := New()
	s.Append(Message{From: "0.1", FromName: "scout", To: "0.2", Subject: "hi"})
	s.Append(Message{From: "0.3", FromName: "docs", To: "0.2", Subject: "yo"})

	if n := s.UnreadCount("0.2"); n != 2 {
		t.Fatalf("unread = %d want 2", n)
	}
	got := s.Take("0.2")
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("take = %+v", got)
	}
	if n := s.UnreadCount("0.2"); n != 0 {
		t.Fatalf("unread after take = %d want 0", n)
	}
	if len(s.Take("0.2")) != 0 {
		t.Fatal("second take should be empty")
	}
	if len(s.All("0.2")) != 2 {
		t.Fatal("All should still show both")
	}
}

func TestSentAndReplied(t *testing.T) {
	s := New()
	id := s.Append(Message{From: "0.1", To: "0.2", Subject: "q"}) // 0.1 asks 0.2
	if sent := s.Sent("0.1"); len(sent) != 1 || sent[0].Replied {
		t.Fatalf("sent before reply = %+v", sent)
	}
	s.Append(Message{From: "0.2", To: "0.1", Subject: "a", ReplyTo: id}) // 0.2 replies
	sent := s.Sent("0.1")
	if len(sent) != 1 || !sent[0].Replied {
		t.Fatalf("original message should be marked replied: %+v", sent)
	}
}

func TestSnapshotLoadRoundTrip(t *testing.T) {
	s := New()
	id1 := s.Append(Message{From: "0.1", To: "0.2", Subject: "hi", Body: "one"})
	s.Append(Message{From: "0.2", To: "0.1", Subject: "re", Body: "two", ReplyTo: id1})
	// read 0.2's inbox so a Read flag is set (must survive the round-trip)
	s.Take("0.2")

	msgs, seq := s.Snapshot()
	if len(msgs) != 2 || seq != 2 {
		t.Fatalf("snapshot = %d msgs seq %d", len(msgs), seq)
	}

	r := New()
	r.Load(msgs, seq)
	// unread survives: 0.1 still has its unread reply
	if r.UnreadCount("0.1") != 1 {
		t.Fatalf("unread not preserved: %d", r.UnreadCount("0.1"))
	}
	// read state survives: 0.2 has nothing unread
	if r.UnreadCount("0.2") != 0 {
		t.Fatalf("read state not preserved: %d unread for 0.2", r.UnreadCount("0.2"))
	}
	// ID sequence continues (no collision with restored ids)
	id3 := r.Append(Message{From: "0.1", To: "0.2", Subject: "again"})
	if id3 != 3 {
		t.Fatalf("next id = %d want 3 (sequence should continue past restored ids)", id3)
	}
	// reply_to still resolves: the restored reply marked msg 1 Replied
	for _, m := range r.Sent("0.1") {
		if m.ID == id1 && !m.Replied {
			t.Fatal("Replied flag lost across restore")
		}
	}
}
