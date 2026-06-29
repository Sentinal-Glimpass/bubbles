package inbox

import "testing"

func TestAppendTakeAndCount(t *testing.T) {
	s := New()
	s.Append(Message{From: "0.1", FromName: "scout", To: "0.2", Subject: "hi"})
	s.Append(Message{From: "0.3", FromName: "docs", To: "0.2", Subject: "yo", Urgent: true})

	if n := s.UnreadCount("0.2"); n != 2 {
		t.Fatalf("unread = %d want 2", n)
	}
	got := s.Take("0.2")
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 2 {
		t.Fatalf("take = %+v", got)
	}
	// reading clears unread
	if n := s.UnreadCount("0.2"); n != 0 {
		t.Fatalf("unread after take = %d want 0", n)
	}
	if len(s.Take("0.2")) != 0 {
		t.Fatal("second take should be empty")
	}
	// history persists
	if len(s.All("0.2")) != 2 {
		t.Fatal("All should still show both")
	}
}
