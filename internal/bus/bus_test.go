package bus

import (
	"errors"
	"testing"

	"github.com/Sentinal-Glimpass/bubbles/internal/addr"
)

func TestSendDelivers(t *testing.T) {
	b := New()
	var got Message
	b.Subscribe(addr.Root, func(m Message) { got = m })
	err := b.Send(Message{From: "0.1", To: addr.Root, Subject: "hi", Body: "there"})
	if err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if got.From != "0.1" || got.Subject != "hi" {
		t.Fatalf("handler got %+v", got)
	}
}

func TestSendNoInbox(t *testing.T) {
	b := New()
	err := b.Send(Message{To: "0.9"})
	if !errors.Is(err, ErrNoInbox) {
		t.Fatalf("got %v want ErrNoInbox", err)
	}
}
