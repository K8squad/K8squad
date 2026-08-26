package jetstream

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Story 12.4 GUARDRAIL (§17.4 no-P2P): the plugin subscribe SDK is emit-only /
// read-only. A subscriber must have NO way to publish a message back onto the
// bus — the seam is a one-way projection of state that already committed to
// Postgres (ADR-001). We assert this structurally: neither the Subscriber type
// nor the Message a plugin receives may expose a publish/write/send/emit method.
// If someone later adds one, this test fails loudly rather than letting NATS
// become a P2P coordination channel.
func TestSubscriber_HasNoPublishSurface(t *testing.T) {
	banned := []string{"publish", "write", "send", "emit", "ack", "nak"}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(&Subscriber{}),
		reflect.TypeOf(Message{}),
		reflect.TypeOf(&Message{}),
	} {
		for i := 0; i < typ.NumMethod(); i++ {
			name := strings.ToLower(typ.Method(i).Name)
			for _, b := range banned {
				if strings.Contains(name, b) {
					t.Fatalf("%s exposes method %q — the subscribe SDK must stay read-only (§17.4, Story 12.4)",
						typ, typ.Method(i).Name)
				}
			}
		}
	}
}

// The Publisher (write side) and Subscriber (read side) are deliberately
// different types in this package. A plugin only ever imports the Subscriber, so
// it CANNOT reach Publish even though both live here — the guardrail is that the
// read path never hands out a *Publisher.
func TestSubscribe_RequiresURLAndDurable(t *testing.T) {
	if _, err := Subscribe(context.TODO(), SubscribeConfig{Durable: "d"}); err == nil {
		t.Fatal("Subscribe with empty URL = nil error, want required-URL error")
	}
	if _, err := Subscribe(context.TODO(), SubscribeConfig{URL: "nats://x:4222"}); err == nil {
		t.Fatal("Subscribe with empty Durable = nil error, want required-Durable error (per-plugin cursor)")
	}
}
