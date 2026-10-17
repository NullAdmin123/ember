package playbackgateway

import (
	"fmt"
	"testing"
	"time"
)

// TestPlaybackCacheCapacityIsolatesMappings covers proof, retained-intent and
// admitted-session writes under both the mapping quota and global pressure.
func TestPlaybackCacheCapacityIsolatesMappings(t *testing.T) {
	for _, kind := range []string{"proof", "intent", "session"} {
		for _, scenario := range []struct {
			name                        string
			capacity, count, wantLength int
			frozen                      bool
		}{
			{"global", 3, 3, 3, false},
			{"mapping quota", 4096, 66, 65, false},
			{"same timestamp", 4096, 66, 65, true},
		} {
			t.Run(kind+"/"+scenario.name, func(t *testing.T) {
				cache := newPlaybackProofCache(scenario.capacity, time.Minute)
				now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
				cache.now = func() time.Time { return now }
				put := func(mapping, item string) {
					t.Helper()
					if !scenario.frozen {
						now = now.Add(time.Millisecond)
					}
					proof := fixturePlaybackProof(mapping, item, "source-1", "session-1")
					switch kind {
					case "proof":
						cache.Record([]PlaybackProof{proof})
					case "intent":
						guard := cache.BeginClient(mapping, item)
						_, retained, ok := cache.publishClientEvidence(guard, nil, []PlaybackProof{proof})
						cache.ReleasePublication(guard)
						if !ok || retained != 1 {
							t.Errorf("new intent rejected: retained=%d published=%t", retained, ok)
						}
					case "session":
						proof.transferIntent = true
						cache.Record([]PlaybackProof{proof})
						proof, _ = cache.Lookup(mapping, item, "source-1", "session-1")
						principal := fixturePrincipal()
						principal.MappingID = mapping
						cache.rememberPlaybackSession(proof, principal)
					}
				}
				has := func(mapping, item string) bool {
					key := playbackProofKey{mapping, item, "source-1", "session-1"}
					switch kind {
					case "proof":
						_, ok := cache.entries[key]
						return ok
					case "intent":
						_, ok := cache.intents[key]
						return ok
					default:
						_, ok := cache.sessions[key]
						return ok
					}
				}
				put("protected", "keep")
				for i := 0; i < scenario.count; i++ {
					put("busy", fmt.Sprintf("item-%d", i))
				}
				length := map[string]int{"proof": len(cache.entries), "intent": len(cache.intents), "session": len(cache.sessions)}[kind]
				if length != scenario.wantLength || !has("protected", "keep") || has("busy", "item-0") || !has("busy", fmt.Sprintf("item-%d", scenario.count-1)) {
					t.Fatalf("length=%d want=%d protected=%t oldestBusy=%t newestBusy=%t", length, scenario.wantLength, has("protected", "keep"), has("busy", "item-0"), has("busy", fmt.Sprintf("item-%d", scenario.count-1)))
				}
				if scenario.capacity == 3 {
					put("new-mapping", "new-item")
					if has("protected", "keep") || !has("new-mapping", "new-item") {
						t.Fatal("new mapping at global capacity did not replace the globally oldest entry")
					}
				}
			})
		}
	}
}
