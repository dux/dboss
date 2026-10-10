package vibe

import (
	"context"
	"sync"
	"time"

	"dboss/internal/fault"
	"dboss/internal/fsutil"
	"dboss/internal/logx"
)

// maxEntries bounds the chat lines a harness keeps; the oldest go first.
const maxEntries = 500

// Entry is one line of the chat the page shows: the owner's message, the assistant's answer, a
// tool call with its result, something an MCP client did, or an error.
type Entry struct {
	ID      int       `json:"id"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text,omitempty"`
	Tool    string    `json:"tool,omitempty"`
	Summary string    `json:"summary,omitempty"`
	Result  string    `json:"result,omitempty"`
	OK      bool      `json:"ok,omitempty"`
	Path    string    `json:"path,omitempty"`
	Time    time.Time `json:"time"`
}

// Entry kinds.
const (
	KindUser      = "user"
	KindAssistant = "assistant"
	KindTool      = "tool"
	KindExternal  = "external"
	KindError     = "error"
)

// transcript is what a harness persists: the messages the model sees and the lines the page shows.
type transcript struct {
	Messages []Message `json:"messages"`
	Entries  []Entry   `json:"entries"`
	NextID   int       `json:"next_id"`
}

// event is one server-sent event of the harness stream.
type event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// pendingTool is the tool call the running turn is waiting on.
type pendingTool struct {
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
}

// snapshot is the first event of every stream: the whole chat and the running turn's progress, so
// a page that reloads mid-turn catches up without a gap.
type snapshot struct {
	Entries []Entry      `json:"entries"`
	Running bool         `json:"running"`
	Live    string       `json:"live"`
	Pending *pendingTool `json:"pending"`
}

// harness is one web process's chat: its transcript, the turn running on it and the pages
// listening to it.
type harness struct {
	app, process, path string

	mu          sync.Mutex
	loaded      bool
	chat        transcript
	running     bool
	cancel      context.CancelFunc
	live        string
	pending     *pendingTool
	subscribers map[chan event]struct{}
}

func newHarness(app, process, path string) *harness {
	return &harness{app: app, process: process, path: path, subscribers: map[chan event]struct{}{}}
}

// load reads the transcript once; callers hold mu.
func (h *harness) load() {
	if h.loaded {
		return
	}
	h.loaded = true
	if err := fsutil.ReadJSON(h.path, &h.chat); err != nil {
		logx.Warnf("vibe %s/%s: unreadable transcript, starting a new chat: %v", h.app, h.process, err)
		h.chat = transcript{}
	}
}

// save writes the transcript; callers hold mu.
func (h *harness) save() {
	if err := fsutil.WriteJSON(h.path, h.chat, 0o600); err != nil {
		logx.Warnf("vibe %s/%s: save transcript: %v", h.app, h.process, err)
	}
}

// subscribe registers a stream and returns its first event and the channel the rest arrive on.
func (h *harness) subscribe() (event, chan event, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	ch := make(chan event, 256)
	h.subscribers[ch] = struct{}{}
	first := event{Type: "snapshot", Data: snapshot{Entries: append([]Entry(nil), h.chat.Entries...), Running: h.running, Live: h.live, Pending: h.pending}}
	return first, ch, func() {
		h.mu.Lock()
		delete(h.subscribers, ch)
		h.mu.Unlock()
	}
}

// publish sends ev to every stream; callers hold mu. A stream too slow to keep up loses the event
// and catches up with the next snapshot, so a stuck page never blocks a turn.
func (h *harness) publish(ev event) {
	for ch := range h.subscribers {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Notify tells the pages to re-read the working tree.
func (h *harness) notifyGit() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.publish(event{Type: "git", Data: struct{}{}})
}

// add appends a chat line, publishes it and, unless a turn is running, saves the transcript.
func (h *harness) add(entry Entry) Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	h.chat.NextID++
	entry.ID = h.chat.NextID
	if entry.Time.IsZero() {
		entry.Time = time.Now().UTC()
	}
	h.chat.Entries = append(h.chat.Entries, entry)
	if len(h.chat.Entries) > maxEntries {
		h.chat.Entries = append([]Entry(nil), h.chat.Entries[len(h.chat.Entries)-maxEntries:]...)
	}
	switch entry.Kind {
	case KindAssistant:
		h.live = ""
	case KindTool:
		h.pending = nil
	}
	h.publish(event{Type: "entry", Data: entry})
	if !h.running {
		h.save()
	}
	return entry
}

// delta streams part of the assistant's answer.
func (h *harness) delta(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.live += text
	h.publish(event{Type: "delta", Data: map[string]string{"text": text}})
}

// toolStarted shows the tool call the turn now waits on.
func (h *harness) toolStarted(tool, summary string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pending = &pendingTool{Tool: tool, Summary: summary}
	h.publish(event{Type: "tool", Data: h.pending})
}

// messages returns a copy of what the model has seen so far.
func (h *harness) messages() []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	return append([]Message(nil), h.chat.Messages...)
}

// appendMessages records what the model saw or said.
func (h *harness) appendMessages(messages ...Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.chat.Messages = append(h.chat.Messages, messages...)
}

// begin marks a turn running, or refuses when one already is.
func (h *harness) begin(cancel context.CancelFunc) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	if h.running {
		return fault.Invalidf("a chat turn is already running")
	}
	h.running, h.cancel, h.live, h.pending = true, cancel, "", nil
	h.publish(event{Type: "turn", Data: turnState{Running: true}})
	return nil
}

// turnState is the "turn" event: whether a turn runs, and what the one that ended did.
type turnState struct {
	Running   bool   `json:"running"`
	Wrote     bool   `json:"wrote,omitempty"`
	Restarted bool   `json:"restarted,omitempty"`
	Error     string `json:"error,omitempty"`
}

// end closes the turn, saves the transcript and tells the pages.
func (h *harness) end(state turnState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running, h.cancel, h.live, h.pending = false, nil, "", nil
	h.save()
	h.publish(event{Type: "turn", Data: state})
}

// stop cancels the running turn, if any.
func (h *harness) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
}

// reset clears the chat, refusing while a turn runs.
func (h *harness) reset() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.load()
	if h.running {
		return fault.Invalidf("stop the running turn first")
	}
	h.chat = transcript{NextID: h.chat.NextID}
	h.save()
	h.publish(event{Type: "snapshot", Data: snapshot{Entries: []Entry{}}})
	return nil
}
