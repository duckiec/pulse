package engine

import (
	"bytes"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"time"
	"unsafe"
)

// SSE limits (ROADMAP.md §2.1.3).
const (
	// DefaultMaxLine bounds a single accumulated line. A server that never
	// emits a newline would otherwise grow the buffer without limit; past the
	// cap the parser fails closed rather than allocating forever.
	DefaultMaxLine = 1 << 20 // 1 MiB

	// DefaultMaxEventData bounds one event's accumulated data lines.
	DefaultMaxEventData = 8 << 20 // 8 MiB
)

// Errors surfaced by the SSE layer. Every one is retryable-or-terminal by
// construction: ErrIdle means "reconnect", the rest mean "stop".
var (
	// ErrWriteUnsupported is returned by a read-only transport's Write. The
	// Stream interface is unified across HTTP/SSE/WS/gRPC; rather than lying
	// with a no-op, read-only transports refuse (ROADMAP.md §2.1.1).
	ErrWriteUnsupported = errors.New("engine: stream is read-only")

	// ErrIdle means the server sent nothing for longer than the idle timeout.
	// It is retryable: the caller reconnects, replaying Last-Event-ID.
	ErrIdle = errors.New("engine: stream idle timeout")

	// ErrOversizeLine means a single SSE line exceeded the cap. Terminal: the
	// framing is broken, so no reconnect can fix it.
	ErrOversizeLine = errors.New("engine: SSE line exceeds the configured cap")

	// ErrOversizeEvent means one event's data exceeded the cap. Terminal.
	ErrOversizeEvent = errors.New("engine: SSE event data exceeds the configured cap")

	// ErrUnexpectedStatus means the server answered a non-2xx. Terminal:
	// reconnecting would hammer an endpoint that already refused.
	ErrUnexpectedStatus = errors.New("engine: SSE endpoint returned a non-2xx status")
)

// Event is one Server-Sent Event (ROADMAP.md §2.1.3).
//
// Data aliases the parser's internal buffer and is valid ONLY until the next
// Feed call: the parser does not copy per event, because an event flood is
// exactly the case where per-event allocation must not happen. A consumer that
// needs to retain the bytes must copy them — which the SSE stream does, into a
// pooled chunk, before handing anything to a Sink.
//
// ID and Name are interned: unchanged values reuse the previously converted
// string, so a stream where id/name never change allocates nothing per event.
type Event struct {
	ID    string
	Name  string
	Data  []byte
	Retry time.Duration
}

// sseParser is a resumable state machine over a bounded line buffer. It holds
// partial state across Feed calls because events split across arbitrary chunk
// boundaries: a field name cut in half, a data line cut in half, an event split
// between two 32 KiB chunks.
type sseParser struct {
	buf  []byte // bytes not yet forming a complete line
	line []byte // current line, being accumulated

	data  []byte // accumulated data lines for the event in progress
	id    []byte
	name  []byte
	retry time.Duration

	haveID   bool // an `id:` field was seen in the CURRENT event
	haveData bool

	// lastID is retained across events AND across connections: it is the value
	// replayed as Last-Event-ID when the transport reconnects (§2.1.6). It is
	// deliberately NOT cleared on disconnect — clearing it is precisely the
	// mutation the reconnect gate exists to catch.
	lastID []byte

	// interned strings for the hot path (unchanged id/name ⇒ no allocation).
	lastIDStr   string
	lastNameStr string
	curIDStr    string
	curNameStr  string

	heartbeats int // comment lines seen since the last Heartbeats() call

	maxLine     int
	maxEvent    int
	bomStripped bool
	err         error

	// pending is the chunk handed to Feed, read by the pre-bound sequence.
	pending []byte

	// seq is pre-bound so repeated Feed calls do not allocate a new closure.
	seq iter.Seq[Event]
}

func newSSEParser() *sseParser {
	p := &sseParser{maxLine: DefaultMaxLine, maxEvent: DefaultMaxEventData}
	p.buf = make([]byte, 0, 4096)
	p.line = make([]byte, 0, 4096)
	p.data = make([]byte, 0, 4096)
	p.seq = p.iterate
	return p
}

// LastEventID returns the id to replay on reconnect ("" when none has been
// seen). This is the whole resume mechanism, and it survives disconnects.
func (p *sseParser) LastEventID() string { return p.curIDStrOrLast() }

// Heartbeats reports how many comment (":" heartbeat) lines were consumed since
// the last call. Heartbeats renew the read deadline without producing an event.
func (p *sseParser) Heartbeats() int {
	n := p.heartbeats
	p.heartbeats = 0
	return n
}

// Err returns the terminal parse error, if any.
func (p *sseParser) Err() error { return p.err }

func (p *sseParser) curIDStrOrLast() string {
	if p.curIDStr != "" {
		return p.curIDStr
	}
	return p.lastIDStr
}

// Feed consumes a chunk and yields every event completed by it.
func (p *sseParser) Feed(chunk []byte) iter.Seq[Event] {
	p.pending = chunk
	return p.seq
}

// iterate is the pre-bound sequence body. Keeping it stored on the parser means
// Feed does not allocate a fresh closure per chunk, which is what keeps a
// steady event flood at zero allocations.
func (p *sseParser) iterate(yield func(Event) bool) {
	chunk := p.pending
	p.heartbeats = 0
	p.err = nil
	p.buf = append(p.buf, chunk...)

	// Walk the buffer tracking BOTH the scan cursor and the start of the line
	// being accumulated: slicing from the cursor alone would hand every line the
	// whole buffer seen so far, and each field value would swallow the lines
	// before it.
	i, start := 0, 0
	for i < len(p.buf) {
		c := p.buf[i]
		if c != '\n' {
			i++
			continue
		}
		// Complete line: p.buf[i] is the terminator.
		blank, err := p.consumeLine(p.buf[start:i])
		if err != nil {
			p.err = err
			return
		}
		i++
		start = i
		// A blank line dispatches the event in progress.
		if blank && p.pendingDispatch() {
			if !p.dispatch(yield) {
				p.buf = append(p.buf[:0], p.buf[start:]...)
				return
			}
		}
	}
	// Retain only the unconsumed tail.
	p.buf = append(p.buf[:0], p.buf[start:]...)
	if len(p.buf) > p.maxLine {
		p.err = ErrOversizeLine
	}
}

// pendingDispatch reports whether a blank line should emit an event. Per the
// SSE spec a blank line with no preceding data is a no-op, not an empty event.
func (p *sseParser) pendingDispatch() bool { return p.haveData || p.haveID || len(p.name) > 0 }

// dispatch emits the accumulated event.
func (p *sseParser) dispatch(yield func(Event) bool) bool {
	// Strip one trailing newline per the spec's data-joining rule.
	data := p.data
	if n := len(data); n > 0 && data[n-1] == '\n' {
		data = data[:n-1]
	}
	// Intern id/name: reuse the previous string when unchanged.
	id := p.intern(p.id, &p.curIDStr, &p.lastIDStr)
	name := p.intern(p.name, &p.curNameStr, &p.lastNameStr)

	ev := Event{ID: id, Name: name, Data: data, Retry: p.retry}
	ok := yield(ev)

	p.data = p.data[:0]
	p.haveData = false
	p.haveID = false
	p.id = p.id[:0]
	p.name = p.name[:0]
	return ok
}

// intern converts a byte slice to a string, reusing the cached string when the
// bytes are unchanged. This is what keeps a steady event flood allocation-free.
func (p *sseParser) intern(b []byte, cur, last *string) string {
	if len(b) == 0 {
		return ""
	}
	if *cur != "" && bytes.Equal(safeBytes(*cur), b) {
		return *cur
	}
	s := string(b)
	*cur = s
	*last = s
	return s
}

// safeBytes views a string's bytes. The string was created from the parser's
// own buffer and the parser never mutates a string's backing bytes (it resets
// slices, never overwrites through the string), so this view is read-only.
func safeBytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// Flush yields the partial event left in the buffer at stream EOF (ROADMAP.md
// §2.1.3): a truncated final event is still an event.
func (p *sseParser) Flush() iter.Seq[Event] {
	return func(yield func(Event) bool) {
		if p.err != nil {
			return
		}
		if len(p.buf) > 0 {
			// A trailing partial line with no terminator.
			_, _ = p.consumeLine(p.buf)
			p.buf = p.buf[:0]
		}
		if p.pendingDispatch() {
			p.dispatch(yield)
		}
	}
}

// consumeLine processes one complete line (without its newline), handling the
// CR of a CRLF pair. It reports whether the line was blank, which is the only
// thing that dispatches an event.
func (p *sseParser) consumeLine(raw []byte) (blank bool, err error) {
	// Tolerate CRLF.
	if n := len(raw); n > 0 && raw[n-1] == '\r' {
		raw = raw[:n-1]
	}
	// Strip a UTF-8 BOM at the very start of the stream.
	if !p.bomStripped {
		p.bomStripped = true
		if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
			raw = raw[3:]
		}
	}

	if len(raw) == 0 {
		return true, nil
	}
	if len(raw) > p.maxLine {
		return false, ErrOversizeLine
	}

	// A leading colon is a comment/heartbeat: ignored for data, but it proves
	// the connection is alive and renews the read deadline.
	if raw[0] == ':' {
		p.heartbeats++
		return false, nil
	}

	field, value, found := bytes.Cut(raw, []byte{':'})
	if !found {
		// A line with no colon is a field with an empty value.
		field, value = raw, nil
	} else {
		// Exactly one optional leading space is stripped.
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
	}

	switch string(field) {
	case "data":
		if len(p.data)+len(value)+1 > p.maxEvent {
			return false, ErrOversizeEvent
		}
		p.data = append(p.data, value...)
		p.data = append(p.data, '\n')
		p.haveData = true
	case "id":
		// Per spec an id containing NUL is ignored.
		if !bytes.ContainsRune(value, 0) {
			p.id = append(p.id[:0], value...)
			p.haveID = true
			// The resume token is retained even before the event dispatches, so
			// a connection dropped mid-event still resumes from it.
			if len(p.id) > 0 {
				p.lastID = append(p.lastID[:0], p.id...)
				p.lastIDStr = string(p.lastID)
			}
		}
	case "event":
		p.name = append(p.name[:0], value...)
	case "retry":
		if ms, err := strconv.Atoi(string(value)); err == nil && ms >= 0 {
			p.retry = time.Duration(ms) * time.Millisecond
		}
	default:
		// Unknown fields are ignored per spec.
	}
	return false, nil
}

// ErrOversizeLinef is a helper for wrapping the cap error with context.
func ErrOversizeLinef(n int) error {
	return fmt.Errorf("%w: %d bytes", ErrOversizeLine, n)
}
