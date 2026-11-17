package logbus

import "testing"

func only(names ...string) func(string) bool {
	return func(instance string) bool {
		for _, n := range names {
			if n == instance {
				return true
			}
		}

		return false
	}
}

func TestSubscribeMatchesSeveralInstancesInPublishOrder(t *testing.T) {
	b := New()
	b.Publish("a", "1")
	b.Publish("a:1", "2")
	b.Publish("b", "3")
	b.Publish("a:2", "4")

	ch, cancel := b.Subscribe(only("a", "a:1", "a:2"), 10)
	defer cancel()

	b.Publish("a:1", "5")
	b.Publish("b", "6")

	got := drain(ch, 4)
	if len(got) != 4 {
		t.Fatalf("got %v", got)
	}

	for i, want := range []string{"1", "2", "4", "5"} {
		if got[i].Text != want {
			t.Errorf("line %d = %q, want %q", i, got[i].Text, want)
		}
	}
}
