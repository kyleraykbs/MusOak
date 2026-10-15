package api

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The same question asked while the first answer is on its way costs one ask,
// not two: every search here is a provider call - for the music services this
// server talks to, a whole process per query - and the tracks tab and the
// albums tab ask at the same moment, as do two people.
func TestSearchAnswersShareTheAskInFlight(t *testing.T) {
	cache := newSearchAnswers(time.Minute, 8)

	var calls atomic.Int32
	release := make(chan struct{})
	first := make(chan any, 1)
	go func() {
		value, _ := cache.do("q", func() (any, error) {
			calls.Add(1)
			<-release
			return "answer", nil
		})
		first <- value
	}()

	// Wait for the ask to be in flight before the second one arrives.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	var wg sync.WaitGroup
	seconds := make([]any, 0, 4)
	var mu sync.Mutex
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			value, err := cache.do("q", func() (any, error) {
				calls.Add(1)
				return "second", nil
			})
			if err != nil {
				t.Errorf("second caller: %v", err)
			}
			mu.Lock()
			seconds = append(seconds, value)
			mu.Unlock()
		}()
	}
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want one shared ask", got)
	}
	if value := <-first; value != "answer" {
		t.Errorf("first caller got %v", value)
	}
	for _, value := range seconds {
		if value != "answer" {
			t.Errorf("a waiting caller got %v, want the shared answer", value)
		}
	}
}

// An answer inside the window is the same answer, not a new ask - and it is the
// same *list*: providers order their hits by whatever they like, so an uncached
// search shows the songs in a different order a second later.
func TestSearchAnswersHoldTheAnswerForTheWindow(t *testing.T) {
	cache := newSearchAnswers(50*time.Millisecond, 8)

	var calls atomic.Int32
	ask := func() (any, error) {
		calls.Add(1)
		return calls.Load(), nil
	}
	first, _ := cache.do("q", ask)
	second, _ := cache.do("q", ask)
	if first != second {
		t.Errorf("answers differ inside the window: %v then %v", first, second)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want the answer held", got)
	}

	time.Sleep(60 * time.Millisecond)
	third, _ := cache.do("q", ask)
	if third == first {
		t.Errorf("answer %v still held past the window", third)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want the window to expire into a fresh ask", got)
	}
}

// A failure is not remembered as the answer: the window is for answers, and a
// provider that was down for a second must not be the answer for the next
// minute.
func TestSearchAnswersDoNotHoldFailures(t *testing.T) {
	cache := newSearchAnswers(time.Minute, 8)
	boom := errors.New("provider down")

	var calls atomic.Int32
	first, err := cache.do("q", func() (any, error) {
		calls.Add(1)
		return nil, boom
	})
	if !errors.Is(err, boom) || first != nil {
		t.Fatalf("first ask = %v, %v", first, err)
	}
	second, err := cache.do("q", func() (any, error) {
		calls.Add(1)
		return "recovered", nil
	})
	if err != nil || second != "recovered" {
		t.Errorf("second ask = %v, %v, want a fresh try", second, err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("provider calls = %d, want the failure asked again", got)
	}
}

// The cache is bounded: a page that searches all day cannot grow it forever.
func TestSearchAnswersStayBounded(t *testing.T) {
	cache := newSearchAnswers(time.Minute, 4)
	for i := range 20 {
		key := string(rune('a' + i))
		if _, err := cache.do(key, func() (any, error) { return key, nil }); err != nil {
			t.Fatalf("ask %s: %v", key, err)
		}
	}
	cache.mu.Lock()
	size := len(cache.entries)
	cache.mu.Unlock()
	if size > 4 {
		t.Errorf("entries = %d, want the cache bounded to 4", size)
	}
}

// The key is the question: same words, same providers, same count - and a
// different provider set is a different question.
func TestSearchKeyNamesTheQuestion(t *testing.T) {
	same := searchKey("tracks", "nine inch nails", []string{"ytmusic", "youtube"}, 25)
	shuffled := searchKey("tracks", "nine inch nails", []string{"youtube", "ytmusic"}, 25)
	if same != shuffled {
		t.Errorf("provider order changed the question:\n%s\n%s", same, shuffled)
	}
	different := []string{
		searchKey("albums", "nine inch nails", []string{"ytmusic"}, 25),
		searchKey("tracks", "nine inch nails", []string{"youtube"}, 25),
		searchKey("tracks", "nine inch nails", []string{"ytmusic"}, 10),
	}
	for _, key := range different {
		if key == same {
			t.Errorf("a different question shares the key %s", key)
		}
	}
}

// A held answer that carries something live is re-stamped on every read: the
// window is for the expensive part of the answer, and the part that changes
// while it stands - a rendition's download state - is not served stale for the
// rest of the window. A search with nothing live in it holds its answer as it
// stands, which is what do does.
func TestSearchAnswersRestampLiveAnswersOnEveryRead(t *testing.T) {
	cache := newSearchAnswers(time.Minute, 8)

	var calls atomic.Int32
	ask := func() (any, error) {
		calls.Add(1)
		return "held", nil
	}
	restamp := func(held any) any { return held.(string) + "+live" }

	first, err := cache.doLive("q", ask, restamp)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	// The first caller gets the ask's own answer, so there is nothing stale in
	// what it is handed.
	if first != "held" {
		t.Errorf("first read = %v, want the fresh answer", first)
	}

	second, err := cache.doLive("q", ask, restamp)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if second != "held+live" {
		t.Errorf("second read = %v, want the held answer re-stamped", second)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want the ask held", got)
	}

	if got, err := cache.do("q", ask); err != nil || got != "held" {
		t.Errorf("do = %v, %v, want the held answer untouched", got, err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("provider calls = %d, want do to read the held answer", got)
	}
}
