package engine

import (
	"container/list"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// This file holds the dynamic-message plumbing shared by the reflection client
// and the gRPC executor, plus the two small utilities the hand-built descriptor
// in reflection_wire.go needs.

// Tiny pointer helpers. protobuf's generated code uses proto.String/proto.Int32,
// but naming those identically here would collide with the runtime package; the
// descriptor literal stays readable with short names instead.
func protoString(s string) *string { return &s }
func protoInt32(i int32) *int32    { return &i }
func protoBool(b bool) *bool       { return &b }

// DynamicMessage is a protobuf message with no Go type behind it. Everything
// Pulse knows about a message — its fields, their kinds, their nesting — comes
// from a descriptor fetched at runtime, so the same code path serves a service
// that did not exist when Pulse was compiled.
type DynamicMessage struct {
	msg *dynamicpb.Message
}

// NewDynamicMessage allocates a message of the descriptor's type.
func NewDynamicMessage(md protoreflect.MessageDescriptor) *DynamicMessage {
	return &DynamicMessage{msg: dynamicpb.NewMessage(md)}
}

// Message exposes the underlying dynamic message for the protobuf runtime.
func (d *DynamicMessage) Message() *dynamicpb.Message {
	if d == nil {
		return nil
	}
	return d.msg
}

// Descriptor reports the runtime descriptor backing this message.
func (d *DynamicMessage) Descriptor() protoreflect.MessageDescriptor {
	if d == nil || d.msg == nil {
		return nil
	}
	return d.msg.Descriptor()
}

// Proto returns the message as a proto.Message, which is what protojson and the
// gRPC codec both accept.
func (d *DynamicMessage) Proto() proto.Message {
	if d == nil {
		return nil
	}
	return d.msg
}

// Reset clears every field, returning the message to its zero state.
func (d *DynamicMessage) Reset() {
	if d != nil && d.msg != nil {
		proto.Reset(d.msg)
	}
}

// lru is a minimal insertion-ordered LRU used to bound the descriptor cache.
// A map plus a list is all the eviction policy needs; a dependency would be a
// worse trade than thirty lines here (AGENTS.md §2.2).
type lru struct {
	order   *list.List
	present map[string]*list.Element
	max     int
}

type lruEntry struct{ key string }

func newLRU(max int) *lru {
	if max <= 0 {
		max = 1
	}
	return &lru{order: list.New(), present: make(map[string]*list.Element, max), max: max}
}

func (l *lru) len() int { return l.order.Len() }

// push inserts or promotes a key, evicting the oldest when over capacity. It
// reports the evicted key, if any, so the caller can drop it from its map.
func (l *lru) push(key string) (evicted string, ok bool) {
	if el, exists := l.present[key]; exists {
		l.order.MoveToFront(el)
		return "", false
	}
	el := l.order.PushFront(&lruEntry{key: key})
	l.present[key] = el
	if l.order.Len() <= l.max {
		return "", false
	}
	back := l.order.Back()
	l.order.Remove(back)
	old := back.Value.(*lruEntry).key
	delete(l.present, old)
	return old, true
}

// evictOldest removes and returns the least-recently-used key.
func (l *lru) evictOldest() (string, bool) {
	back := l.order.Back()
	if back == nil {
		return "", false
	}
	l.order.Remove(back)
	key := back.Value.(*lruEntry).key
	delete(l.present, key)
	return key, true
}

// contains reports whether a key is resident.
func (l *lru) contains(key string) bool {
	_, ok := l.present[key]
	return ok
}

// get promotes a key to most-recent and reports whether it was present.
func (l *lru) get(key string) bool {
	el, ok := l.present[key]
	if !ok {
		return false
	}
	l.order.MoveToFront(el)
	return true
}

// ---- per-call scratch -------------------------------------------------------

// muPool hands out per-request bookkeeping so a burst of concurrent calls does
// not each allocate their own field-lookup table.
var muPool = sync.Pool{New: func() any { return &mappingState{} }}

// mappingState is per-call scratch space for the JSON→protobuf walk.
type mappingState struct {
	// fields caches descriptor lookups keyed by full message name, so walking
	// a large repeated message does not re-resolve the same descriptor for
	// every element.
	fields map[protoreflect.FullName]protoreflect.FieldDescriptors
}

func acquireMappingState() *mappingState {
	s := muPool.Get().(*mappingState)
	if s.fields == nil {
		s.fields = make(map[protoreflect.FullName]protoreflect.FieldDescriptors, 8)
	}
	return s
}

func releaseMappingState(s *mappingState) {
	clear(s.fields)
	muPool.Put(s)
}
