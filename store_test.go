package scopecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

func newItem(scope, id string, payload map[string]interface{}) Item {
	if payload == nil {
		payload = map[string]interface{}{"v": 1}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return Item{Scope: scope, ID: id, Payload: raw}
}

// --- ScopeBuffer.appendItem ---------------------------------------------------

func TestAppendItem_AssignsSeqMonotonically(t *testing.T) {
	buf := NewScopeBuffer(10)

	for i := 1; i <= 5; i++ {
		it, err := buf.appendItem(newItem("s", "", nil))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if it.Seq != uint64(i) {
			t.Fatalf("append %d: seq=%d want %d", i, it.Seq, i)
		}
	}
}

func TestAppendItem_RejectsDuplicateID(t *testing.T) {
	buf := NewScopeBuffer(10)

	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if _, err := buf.appendItem(newItem("s", "a", nil)); err == nil {
		t.Fatal("expected duplicate id rejection")
	}
}

func TestAppendItem_AllowsMultipleEmptyIDs(t *testing.T) {
	buf := NewScopeBuffer(10)

	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if len(buf.items) != 3 {
		t.Fatalf("len=%d want 3", len(buf.items))
	}
}

// Capacity is a hard cap: a write that would push the buffer past maxItems
// is rejected with ScopeFullError. No eviction happens — state, seq cursor
// and byID index stay exactly as they were before the failed append.
func TestAppendItem_RejectsAtCapacity(t *testing.T) {
	buf := NewScopeBuffer(3)

	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("pre-fill %d: %v", i, err)
		}
	}

	_, err := buf.appendItem(newItem("s", "overflow", nil))
	if err == nil {
		t.Fatal("expected ScopeFullError when appending past cap")
	}
	var sfe *ScopeFullError
	if !errors.As(err, &sfe) {
		t.Fatalf("expected *ScopeFullError, got %T: %v", err, err)
	}
	if sfe.Count != 3 || sfe.Cap != 3 {
		t.Fatalf("ScopeFullError{Count:%d Cap:%d}, want {3,3}", sfe.Count, sfe.Cap)
	}

	if len(buf.items) != 3 {
		t.Fatalf("rejected write mutated buffer: len=%d want 3", len(buf.items))
	}
	if buf.lastSeq != 3 {
		t.Fatalf("rejected write advanced lastSeq: got %d want 3", buf.lastSeq)
	}
	if _, ok := buf.byID["overflow"]; ok {
		t.Fatal("rejected id 'overflow' leaked into byID index")
	}
}

// --- ScopeBuffer.replaceAll ---------------------------------------------------

func TestReplaceAll_AssignsFreshSeqFromOne(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "", nil))
	_, _ = buf.appendItem(newItem("s", "", nil))

	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "b", nil),
	}
	_, err := buf.replaceAll(items)
	if err != nil {
		t.Fatal(err)
	}

	if buf.items[0].Seq != 1 || buf.items[1].Seq != 2 {
		t.Fatalf("seq not reset: %+v", buf.items)
	}
	if buf.lastSeq != 2 {
		t.Fatalf("lastSeq=%d want 2", buf.lastSeq)
	}
}

// replaceAll rejects the whole batch when it exceeds the per-scope cap —
// no silent truncation. Pre-existing state must stay untouched since the
// buffer is the mutation target and the caller expects all-or-nothing.
func TestReplaceAll_RejectsOverCap(t *testing.T) {
	buf := NewScopeBuffer(3)
	_, _ = buf.appendItem(newItem("s", "keep", nil))
	priorLen := len(buf.items)

	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "b", nil),
		newItem("s", "c", nil),
		newItem("s", "d", nil),
	}
	_, err := buf.replaceAll(items)
	if err == nil {
		t.Fatal("expected ScopeFullError when replacement exceeds cap")
	}
	var sfe *ScopeFullError
	if !errors.As(err, &sfe) {
		t.Fatalf("expected *ScopeFullError, got %T: %v", err, err)
	}
	if sfe.Count != 4 || sfe.Cap != 3 {
		t.Fatalf("ScopeFullError{Count:%d Cap:%d}, want {4,3}", sfe.Count, sfe.Cap)
	}
	if len(buf.items) != priorLen {
		t.Fatalf("rejected replaceAll mutated buffer: len=%d want %d", len(buf.items), priorLen)
	}
}

func TestReplaceAll_RejectsDuplicateIDs(t *testing.T) {
	buf := NewScopeBuffer(10)

	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "a", nil),
	}
	if _, err := buf.replaceAll(items); err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestReplaceAll_EmptyItemsClearsScope(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "", nil))

	if _, err := buf.replaceAll([]Item{}); err != nil {
		t.Fatal(err)
	}
	if len(buf.items) != 0 {
		t.Fatalf("expected empty buffer, got %d items", len(buf.items))
	}
}

// --- ScopeBuffer.updateByID ---------------------------------------------------

func TestUpdateByID_HitPreservesSeq(t *testing.T) {
	buf := NewScopeBuffer(10)
	original, _ := buf.appendItem(newItem("s", "a", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 2})
	n, err := buf.updateByID("a", newPayload, nil)
	if err != nil {
		t.Fatalf("updateByID: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated=%d want 1", n)
	}

	got, _ := buf.getByID("a")
	if got.Seq != original.Seq {
		t.Fatalf("seq changed: %d -> %d", original.Seq, got.Seq)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(got.Payload, &decoded); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if v, ok := decoded["v"].(float64); !ok || v != 2 {
		t.Fatalf("payload not updated: %s", string(got.Payload))
	}
}

func TestUpdateByID_Miss(t *testing.T) {
	buf := NewScopeBuffer(10)
	raw, _ := json.Marshal(map[string]interface{}{"v": 1})
	n, err := buf.updateByID("missing", raw, nil)
	if err != nil {
		t.Fatalf("updateByID: %v", err)
	}
	if n != 0 {
		t.Fatalf("updated=%d want 0", n)
	}
}

// --- ScopeBuffer.updateBySeq --------------------------------------------------

func TestUpdateBySeq_Hit(t *testing.T) {
	buf := NewScopeBuffer(10)
	it, _ := buf.appendItem(newItem("s", "", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 2})
	n, err := buf.updateBySeq(it.Seq, newPayload, nil)
	if err != nil {
		t.Fatalf("updateBySeq: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated=%d want 1", n)
	}

	got, _ := buf.getBySeq(it.Seq)
	var decoded map[string]interface{}
	if err := json.Unmarshal(got.Payload, &decoded); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	if v, ok := decoded["v"].(float64); !ok || v != 2 {
		t.Fatalf("payload not updated: %s", string(got.Payload))
	}
}

func TestUpdateBySeq_KeepsByIDIndexInSync(t *testing.T) {
	buf := NewScopeBuffer(10)
	it, _ := buf.appendItem(newItem("s", "a", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 42})
	if _, err := buf.updateBySeq(it.Seq, newPayload, nil); err != nil {
		t.Fatalf("updateBySeq: %v", err)
	}

	// byID index must reflect the new payload too, otherwise a /get by id
	// would return the pre-update payload.
	got, ok := buf.getByID("a")
	if !ok {
		t.Fatal("getByID missed after updateBySeq")
	}
	var decoded map[string]interface{}
	_ = json.Unmarshal(got.Payload, &decoded)
	if decoded["v"].(float64) != 42 {
		t.Fatalf("byID stale payload: %s", string(got.Payload))
	}
}

func TestUpdateBySeq_Miss(t *testing.T) {
	buf := NewScopeBuffer(10)
	raw, _ := json.Marshal(map[string]interface{}{"v": 1})
	n, err := buf.updateBySeq(999, raw, nil)
	if err != nil {
		t.Fatalf("updateBySeq: %v", err)
	}
	if n != 0 {
		t.Fatalf("updated=%d want 0", n)
	}
}

// --- ScopeBuffer.upsertByID ---------------------------------------------------

func TestUpsertByID_CreatesNewItem(t *testing.T) {
	buf := NewScopeBuffer(10)

	result, created, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 1}))
	if err != nil {
		t.Fatalf("upsertByID: %v", err)
	}
	if !created {
		t.Fatal("created=false on first upsert")
	}
	if result.Seq != 1 {
		t.Fatalf("seq=%d want 1", result.Seq)
	}
	if _, ok := buf.byID["a"]; !ok {
		t.Fatal("byID index missing new item")
	}
	if len(buf.items) != 1 {
		t.Fatalf("items len=%d want 1", len(buf.items))
	}
}

func TestUpsertByID_ReplacesPayloadAndPreservesSeq(t *testing.T) {
	buf := NewScopeBuffer(10)
	first, _, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 1}))
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	second, created, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 2}))
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if created {
		t.Fatal("created=true on replace")
	}
	if second.Seq != first.Seq {
		t.Fatalf("seq changed: %d -> %d", first.Seq, second.Seq)
	}
	if len(buf.items) != 1 {
		t.Fatalf("items len=%d want 1 (no duplicate inserted)", len(buf.items))
	}

	got, _ := buf.getByID("a")
	var decoded map[string]interface{}
	_ = json.Unmarshal(got.Payload, &decoded)
	if decoded["v"].(float64) != 2 {
		t.Fatalf("payload not replaced: %s", string(got.Payload))
	}
}

func TestUpsertByID_CoexistsWithAppend(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))

	result, created, err := buf.upsertByID(newItem("s", "b", map[string]interface{}{"v": 9}))
	if err != nil {
		t.Fatalf("upsertByID: %v", err)
	}
	if !created {
		t.Fatal("created=false for a fresh id")
	}
	if result.Seq != 2 {
		t.Fatalf("seq=%d want 2 (continuous with prior append)", result.Seq)
	}
}

func TestUpsertByID_RejectsAtCapacity(t *testing.T) {
	buf := NewScopeBuffer(2)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))

	_, _, err := buf.upsertByID(newItem("s", "c", nil))
	if err == nil {
		t.Fatal("expected ScopeFullError when upserting past cap")
	}
	var sfe *ScopeFullError
	if !errors.As(err, &sfe) {
		t.Fatalf("expected *ScopeFullError, got %T: %v", err, err)
	}

	// A replace must still succeed at capacity — only create hits the cap.
	if _, _, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 99})); err != nil {
		t.Fatalf("replace at cap should succeed: %v", err)
	}
}

// --- ScopeBuffer.counterAdd ---------------------------------------------------

func TestCounterAdd_CreatesWithStartingValue(t *testing.T) {
	buf := NewScopeBuffer(10)

	value, created, err := buf.counterAdd("views", "article_1", 1)
	if err != nil {
		t.Fatalf("counterAdd: %v", err)
	}
	if !created {
		t.Fatal("created=false on first call")
	}
	if value != 1 {
		t.Fatalf("value=%d want 1", value)
	}
	got, ok := buf.getByID("article_1")
	if !ok {
		t.Fatal("item not in byID index")
	}
	if string(got.Payload) != "1" {
		t.Fatalf("payload=%q want %q", string(got.Payload), "1")
	}
	if got.Seq != 1 {
		t.Fatalf("seq=%d want 1", got.Seq)
	}
}

func TestCounterAdd_IncrementsExistingCounter(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _, _ = buf.counterAdd("views", "article_1", 10)

	value, created, err := buf.counterAdd("views", "article_1", 5)
	if err != nil {
		t.Fatalf("counterAdd: %v", err)
	}
	if created {
		t.Fatal("created=true on existing counter")
	}
	if value != 15 {
		t.Fatalf("value=%d want 15", value)
	}
	got, _ := buf.getByID("article_1")
	if string(got.Payload) != "15" {
		t.Fatalf("payload=%q want %q", string(got.Payload), "15")
	}
	if got.Seq != 1 {
		t.Fatalf("seq changed: got %d want 1", got.Seq)
	}
}

func TestCounterAdd_NegativeByDecrements(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _, _ = buf.counterAdd("c", "k", 100)

	value, _, err := buf.counterAdd("c", "k", -40)
	if err != nil {
		t.Fatalf("counterAdd: %v", err)
	}
	if value != 60 {
		t.Fatalf("value=%d want 60", value)
	}
}

func TestCounterAdd_AllowsNegativeCreate(t *testing.T) {
	buf := NewScopeBuffer(10)

	value, created, err := buf.counterAdd("c", "k", -5)
	if err != nil {
		t.Fatalf("counterAdd: %v", err)
	}
	if !created {
		t.Fatal("created=false on fresh counter")
	}
	if value != -5 {
		t.Fatalf("value=%d want -5", value)
	}
}

// A payload that isn't a JSON number (e.g. an earlier /append of an HTML
// string or object) must not be silently overwritten — /counter_add returns
// a CounterPayloadError so the handler can map it to 409 Conflict.
func TestCounterAdd_RejectsNonNumericExisting(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(Item{Scope: "c", ID: "k", Payload: json.RawMessage(`"hello"`)})

	_, _, err := buf.counterAdd("c", "k", 1)
	if err == nil {
		t.Fatal("expected CounterPayloadError for string payload")
	}
	var cpe *CounterPayloadError
	if !errors.As(err, &cpe) {
		t.Fatalf("expected *CounterPayloadError, got %T: %v", err, err)
	}
}

func TestCounterAdd_RejectsFloatExisting(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(Item{Scope: "c", ID: "k", Payload: json.RawMessage(`3.14`)})

	_, _, err := buf.counterAdd("c", "k", 1)
	if err == nil {
		t.Fatal("expected CounterPayloadError for float payload")
	}
	var cpe *CounterPayloadError
	if !errors.As(err, &cpe) {
		t.Fatalf("expected *CounterPayloadError, got %T: %v", err, err)
	}
}

func TestCounterAdd_RejectsObjectExisting(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(Item{Scope: "c", ID: "k", Payload: json.RawMessage(`{"v":1}`)})

	_, _, err := buf.counterAdd("c", "k", 1)
	var cpe *CounterPayloadError
	if !errors.As(err, &cpe) {
		t.Fatalf("expected *CounterPayloadError, got %T: %v", err, err)
	}
}

func TestCounterAdd_RejectsOutOfRangeExisting(t *testing.T) {
	buf := NewScopeBuffer(10)
	// 2^53 — one above the allowed ±(2^53-1) range.
	_, _ = buf.appendItem(Item{Scope: "c", ID: "k", Payload: json.RawMessage(`9007199254740992`)})

	_, _, err := buf.counterAdd("c", "k", 1)
	var cpe *CounterPayloadError
	if !errors.As(err, &cpe) {
		t.Fatalf("expected *CounterPayloadError, got %T: %v", err, err)
	}
}

func TestCounterAdd_RejectsOverflow(t *testing.T) {
	buf := NewScopeBuffer(10)
	// Seed at max.
	_, _, _ = buf.counterAdd("c", "k", MaxCounterValue)

	_, _, err := buf.counterAdd("c", "k", 1)
	if err == nil {
		t.Fatal("expected CounterOverflowError when going past MaxCounterValue")
	}
	var coe *CounterOverflowError
	if !errors.As(err, &coe) {
		t.Fatalf("expected *CounterOverflowError, got %T: %v", err, err)
	}

	// Existing counter unchanged after rejected overflow.
	got, _ := buf.getByID("k")
	if string(got.Payload) != strconvFormatInt(MaxCounterValue) {
		t.Fatalf("counter mutated on overflow reject: %q", string(got.Payload))
	}
}

func TestCounterAdd_RejectsUnderflow(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _, _ = buf.counterAdd("c", "k", -MaxCounterValue)

	_, _, err := buf.counterAdd("c", "k", -1)
	var coe *CounterOverflowError
	if !errors.As(err, &coe) {
		t.Fatalf("expected *CounterOverflowError, got %T: %v", err, err)
	}
}

func TestCounterAdd_RejectsAtScopeCapacity(t *testing.T) {
	buf := NewScopeBuffer(1)
	_, _, _ = buf.counterAdd("c", "existing", 1)

	_, _, err := buf.counterAdd("c", "another", 1)
	if err == nil {
		t.Fatal("expected ScopeFullError when creating past cap")
	}
	var sfe *ScopeFullError
	if !errors.As(err, &sfe) {
		t.Fatalf("expected *ScopeFullError, got %T: %v", err, err)
	}

	// Increment of existing must still succeed at capacity — only create hits the cap.
	value, _, err := buf.counterAdd("c", "existing", 5)
	if err != nil {
		t.Fatalf("increment at cap should succeed: %v", err)
	}
	if value != 6 {
		t.Fatalf("value=%d want 6", value)
	}
}

// Helper: keep the test readable without importing strconv.
func strconvFormatInt(n int64) string {
	// json.Marshal on int64 gives the same decimal representation.
	b, _ := json.Marshal(n)
	return string(b)
}

// --- ScopeBuffer.deleteByID ---------------------------------------------------

func TestDeleteByID_Hit(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))

	n, _ := buf.deleteByID("a")
	if n != 1 {
		t.Fatalf("deleted=%d want 1", n)
	}
	if _, ok := buf.byID["a"]; ok {
		t.Fatal("id 'a' still in index")
	}
	if len(buf.items) != 1 {
		t.Fatalf("len=%d want 1", len(buf.items))
	}
}

func TestDeleteByID_Miss(t *testing.T) {
	buf := NewScopeBuffer(10)
	n, _ := buf.deleteByID("missing")
	if n != 0 {
		t.Fatalf("deleted=%d want 0", n)
	}
}

func TestDeleteByID_DoesNotRollbackLastSeq(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))

	_, _ = buf.deleteByID("b")
	next, _ := buf.appendItem(newItem("s", "c", nil))
	if next.Seq != 3 {
		t.Fatalf("seq=%d want 3 (no rollback)", next.Seq)
	}
}

// --- ScopeBuffer.deleteBySeq --------------------------------------------------

func TestDeleteBySeq_Hit(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	it2, _ := buf.appendItem(newItem("s", "b", nil))
	_, _ = buf.appendItem(newItem("s", "c", nil))

	n, _ := buf.deleteBySeq(it2.Seq)
	if n != 1 {
		t.Fatalf("deleted=%d want 1", n)
	}
	if _, ok := buf.bySeq[it2.Seq]; ok {
		t.Fatal("seq still in bySeq index")
	}
	if _, ok := buf.byID["b"]; ok {
		t.Fatal("id 'b' still in byID index")
	}
	if len(buf.items) != 2 {
		t.Fatalf("len=%d want 2", len(buf.items))
	}
}

func TestDeleteBySeq_Miss(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))

	if n, _ := buf.deleteBySeq(999); n != 0 {
		t.Fatalf("deleted=%d want 0", n)
	}
	if len(buf.items) != 1 {
		t.Fatalf("len=%d want 1", len(buf.items))
	}
}

func TestDeleteBySeq_NoIDItem(t *testing.T) {
	buf := NewScopeBuffer(10)
	it, _ := buf.appendItem(newItem("s", "", nil))

	if n, _ := buf.deleteBySeq(it.Seq); n != 1 {
		t.Fatalf("deleted=%d want 1", n)
	}
	if len(buf.items) != 0 {
		t.Fatalf("len=%d want 0", len(buf.items))
	}
}

func TestDeleteBySeq_DoesNotRollbackLastSeq(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	it2, _ := buf.appendItem(newItem("s", "b", nil))

	_, _ = buf.deleteBySeq(it2.Seq)
	next, _ := buf.appendItem(newItem("s", "c", nil))
	if next.Seq != 3 {
		t.Fatalf("seq=%d want 3 (no rollback)", next.Seq)
	}
}

// --- ScopeBuffer.deleteUpToSeq ---------------------------------------------

func TestDeleteUpToSeq_RemovesPrefix(t *testing.T) {
	buf := NewScopeBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	n, _ := buf.deleteUpToSeq(3)
	if n != 3 {
		t.Fatalf("deleted=%d want 3", n)
	}
	if len(buf.items) != 2 {
		t.Fatalf("len=%d want 2", len(buf.items))
	}
	if buf.items[0].Seq != 4 || buf.items[1].Seq != 5 {
		t.Fatalf("unexpected survivors: %+v", buf.items)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if _, ok := buf.bySeq[seq]; ok {
			t.Fatalf("seq %d should be gone from bySeq", seq)
		}
	}
}

func TestDeleteUpToSeq_RemovesIDsToo(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))
	_, _ = buf.appendItem(newItem("s", "c", nil))

	_, _ = buf.deleteUpToSeq(2)

	if _, ok := buf.byID["a"]; ok {
		t.Fatal("id 'a' should have been removed from byID")
	}
	if _, ok := buf.byID["b"]; ok {
		t.Fatal("id 'b' should have been removed from byID")
	}
	if _, ok := buf.byID["c"]; !ok {
		t.Fatal("id 'c' should still exist")
	}
}

func TestDeleteUpToSeq_NoOpBelowRange(t *testing.T) {
	buf := NewScopeBuffer(5)
	// Append 3 items, then delete through the prefix before seq 3's start.
	// Nothing matches because no item has seq <= 0.
	for i := 1; i <= 3; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}
	// Drop seqs 1..2 first to simulate a post-drain state, then ask to drop
	// anything <= 2 again. The cut point is already past — expect no-op.
	_, _ = buf.deleteUpToSeq(2)

	n, _ := buf.deleteUpToSeq(2)
	if n != 0 {
		t.Fatalf("deleted=%d want 0 (no items at or below seq 2 remain)", n)
	}
	if len(buf.items) != 1 {
		t.Fatalf("len=%d want 1", len(buf.items))
	}
}

func TestDeleteUpToSeq_RemovesAllWhenMaxAtOrAboveLast(t *testing.T) {
	buf := NewScopeBuffer(10)
	for i := 1; i <= 3; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	n, _ := buf.deleteUpToSeq(99)
	if n != 3 {
		t.Fatalf("deleted=%d want 3", n)
	}
	if len(buf.items) != 0 {
		t.Fatalf("expected empty scope, got %d", len(buf.items))
	}
}

func TestDeleteUpToSeq_DoesNotRollbackLastSeq(t *testing.T) {
	buf := NewScopeBuffer(10)
	for i := 1; i <= 3; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	_, _ = buf.deleteUpToSeq(3)
	next, _ := buf.appendItem(newItem("s", "", nil))
	if next.Seq != 4 {
		t.Fatalf("seq=%d want 4 (no rollback after draining)", next.Seq)
	}
}

func TestDeleteUpToSeq_ReleasesBackingArray(t *testing.T) {
	// Fill a scope well past its natural grow-cycle so the backing array
	// has capacity noticeably larger than the survivors. Drain the prefix
	// and assert the backing array was reallocated to match the remainder
	// — that is the guarantee that frees the removed-payload memory for
	// GC in the write-buffer drain-from-front pattern.
	const fill = 1000
	buf := NewScopeBuffer(fill * 2)
	for i := 0; i < fill; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}
	preCap := cap(buf.items)
	if preCap < fill {
		t.Fatalf("sanity: pre-drain cap=%d want >= %d", preCap, fill)
	}

	drained, _ := buf.deleteUpToSeq(uint64(fill - 10))
	if drained != fill-10 {
		t.Fatalf("drained=%d want %d", drained, fill-10)
	}
	if len(buf.items) != 10 {
		t.Fatalf("len=%d want 10", len(buf.items))
	}
	if cap(buf.items) != len(buf.items) {
		t.Fatalf("backing array not released: cap=%d len=%d (pre-drain cap was %d)",
			cap(buf.items), len(buf.items), preCap)
	}
}

// --- ScopeBuffer.tailOffset ---------------------------------------------------

func TestTailOffset_BasicAndEdges(t *testing.T) {
	buf := NewScopeBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	tests := []struct {
		limit, offset int
		wantSeq       []uint64
	}{
		{2, 0, []uint64{4, 5}},
		{2, 2, []uint64{2, 3}},
		{10, 0, []uint64{1, 2, 3, 4, 5}},
		{2, 10, nil},
	}

	for _, tc := range tests {
		got, _ := buf.tailOffset(tc.limit, tc.offset)
		if len(got) != len(tc.wantSeq) {
			t.Errorf("tail(limit=%d offset=%d): len=%d want %d", tc.limit, tc.offset, len(got), len(tc.wantSeq))
			continue
		}
		for i, seq := range tc.wantSeq {
			if got[i].Seq != seq {
				t.Errorf("tail(limit=%d offset=%d)[%d].seq=%d want %d", tc.limit, tc.offset, i, got[i].Seq, seq)
			}
		}
	}
}

// --- ScopeBuffer.sinceSeq -----------------------------------------------------

func TestSinceSeq_ReturnsItemsAfterCursor(t *testing.T) {
	buf := NewScopeBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	got, _ := buf.sinceSeq(2, 0)
	if len(got) != 3 {
		t.Fatalf("len=%d want 3", len(got))
	}
	if got[0].Seq != 3 {
		t.Fatalf("first.seq=%d want 3", got[0].Seq)
	}
}

func TestSinceSeq_RespectsLimit(t *testing.T) {
	buf := NewScopeBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	got, _ := buf.sinceSeq(0, 2)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
}

func TestSinceSeq_EmptyWhenPastEnd(t *testing.T) {
	buf := NewScopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "", nil))

	got, _ := buf.sinceSeq(100, 0)
	if len(got) != 0 {
		t.Fatalf("len=%d want 0", len(got))
	}
}

// --- ScopeBuffer.getByID / getBySeq -------------------------------------------

func TestGetByIDAndSeq(t *testing.T) {
	buf := NewScopeBuffer(10)
	it, _ := buf.appendItem(newItem("s", "a", nil))

	if got, ok := buf.getByID("a"); !ok || got.Seq != it.Seq {
		t.Fatalf("getByID: ok=%v seq=%d want %d", ok, got.Seq, it.Seq)
	}
	if got, ok := buf.getBySeq(it.Seq); !ok || got.ID != "a" {
		t.Fatalf("getBySeq: ok=%v id=%q", ok, got.ID)
	}
	if _, ok := buf.getByID("missing"); ok {
		t.Fatal("getByID('missing') should miss")
	}
	if _, ok := buf.getBySeq(999); ok {
		t.Fatal("getBySeq(999) should miss")
	}
}

// --- Store --------------------------------------------------------------------

func TestStore_GetOrCreateScope_RequiresScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, err := s.getOrCreateScope(""); err == nil {
		t.Fatal("expected error for empty scope")
	}
}

func TestStore_GetOrCreateScope_ReturnsSameBuffer(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	b1, _ := s.getOrCreateScope("x")
	b2, _ := s.getOrCreateScope("x")
	if b1 != b2 {
		t.Fatal("scope buffers should be identical")
	}
}

// Race regression: /warm's Phase 1.5 reservation and Phase 2 commit must
// serialise against /wipe and /rebuild. Pre-fix /wipe's Swap(0) (or
// /rebuild's Store) erased a /warm reservation, and the drift comp in
// commitReplacementPreReserved then over-credited totalBytes by the
// snapshot's oldBytes — leaving the store-wide invariant
// (totalBytes == Σ buf.bytes) violated.
//
// The race window is tiny (a few ns between reserveBytes and the per-
// scope commit), so a naive "fire both goroutines and see what happens"
// test mostly observes one finishing before the other starts. Stress
// with many iterations to make the race probabilistic. With the fix in
// place every iteration should pass.
func TestStore_ReplaceScopes_RaceVsWipe(t *testing.T) {
	const iterations = 5000

	for i := 0; i < iterations; i++ {
		s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

		// Seed multiple scopes so /warm has multiple oldBytes snapshots and
		// a wider Phase 1.5 → Phase 2 window for /wipe to slip into.
		seed := map[string][]Item{}
		for s2 := 0; s2 < 5; s2++ {
			scope := "s" + string(rune('0'+s2))
			items := []Item{}
			for j := 0; j < 4; j++ {
				items = append(items, Item{Scope: scope, ID: "k" + string(rune('0'+j)), Payload: jsonRaw(`{"v":1}`)})
			}
			seed[scope] = items
		}
		if _, err := s.replaceScopes(seed); err != nil {
			t.Fatalf("seed: %v", err)
		}

		// Concurrent /warm (replace all 5 scopes with smaller payloads) + /wipe.
		// Either order is legal; what's NOT legal is them interleaving in a
		// way that leaves totalBytes != Σ buf.bytes.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			repl := map[string][]Item{}
			for s2 := 0; s2 < 5; s2++ {
				scope := "s" + string(rune('0'+s2))
				repl[scope] = []Item{{Scope: scope, ID: "newkey", Payload: jsonRaw(`{"v":42}`)}}
			}
			_, _ = s.replaceScopes(repl)
		}()
		go func() {
			defer wg.Done()
			s.wipe()
		}()
		wg.Wait()

		assertBytesInvariant(t, s, i, "warm-vs-wipe")
	}
}

func TestStore_ReplaceScopes_RaceVsRebuild(t *testing.T) {
	const iterations = 5000

	for i := 0; i < iterations; i++ {
		s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

		seed := map[string][]Item{}
		for s2 := 0; s2 < 5; s2++ {
			scope := "s" + string(rune('0'+s2))
			items := []Item{}
			for j := 0; j < 4; j++ {
				items = append(items, Item{Scope: scope, ID: "k" + string(rune('0'+j)), Payload: jsonRaw(`{"v":1}`)})
			}
			seed[scope] = items
		}
		if _, err := s.replaceScopes(seed); err != nil {
			t.Fatalf("seed: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			repl := map[string][]Item{}
			for s2 := 0; s2 < 5; s2++ {
				scope := "s" + string(rune('0'+s2))
				repl[scope] = []Item{{Scope: scope, ID: "newkey", Payload: jsonRaw(`{"v":42}`)}}
			}
			_, _ = s.replaceScopes(repl)
		}()
		go func() {
			defer wg.Done()
			rebuildItems := map[string][]Item{
				"new-after-rebuild": {{Scope: "new-after-rebuild", ID: "x", Payload: jsonRaw(`{"v":99}`)}},
			}
			_, _, _ = s.rebuildAll(rebuildItems)
		}()
		wg.Wait()

		assertBytesInvariant(t, s, i, "warm-vs-rebuild")
	}
}

func assertBytesInvariant(t *testing.T, s *Store, iter int, label string) {
	t.Helper()

	var sum int64
	var scopeCount int
	for shIdx := range s.shards {
		s.shards[shIdx].mu.RLock()
		scopeCount += len(s.shards[shIdx].scopes)
		for _, buf := range s.shards[shIdx].scopes {
			buf.mu.RLock()
			sum += buf.bytes
			buf.mu.RUnlock()
		}
		s.shards[shIdx].mu.RUnlock()
	}

	// Since v0.5.14 totalBytes also includes scopeBufferOverhead per
	// allocated scope (admission control sees the real per-scope memory
	// cost). Σ buf.bytes is item-bytes only, so add the overhead per
	// surviving scope to get the expected counter value.
	expected := sum + int64(scopeCount)*scopeBufferOverhead
	total := s.totalBytes.Load()
	if total != expected {
		t.Errorf("[%s iter %d] totalBytes=%d but expected=%d (Σ buf.bytes=%d + %d×%d overhead, drift=%d)",
			label, iter, total, expected, sum, scopeCount, scopeBufferOverhead, total-expected)
	}
}

func jsonRaw(s string) []byte { return []byte(s) }

// NewStore(Config{}) must produce a usable Store. Pre-fix the zero
// Config carried zero caps to every field, so any positive write
// failed with StoreFullError or worse — the public package was
// effectively dead-on-arrival for library users.
func TestNewStore_ZeroConfigUsesDefaults(t *testing.T) {
	s := NewStore(Config{})
	api := NewAPI(s)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// A normal /append must just work.
	body := `{"scope":"smoke","id":"a","payload":{"v":1}}`
	code, _, raw := doRequest(t, mux, "POST", "/append", body)
	if code != 200 {
		t.Fatalf("/append on default-config Store: code=%d body=%s", code, raw)
	}

	// Caps must match the package-level compile-time defaults.
	if s.defaultMaxItems != ScopeMaxItems {
		t.Errorf("defaultMaxItems=%d want %d", s.defaultMaxItems, ScopeMaxItems)
	}
	if s.maxStoreBytes != int64(MaxStoreMiB)<<20 {
		t.Errorf("maxStoreBytes=%d want %d", s.maxStoreBytes, int64(MaxStoreMiB)<<20)
	}
	if s.maxItemBytes != int64(MaxItemBytes) {
		t.Errorf("maxItemBytes=%d want %d", s.maxItemBytes, int64(MaxItemBytes))
	}
	if s.maxResponseBytes != int64(MaxResponseMiB)<<20 {
		t.Errorf("maxResponseBytes=%d want %d", s.maxResponseBytes, int64(MaxResponseMiB)<<20)
	}
	if s.maxMultiCallBytes != int64(MaxMultiCallMiB)<<20 {
		t.Errorf("maxMultiCallBytes=%d want %d", s.maxMultiCallBytes, int64(MaxMultiCallMiB)<<20)
	}
	if s.maxMultiCallCount != MaxMultiCallCount {
		t.Errorf("maxMultiCallCount=%d want %d", s.maxMultiCallCount, MaxMultiCallCount)
	}
}

// Config.WithDefaults treats <= 0 as "use default" (matching the
// standalone binary's env-var helpers) but leaves explicit positive
// values alone. ServerSecret is untouched: empty disables /guarded
// by design, not by accident.
func TestConfig_WithDefaults(t *testing.T) {
	t.Run("zero fields fall back to defaults", func(t *testing.T) {
		got := Config{}.WithDefaults()
		if got.ScopeMaxItems != ScopeMaxItems {
			t.Errorf("ScopeMaxItems=%d", got.ScopeMaxItems)
		}
		if got.MaxStoreBytes != int64(MaxStoreMiB)<<20 {
			t.Errorf("MaxStoreBytes=%d", got.MaxStoreBytes)
		}
		if got.MaxItemBytes != int64(MaxItemBytes) {
			t.Errorf("MaxItemBytes=%d", got.MaxItemBytes)
		}
		if got.MaxResponseBytes != int64(MaxResponseMiB)<<20 {
			t.Errorf("MaxResponseBytes=%d", got.MaxResponseBytes)
		}
		if got.MaxMultiCallBytes != int64(MaxMultiCallMiB)<<20 {
			t.Errorf("MaxMultiCallBytes=%d", got.MaxMultiCallBytes)
		}
		if got.MaxMultiCallCount != MaxMultiCallCount {
			t.Errorf("MaxMultiCallCount=%d", got.MaxMultiCallCount)
		}
	})

	t.Run("positive fields preserved", func(t *testing.T) {
		in := Config{
			ScopeMaxItems:     5,
			MaxStoreBytes:     7,
			MaxItemBytes:      11,
			MaxResponseBytes:  13,
			MaxMultiCallBytes: 17,
			MaxMultiCallCount: 19,
			ServerSecret:      "real-secret",
		}
		got := in.WithDefaults()
		if got.ScopeMaxItems != in.ScopeMaxItems ||
			got.MaxStoreBytes != in.MaxStoreBytes ||
			got.MaxItemBytes != in.MaxItemBytes ||
			got.MaxResponseBytes != in.MaxResponseBytes ||
			got.MaxMultiCallBytes != in.MaxMultiCallBytes ||
			got.MaxMultiCallCount != in.MaxMultiCallCount ||
			got.ServerSecret != in.ServerSecret {
			t.Errorf("positive Config mutated: got %+v want %+v", got, in)
		}
	})

	t.Run("negative treated as zero", func(t *testing.T) {
		// Same lenient policy as the standalone env-var helpers (n<=0 → default).
		// The Caddy module rejects negatives explicitly via validateConfig
		// before even calling NewStore, so this path only fires for direct
		// library callers — friendlier to fall back than to crash.
		got := Config{ScopeMaxItems: -1, MaxStoreBytes: -100}.WithDefaults()
		if got.ScopeMaxItems != ScopeMaxItems {
			t.Errorf("negative ScopeMaxItems not defaulted: %d", got.ScopeMaxItems)
		}
		if got.MaxStoreBytes != int64(MaxStoreMiB)<<20 {
			t.Errorf("negative MaxStoreBytes not defaulted: %d", got.MaxStoreBytes)
		}
	})

	t.Run("empty server_secret stays empty (kill-switch)", func(t *testing.T) {
		got := Config{ServerSecret: ""}.WithDefaults()
		if got.ServerSecret != "" {
			t.Errorf("ServerSecret got %q want empty", got.ServerSecret)
		}
	})
}

// Empty-scope spam — a malicious client with auto-create access
// (e.g., a /guarded tenant under v0.5.12+) creating thousands of
// empty scopes — must eventually hit the store-byte cap. Pre-v0.5.14
// this attack was unbounded: scope-buffer overhead was not charged
// against totalBytes, so 1M empty scopes consumed ~1 GiB while
// approx_store_mb stayed at 0. This test anchors the bound.
func TestStore_EmptyScopeSpam_HitsByteCap(t *testing.T) {
	// Cap big enough for ~10 scopes' worth of overhead, no item room.
	capBytes := int64(scopeBufferOverhead) * 10
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	created := 0
	var lastErr error
	for i := 0; i < 100; i++ {
		_, err := s.getOrCreateScope(fmt.Sprintf("scope_%d", i))
		if err != nil {
			lastErr = err
			break
		}
		created++
	}

	if created >= 100 {
		t.Fatalf("created %d empty scopes without hitting cap (cap=%d, overhead=%d)",
			created, capBytes, scopeBufferOverhead)
	}
	if lastErr == nil {
		t.Fatal("expected StoreFullError after the cap is reached")
	}
	var stfe *StoreFullError
	if !errors.As(lastErr, &stfe) {
		t.Errorf("expected *StoreFullError, got %T: %v", lastErr, lastErr)
	}

	// totalBytes equals exactly created × overhead — a clean accounting
	// of pure scope-buffer cost, no items in any scope.
	wantBytes := int64(created) * scopeBufferOverhead
	if got := s.totalBytes.Load(); got != wantBytes {
		t.Errorf("totalBytes=%d want %d (created=%d × overhead=%d)",
			got, wantBytes, created, scopeBufferOverhead)
	}
}

// /delete_scope must release the per-scope overhead, not just the
// items. Without this, a workload that churns scopes (create, fill,
// delete, repeat) would slowly leak overhead and eventually 507 even
// when the store looks empty.
func TestStore_DeleteScope_ReleasesOverhead(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: int64(scopeBufferOverhead) * 5, MaxItemBytes: 1 << 20})

	// Fill the cap with empty scopes — 5 scopes × overhead = cap.
	for i := 0; i < 5; i++ {
		if _, err := s.getOrCreateScope(fmt.Sprintf("s_%d", i)); err != nil {
			t.Fatalf("getOrCreateScope %d: %v", i, err)
		}
	}
	// 6th must fail.
	if _, err := s.getOrCreateScope("s_overflow"); err == nil {
		t.Fatal("expected StoreFullError at scope #6")
	}

	// Delete one scope — its overhead is released.
	if _, ok := s.deleteScope("s_0"); !ok {
		t.Fatal("deleteScope s_0 reported miss")
	}

	// Now there's room for one more.
	if _, err := s.getOrCreateScope("s_replaced"); err != nil {
		t.Fatalf("getOrCreateScope after delete: %v", err)
	}

	// totalBytes is still exactly 5 × overhead.
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)*5; got != want {
		t.Errorf("totalBytes=%d want %d after delete+create cycle", got, want)
	}
}

func TestStore_GetScope_Miss(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, ok := s.getScope("nope"); ok {
		t.Fatal("expected miss")
	}
}

// AppendOne, UpsertOne, CounterAddOne must roll back the freshly-created
// scope when the item-byte reservation fails. Without rollback, every
// failed write to a new scope would leak scopeBufferOverhead onto the
// store-byte cap — a multi-tenant attacker could fill the cap with
// empty scopes and DoS legitimate writers (see ChatGPT bug review).

// bigPayload returns a JSON string payload of approximately the given
// total byte count. Used by the rollback tests to push past the
// store-byte cap without hitting the per-item cap.
func bigPayload(n int) json.RawMessage {
	buf := make([]byte, n+2)
	buf[0] = '"'
	for i := 1; i < n+1; i++ {
		buf[i] = 'a'
	}
	buf[n+1] = '"'
	return json.RawMessage(buf)
}

func TestStore_AppendOne_RollsBackEmptyScopeOnFailure(t *testing.T) {
	// Cap = overhead + 50 bytes. AppendOne reserves overhead first,
	// then the item-bytes reservation overflows — scope must be
	// rolled back so the overhead is released.
	capBytes := int64(scopeBufferOverhead) + 50
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	bigItem := Item{Scope: "victim", ID: "x", Payload: bigPayload(200)}
	_, err := s.AppendOne(bigItem)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after rolled-back AppendOne; want 0", got)
	}
	if _, ok := s.getScope("victim"); ok {
		t.Errorf("scope 'victim' still present in s.scopes after rollback")
	}
}

func TestStore_UpsertOne_RollsBackEmptyScopeOnFailure(t *testing.T) {
	capBytes := int64(scopeBufferOverhead) + 50
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	bigItem := Item{Scope: "victim", ID: "x", Payload: bigPayload(200)}
	_, _, err := s.UpsertOne(bigItem)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after rolled-back UpsertOne; want 0", got)
	}
	if _, ok := s.getScope("victim"); ok {
		t.Errorf("scope 'victim' still present in s.scopes after rollback")
	}
}

func TestStore_CounterAddOne_RollsBackEmptyScopeOnFailure(t *testing.T) {
	// Cap = overhead + 1 byte. Even the smallest counter payload
	// (a one-digit integer) overflows on the item-bytes reservation
	// after the per-scope overhead has been claimed.
	capBytes := int64(scopeBufferOverhead) + 1
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	_, _, err := s.CounterAddOne("victim", "c1", 42)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after rolled-back CounterAddOne; want 0", got)
	}
	if _, ok := s.getScope("victim"); ok {
		t.Errorf("scope 'victim' still present in s.scopes after rollback")
	}
}

// AppendOne loop with new scope names + oversized items must not leak
// per-scope overhead. Without the rollback this is the multi-tenant
// DoS path: ~100k requests fill the default 100 MiB cap with empty
// scopes, after which all legitimate writes 507.
func TestStore_AppendOne_DoSPathStaysClean(t *testing.T) {
	capBytes := int64(scopeBufferOverhead) + 50
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	for i := 0; i < 1000; i++ {
		item := Item{Scope: fmt.Sprintf("attempt_%d", i), ID: "x", Payload: bigPayload(200)}
		_, err := s.AppendOne(item)
		var stfe *StoreFullError
		if !errors.As(err, &stfe) {
			t.Fatalf("iter %d: expected StoreFullError, got %T: %v", i, err, err)
		}
	}

	var scopeCount int
	for shIdx := range s.shards {
		s.shards[shIdx].mu.RLock()
		scopeCount += len(s.shards[shIdx].scopes)
		s.shards[shIdx].mu.RUnlock()
	}
	if scopeCount != 0 {
		t.Errorf("after 1000 failed AppendOne calls, %d empty scopes leaked", scopeCount)
	}
	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after 1000 rolled-back AppendOne calls; want 0", got)
	}
}

// AppendOne must NOT roll back the scope when a concurrent caller has
// successfully committed an item to the same scope between our create
// and our cleanup. The cleanup helper checks len(buf.items)==0 under
// buf.mu, so a successful concurrent write keeps the scope alive.
//
// Race-detector-friendly: pairs of goroutines per scope — one tries an
// oversized write, the other a small write. No empty scopes may leak.
func TestStore_AppendOne_ConcurrentSuccessSurvivesCleanup(t *testing.T) {
	const N = 50
	// Cap room for N small items + their scope overheads, plus slack
	// for the oversized writers' interleaving overhead-reservations.
	capBytes := int64(N) * (int64(scopeBufferOverhead) + 256)
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(2)
		scope := fmt.Sprintf("shared_%d", i)
		go func(scope string) {
			defer wg.Done()
			big := Item{Scope: scope, ID: "big", Payload: bigPayload(int(capBytes))}
			_, _ = s.AppendOne(big)
		}(scope)
		go func(scope string) {
			defer wg.Done()
			small := Item{Scope: scope, ID: "small", Payload: json.RawMessage(`"hi"`)}
			_, _ = s.AppendOne(small)
		}(scope)
	}
	wg.Wait()

	for shIdx := range s.shards {
		s.shards[shIdx].mu.RLock()
		for name, buf := range s.shards[shIdx].scopes {
			buf.mu.Lock()
			empty := len(buf.items) == 0
			buf.mu.Unlock()
			if empty {
				t.Errorf("empty scope %q leaked through concurrent cleanup", name)
			}
		}
		s.shards[shIdx].mu.RUnlock()
	}
}

func TestStore_EnsureScope_CreatesEmpty(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	buf := s.ensureScope("_counters_count_calls")
	if buf == nil {
		t.Fatal("ensureScope returned nil")
	}
	if got, ok := s.getScope("_counters_count_calls"); !ok || got != buf {
		t.Fatal("scope not registered or different buffer returned")
	}
	if n := len(buf.items); n != 0 {
		t.Errorf("new scope should be empty, got %d items", n)
	}
}

// ensureScope must charge scopeBufferOverhead against totalBytes so a
// later /admin /delete_scope releases exactly what was reserved.
// Without this, deleteScope's unconditional `-(scopeBytes + overhead)`
// would underflow totalBytes by 1024 bytes per cycle on these
// internal counter scopes — bounded, but a real invariant break.
func TestStore_EnsureScope_ReservesOverheadAndRoundTrips(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	before := s.totalBytes.Load()
	buf := s.ensureScope("_counters_count_calls")
	if buf == nil {
		t.Fatal("ensureScope returned nil with ample cap")
	}
	if got := s.totalBytes.Load() - before; got != int64(scopeBufferOverhead) {
		t.Fatalf("ensureScope reserved %d bytes; want %d (scopeBufferOverhead)", got, scopeBufferOverhead)
	}

	if _, ok := s.deleteScope("_counters_count_calls"); !ok {
		t.Fatal("deleteScope reported miss on the freshly ensured scope")
	}
	if got := s.totalBytes.Load(); got != before {
		t.Errorf("totalBytes drift after ensureScope+deleteScope round-trip: got=%d want=%d", got, before)
	}
}

// On cap exhaustion ensureScope must return nil, not panic and not
// double-charge — guardedIncrementCounters is best-effort and skips
// silently on nil, so observability counters never block legitimate
// /guarded calls.
func TestStore_EnsureScope_NilOnCapExhausted(t *testing.T) {
	// Cap = 100 bytes, well below scopeBufferOverhead (1024).
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100, MaxItemBytes: 1 << 20})

	if buf := s.ensureScope("_counters_count_calls"); buf != nil {
		t.Errorf("ensureScope returned %p with cap below overhead; want nil", buf)
	}
	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after failed ensureScope; want 0 (no leak)", got)
	}
	if _, ok := s.getScope("_counters_count_calls"); ok {
		t.Errorf("ensureScope leaked the scope into s.scopes despite cap-fail")
	}
}

func TestStore_EnsureScope_Idempotent(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	b1 := s.ensureScope("_counters_count_calls")
	b2 := s.ensureScope("_counters_count_calls")
	if b1 != b2 {
		t.Fatal("repeat ensureScope should return same buffer")
	}
}

// ensureScope under concurrent access must not double-create or panic.
func TestStore_EnsureScope_Concurrent(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	const N = 50
	bufs := make([]*ScopeBuffer, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			bufs[idx] = s.ensureScope("_counters_count_calls")
		}(i)
	}
	wg.Wait()

	first := bufs[0]
	for i, b := range bufs {
		if b != first {
			t.Errorf("ensureScope returned different buffer at idx %d", i)
		}
	}
}

// ensureScope on already-existing scope must not wipe its items.
func TestStore_EnsureScope_PreservesExisting(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("_counters_count_calls")
	if _, _, err := buf.counterAdd("_counters_count_calls", "cap1", 42); err != nil {
		t.Fatalf("counterAdd: %v", err)
	}
	again := s.ensureScope("_counters_count_calls")
	if again != buf {
		t.Fatal("ensureScope returned different buffer")
	}
	if got, _, err := again.counterAdd("_counters_count_calls", "cap1", 0); err != nil {
		// counterAdd with by=0 isn't allowed by /counter_add validation, but at
		// the buffer level it should still let us read the existing value via
		// a noop add — except that the buffer rejects zero too. So instead
		// just check items length.
		_ = got
		_ = err
	}
	if n := len(again.items); n != 1 {
		t.Errorf("expected 1 existing item preserved, got %d", n)
	}
}

func TestStore_DeleteScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("x")
	_, _ = buf.appendItem(newItem("x", "a", nil))
	_, _ = buf.appendItem(newItem("x", "b", nil))

	n, ok := s.deleteScope("x")
	if !ok || n != 2 {
		t.Fatalf("deleteScope: ok=%v n=%d", ok, n)
	}
	if _, found := s.getScope("x"); found {
		t.Fatal("scope should be gone")
	}

	n, ok = s.deleteScope("missing")
	if ok || n != 0 {
		t.Fatalf("deleteScope(missing): ok=%v n=%d", ok, n)
	}

	// Empty scope is a shape bug from the caller — the store refuses it up
	// front rather than walking the map for a key that cannot exist.
	n, ok = s.deleteScope("")
	if ok || n != 0 {
		t.Fatalf("deleteScope(\"\"): ok=%v n=%d", ok, n)
	}
}

// --- Store.wipe ---------------------------------------------------------------

func TestStore_Wipe_EmptyStore(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	scopes, items, freed := s.wipe()
	if scopes != 0 || items != 0 || freed != 0 {
		t.Fatalf("wipe empty: scopes=%d items=%d freed=%d want 0,0,0", scopes, items, freed)
	}
}

func TestStore_Wipe_RemovesEveryScopeAndCountsItems(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	for _, name := range []string{"a", "b", "c"} {
		buf, _ := s.getOrCreateScope(name)
		for i := 0; i < 4; i++ {
			if _, err := buf.appendItem(newItem(name, "", nil)); err != nil {
				t.Fatalf("append %s/%d: %v", name, i, err)
			}
		}
	}

	scopes, items, freed := s.wipe()
	if scopes != 3 || items != 12 {
		t.Fatalf("wipe: scopes=%d items=%d want 3,12", scopes, items)
	}
	if freed <= 0 {
		t.Fatalf("wipe: freed=%d want >0", freed)
	}

	for _, name := range []string{"a", "b", "c"} {
		if _, ok := s.getScope(name); ok {
			t.Errorf("scope %q should be gone after wipe", name)
		}
	}
}

// wipe must bring s.totalBytes exactly back to zero so the next append's
// reservation starts from a clean baseline. This is the property /wipe
// promises to clients that are about to /rebuild into a freshly empty store.
func TestStore_Wipe_ResetsTotalBytes(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	buf, _ := s.getOrCreateScope("s")
	for i := 0; i < 5; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if s.totalBytes.Load() == 0 {
		t.Fatal("totalBytes should be non-zero before wipe")
	}

	_, _, freed := s.wipe()
	if got := s.totalBytes.Load(); got != 0 {
		t.Fatalf("totalBytes=%d want 0 after wipe", got)
	}
	if freed == 0 {
		t.Fatal("freed bytes reported as 0 despite non-empty store")
	}
}

// After /wipe the next /append must succeed even when the pre-wipe store
// was at its byte cap — the cap budget has been fully released.
func TestStore_Wipe_FreesHeadroomForNextAppend(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	// Cap room for: per-scope overhead (allocated by getOrCreateScope)
	// + 3 items. The fourth item then exceeds the cap.
	capBytes := scopeBufferOverhead + itemSize*3

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	// At the cap: the next append must fail.
	if _, err := buf.appendItem(newItem("s", "", nil)); err == nil {
		t.Fatal("expected StoreFullError before wipe")
	}

	s.wipe()

	buf2, err := s.getOrCreateScope("s")
	if err != nil {
		t.Fatalf("getOrCreateScope after wipe: %v", err)
	}
	if _, err := buf2.appendItem(newItem("s", "", nil)); err != nil {
		t.Fatalf("append after wipe: %v", err)
	}
}

// A ScopeBuffer pointer held before wipe must detach cleanly: further
// writes on it must return *ScopeDetachedError rather than silently
// succeeding into an orphan buffer no reader can reach. The store's byte
// counter must also remain at zero.
func TestStore_Wipe_DetachesOrphanedBuffers(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	buf, _ := s.getOrCreateScope("s")
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	s.wipe()

	// The caller still has `buf`; a write must now fail with ScopeDetachedError.
	_, err := buf.appendItem(newItem("s", "b", nil))
	var sde *ScopeDetachedError
	if !errors.As(err, &sde) {
		t.Fatalf("orphan append: got %v, want *ScopeDetachedError", err)
	}
	if got := s.totalBytes.Load(); got != 0 {
		t.Fatalf("orphan mutation leaked into totalBytes: got %d want 0", got)
	}
}

// /rebuild swaps the entire store map. Any stale ScopeBuffer pointer held
// by an in-flight /append must be detached — otherwise reserveBytes on the
// post-rebuild counter inflates totalBytes permanently, while the item
// lands in an orphan buffer that no reader can reach. Mirrors the wipe and
// delete_scope guarantees.
func TestStore_RebuildAll_DetachesOrphanedBuffers(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	buf, _ := s.getOrCreateScope("old")
	if _, err := buf.appendItem(newItem("old", "a", nil)); err != nil {
		t.Fatalf("pre-rebuild append: %v", err)
	}

	grouped := map[string][]Item{"new": {newItem("new", "x", nil)}}
	if _, _, err := s.rebuildAll(grouped); err != nil {
		t.Fatalf("rebuildAll: %v", err)
	}

	// The pre-rebuild buf pointer is stale; a write must now fail.
	_, err := buf.appendItem(newItem("old", "b", nil))
	var sde *ScopeDetachedError
	if !errors.As(err, &sde) {
		t.Fatalf("orphan append: got %v, want *ScopeDetachedError", err)
	}
	// Counter must still match only the rebuilt scope's item + its
	// per-scope buffer overhead — the orphan write must not have
	// inflated it.
	newBuf, _ := s.getScope("new")
	if got, want := s.totalBytes.Load(), newBuf.bytes+scopeBufferOverhead; got != want {
		t.Fatalf("totalBytes=%d want %d (orphan leaked into counter)", got, want)
	}
}

// Orphan deletes (deleteByID, deleteBySeq, deleteUpToSeq) must surface
// *ScopeDetachedError rather than silently mutate a buffer no reader
// can reach. Pre-fix the delete methods skipped the detached check, so
// a /delete handler that grabbed buf before /delete_scope (or /wipe,
// or /rebuild) detached it would mutate the orphan and return
// hit:true,deleted_count:1 to the client — meanwhile the live store
// either has no such scope or has a freshly-created one with the item
// still present. The fix returns *ScopeDetachedError; the handlers
// surface it as 409 Conflict, matching every other write path.
func TestScopeBuffer_DeletesDetectDetached(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	buf, _ := s.getOrCreateScope("s")
	it1, _ := buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))
	_, _ = buf.appendItem(newItem("s", "c", nil))

	// Detach by deleting the scope. buf is now an orphan.
	if _, ok := s.deleteScope("s"); !ok {
		t.Fatal("deleteScope reported miss on a scope that exists")
	}

	for _, tc := range []struct {
		name string
		fn   func() (int, error)
	}{
		{"deleteByID", func() (int, error) { return buf.deleteByID("a") }},
		{"deleteBySeq", func() (int, error) { return buf.deleteBySeq(it1.Seq) }},
		{"deleteUpToSeq", func() (int, error) { return buf.deleteUpToSeq(99) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := tc.fn()
			var sde *ScopeDetachedError
			if !errors.As(err, &sde) {
				t.Fatalf("got err=%v, want *ScopeDetachedError", err)
			}
			if n != 0 {
				t.Errorf("returned n=%d on detached buffer; want 0", n)
			}
		})
	}

	// Counter must remain at zero — orphan deletes must not leak into
	// totalBytes either way (the b.store guard exists, but with the
	// detached check we never reach it).
	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d want 0 (orphan deletes leaked into counter)", got)
	}
}

func TestStore_ReplaceScopes_LeavesOtherScopesUntouched(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	keep, _ := s.getOrCreateScope("keep")
	_, _ = keep.appendItem(newItem("keep", "k1", nil))
	keepLen := len(keep.items)

	grouped := map[string][]Item{
		"replace": {newItem("replace", "r1", nil)},
	}
	n, err := s.replaceScopes(grouped)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replaced=%d want 1", n)
	}
	if len(keep.items) != keepLen {
		t.Fatal("untouched scope was mutated")
	}
}

// A grouped map with an empty scope key must be rejected with a shape error
// (not an offender list), since the empty scope could not have passed the
// handler's per-item validation.
func TestStore_ReplaceScopes_RejectsEmptyScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	grouped := map[string][]Item{
		"": {newItem("", "a", nil)},
	}
	if _, err := s.replaceScopes(grouped); err == nil {
		t.Fatal("expected empty-scope rejection")
	}
}

func TestStore_RebuildAll_WipesEverything(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	old, _ := s.getOrCreateScope("old")
	_, _ = old.appendItem(newItem("old", "", nil))

	grouped := map[string][]Item{
		"new": {newItem("new", "n1", nil)},
	}
	nScopes, nItems, err := s.rebuildAll(grouped)
	if err != nil {
		t.Fatal(err)
	}
	if nScopes != 1 || nItems != 1 {
		t.Fatalf("rebuildAll: scopes=%d items=%d", nScopes, nItems)
	}
	if _, ok := s.getScope("old"); ok {
		t.Fatal("old scope should be wiped")
	}
	if _, ok := s.getScope("new"); !ok {
		t.Fatal("new scope should exist")
	}
}

// /warm must reject the whole batch (not partial-apply) when any scope in
// the request exceeds the cap. The error carries every offending scope so a
// client can fix all at once rather than discovering them one-by-one.
func TestStore_ReplaceScopes_RejectsOverCapWithAllOffenders(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 3, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	keep, _ := s.getOrCreateScope("untouched")
	_, _ = keep.appendItem(newItem("untouched", "k", nil))
	preLen := len(keep.items)

	grouped := map[string][]Item{
		"fits": {newItem("fits", "a", nil), newItem("fits", "b", nil)},
		"too_big_1": {
			newItem("too_big_1", "a", nil),
			newItem("too_big_1", "b", nil),
			newItem("too_big_1", "c", nil),
			newItem("too_big_1", "d", nil),
		},
		"too_big_2": {
			newItem("too_big_2", "a", nil),
			newItem("too_big_2", "b", nil),
			newItem("too_big_2", "c", nil),
			newItem("too_big_2", "d", nil),
			newItem("too_big_2", "e", nil),
		},
	}

	_, err := s.replaceScopes(grouped)
	if err == nil {
		t.Fatal("expected ScopeCapacityError when batch exceeds cap")
	}
	var sce *ScopeCapacityError
	if !errors.As(err, &sce) {
		t.Fatalf("expected *ScopeCapacityError, got %T: %v", err, err)
	}
	if len(sce.Offenders) != 2 {
		t.Fatalf("expected 2 offenders, got %d: %+v", len(sce.Offenders), sce.Offenders)
	}
	// Validate each offender carries scope/count/cap; map ordering means the
	// offender list order is not deterministic.
	seen := map[string]ScopeCapacityOffender{}
	for _, o := range sce.Offenders {
		seen[o.Scope] = o
	}
	if o, ok := seen["too_big_1"]; !ok || o.Count != 4 || o.Cap != 3 {
		t.Fatalf("offender for too_big_1 missing or wrong: %+v", o)
	}
	if o, ok := seen["too_big_2"]; !ok || o.Count != 5 || o.Cap != 3 {
		t.Fatalf("offender for too_big_2 missing or wrong: %+v", o)
	}

	// Atomic: the well-sized "fits" scope must NOT have been applied, and
	// the pre-existing "untouched" scope must still have its original item.
	if _, ok := s.getScope("fits"); ok {
		t.Fatal("fits scope was applied despite batch error")
	}
	if len(keep.items) != preLen {
		t.Fatal("existing scope was mutated by a failed batch")
	}
}

func TestStore_RebuildAll_RejectsOverCapWithAllOffenders(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 2, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	priorScope, _ := s.getOrCreateScope("prior")
	_, _ = priorScope.appendItem(newItem("prior", "p", nil))

	grouped := map[string][]Item{
		"too_big_1": {
			newItem("too_big_1", "a", nil),
			newItem("too_big_1", "b", nil),
			newItem("too_big_1", "c", nil),
		},
		"too_big_2": {
			newItem("too_big_2", "a", nil),
			newItem("too_big_2", "b", nil),
			newItem("too_big_2", "c", nil),
		},
	}

	_, _, err := s.rebuildAll(grouped)
	if err == nil {
		t.Fatal("expected ScopeCapacityError")
	}
	var sce *ScopeCapacityError
	if !errors.As(err, &sce) {
		t.Fatalf("expected *ScopeCapacityError, got %T: %v", err, err)
	}
	if len(sce.Offenders) != 2 {
		t.Fatalf("expected 2 offenders, got %d", len(sce.Offenders))
	}
	// rebuildAll replaces the entire store on success. On failure the old
	// store must remain intact — verify the prior scope is still there.
	if _, ok := s.getScope("prior"); !ok {
		t.Fatal("prior scope was wiped by a failed rebuild")
	}
}

func TestStore_RebuildAll_RejectsDuplicateIDs(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	s2, _ := s.getOrCreateScope("keep")
	_, _ = s2.appendItem(newItem("keep", "k1", nil))

	grouped := map[string][]Item{
		"bad": {
			newItem("bad", "x", nil),
			newItem("bad", "x", nil),
		},
	}
	if _, _, err := s.rebuildAll(grouped); err == nil {
		t.Fatal("expected duplicate id error")
	}
	// Validation happens before wiping, so existing scopes must be intact.
	if _, ok := s.getScope("keep"); !ok {
		t.Fatal("rebuildAll must not wipe on validation failure")
	}
}

// --- recordRead (7-day heat tracking) -----------------------------------------

// microsOnDay returns a microsecond Unix timestamp that falls on the given day index.
func microsOnDay(day int64) int64 {
	return day * 86400000000
}

func TestRecordRead_KeepsReadsWithinWindow(t *testing.T) {
	buf := NewScopeBuffer(10)

	// Read on day 1000 and day 1001 (both within the 7-day window).
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1001))

	if buf.last7DReadCount != 2 {
		t.Fatalf("last7DReadCount=%d want 2 (buggy code would reset on day change)", buf.last7DReadCount)
	}
}

func TestRecordRead_ExpiresBucketsOutsideWindow(t *testing.T) {
	buf := NewScopeBuffer(10)

	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1001))
	buf.recordRead(microsOnDay(1002))

	if buf.last7DReadCount != 3 {
		t.Fatalf("pre-window last7DReadCount=%d want 3", buf.last7DReadCount)
	}

	// Jump to day 1010 — all prior reads are > 6 days old.
	buf.recordRead(microsOnDay(1010))

	if buf.last7DReadCount != 1 {
		t.Fatalf("after expiry last7DReadCount=%d want 1", buf.last7DReadCount)
	}
}

func TestRecordRead_ReusesBucketSlotAcross7DayCycle(t *testing.T) {
	buf := NewScopeBuffer(10)

	// Day 1000 lands in slot 1000%7 = 6.
	buf.recordRead(microsOnDay(1000))
	// Day 1007 also lands in slot 6 — same physical slot, 7 days later.
	buf.recordRead(microsOnDay(1007))

	// Day 1000's read is now outside the rolling window (>= 7 days old).
	if buf.last7DReadCount != 1 {
		t.Fatalf("last7DReadCount=%d want 1 (old slot should have been expired)", buf.last7DReadCount)
	}
}

// stats() must report a fresh Last7DReadCount even when no recent
// recordRead has expired stale buckets. Without computeLast7DReadCountLocked
// a scope read 8 days ago and never since would still report its old
// "warm" count via /stats and /delete_scope_candidates, biasing
// eviction decisions against scopes that are actually cold.
func TestStats_Last7DReadCount_ExpiresWithoutNewReads(t *testing.T) {
	buf := NewScopeBuffer(10)

	// Three reads on day 1000 — cached field reads 3.
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1000))

	if buf.last7DReadCount != 3 {
		t.Fatalf("pre-stats last7DReadCount=%d want 3", buf.last7DReadCount)
	}

	// Eight days later — no fresh recordRead, so the cached field
	// still reads 3. stats() must compute live and report 0.
	st := buf.stats(microsOnDay(1008))
	if st.Last7DReadCount != 0 {
		t.Errorf("stats(day 1008).Last7DReadCount=%d want 0; cached field=%d (stale)",
			st.Last7DReadCount, buf.last7DReadCount)
	}

	// Boundary check: at day 1006 (6 days after the reads, still in
	// the rolling 7-day window), stats() must still report 3.
	st = buf.stats(microsOnDay(1006))
	if st.Last7DReadCount != 3 {
		t.Errorf("stats(day 1006).Last7DReadCount=%d want 3 (still in window)",
			st.Last7DReadCount)
	}
}

func TestRecordRead_RollingWindowSum(t *testing.T) {
	buf := NewScopeBuffer(10)

	// 2 reads on day 1000, 1 on day 1003, 3 on day 1006.
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1003))
	buf.recordRead(microsOnDay(1006))
	buf.recordRead(microsOnDay(1006))
	buf.recordRead(microsOnDay(1006))

	if buf.last7DReadCount != 6 {
		t.Fatalf("last7DReadCount=%d want 6", buf.last7DReadCount)
	}

	// Read on day 1007 — day 1000 falls out of window (1007-6=1001, 1000 < 1001).
	buf.recordRead(microsOnDay(1007))

	// Expected: 0 from day 1000, 1 from day 1003, 3 from day 1006, 1 from day 1007 = 5.
	if buf.last7DReadCount != 5 {
		t.Fatalf("last7DReadCount=%d want 5 (day 1000's 2 reads should expire)", buf.last7DReadCount)
	}
}

// --- approxSizeBytes ----------------------------------------------------------

func TestApproxSizeBytes_IgnoresReservedCapacity(t *testing.T) {
	buf := NewScopeBuffer(10000)
	size := buf.approxSizeBytes()

	// Buggy code counted cap(items)*32 = 320KB for an empty scope.
	if size > 2048 {
		t.Fatalf("empty scope approx_scope_bytes=%d want < 2KB (should not count reserved capacity)", size)
	}
}

func TestApproxSizeBytes_GrowsWithItems(t *testing.T) {
	buf := NewScopeBuffer(10000)
	before := buf.approxSizeBytes()

	_, _ = buf.appendItem(newItem("s", "a", map[string]interface{}{"text": "hello world"}))

	after := buf.approxSizeBytes()
	if after <= before {
		t.Fatalf("size did not grow after append: before=%d after=%d", before, after)
	}
}

// TestDeleteByID_ClearsBackingSlot verifies the GC invariant for deleteByID:
// after the slice shift-and-shrink, the tail slot must be zeroed so the Item's
// payload map is eligible for GC. The backing array still exists at full
// capacity, so we reslice past the current length to peek at the vacated slot.
func TestDeleteByID_ClearsBackingSlot(t *testing.T) {
	buf := NewScopeBuffer(8)

	_, _ = buf.appendItem(newItem("s", "a", map[string]interface{}{"marker": "A"}))
	_, _ = buf.appendItem(newItem("s", "b", map[string]interface{}{"marker": "B"}))
	_, _ = buf.appendItem(newItem("s", "c", map[string]interface{}{"marker": "C"}))

	if n, _ := buf.deleteByID("b"); n != 1 {
		t.Fatalf("delete: n=%d want 1", n)
	}
	if len(buf.items) != 2 {
		t.Fatalf("len=%d want 2 after delete", len(buf.items))
	}

	full := buf.items[:3]
	tail := full[2]
	if tail.ID != "" || tail.Seq != 0 || tail.Payload != nil {
		t.Fatalf("tail slot not cleared in backing array: %+v", tail)
	}
}

// --- buildReplacementState ----------------------------------------------------

func TestBuildReplacementState_SeqFromOne(t *testing.T) {
	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "b", nil),
		newItem("s", "c", nil),
	}
	r, err := buildReplacementState(items)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.items) != 3 {
		t.Fatalf("len=%d want 3 (no trim)", len(r.items))
	}
	if r.items[0].ID != "a" || r.items[2].ID != "c" {
		t.Fatalf("input order not preserved: %+v", r.items)
	}
	if r.items[0].Seq != 1 || r.items[2].Seq != 3 {
		t.Fatalf("seq not fresh: %+v", r.items)
	}
	if r.lastSeq != 3 {
		t.Fatalf("lastSeq=%d want 3", r.lastSeq)
	}
	if _, ok := r.byID["a"]; !ok {
		t.Fatal("byID missing 'a'")
	}
	if _, ok := r.bySeq[1]; !ok {
		t.Fatal("bySeq missing seq 1")
	}
}

// --- store-level byte budget --------------------------------------------------

// Byte-cap is the aggregate approxItemSize across all scopes; writes that
// would push the running total past maxStoreBytes are rejected with
// StoreFullError. State must stay untouched on rejection — same contract as
// the per-scope ScopeFullError.
func TestStore_Append_RejectsAtByteCap(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	capBytes := scopeBufferOverhead + itemSize*3

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")

	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d within cap: %v", i, err)
		}
	}

	_, err := buf.appendItem(newItem("s", "", nil))
	if err == nil {
		t.Fatal("expected StoreFullError when append would exceed byte cap")
	}
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected *StoreFullError, got %T: %v", err, err)
	}
	if stfe.Cap != capBytes {
		t.Fatalf("Cap=%d want %d", stfe.Cap, capBytes)
	}
	if stfe.AddedBytes != itemSize {
		t.Fatalf("AddedBytes=%d want %d", stfe.AddedBytes, itemSize)
	}

	if len(buf.items) != 3 {
		t.Fatalf("rejected write mutated buffer: len=%d want 3", len(buf.items))
	}
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)+itemSize*3; got != want {
		t.Fatalf("totalBytes=%d want %d after rejected append (overhead + 3 items)", got, want)
	}
}

// Freeing capacity via /delete must let subsequent appends succeed: the
// byte counter has to drop by the removed item's size or the store drifts
// into a permanently "full" state.
func TestStore_Delete_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "a", nil))
	capBytes := scopeBufferOverhead + itemSize*2

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append a: %v", err)
	}
	if _, err := buf.appendItem(newItem("s", "b", nil)); err != nil {
		t.Fatalf("append b: %v", err)
	}

	// At cap now — a third append must fail.
	if _, err := buf.appendItem(newItem("s", "c", nil)); err == nil {
		t.Fatal("expected StoreFullError at cap")
	}

	if n, _ := buf.deleteByID("a"); n != 1 {
		t.Fatalf("deleteByID a: n=%d want 1", n)
	}

	// After freeing one item's worth, a new append must succeed.
	if _, err := buf.appendItem(newItem("s", "c", nil)); err != nil {
		t.Fatalf("append c after delete: %v", err)
	}
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)+itemSize*2; got != want {
		t.Fatalf("totalBytes=%d want %d after delete+append", got, want)
	}
}

func TestStore_DeleteUpTo_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	capBytes := scopeBufferOverhead + itemSize*3

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if n, _ := buf.deleteUpToSeq(2); n != 2 {
		t.Fatalf("deleteUpToSeq: n=%d want 2", n)
	}

	// Two items freed, so room for two more.
	for i := 0; i < 2; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append after drain %d: %v", i, err)
		}
	}
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)+itemSize*3; got != want {
		t.Fatalf("totalBytes=%d want %d", got, want)
	}
}

func TestStore_DeleteScope_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	capBytes := scopeBufferOverhead + itemSize*4

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	for i := 0; i < 4; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	n, ok := s.deleteScope("s")
	if !ok || n != 4 {
		t.Fatalf("deleteScope: ok=%v n=%d", ok, n)
	}
	if got := s.totalBytes.Load(); got != 0 {
		t.Fatalf("totalBytes=%d want 0 after deleteScope", got)
	}
}

// /update that grows the payload must reserve the delta — a grow past the
// byte cap returns StoreFullError without mutating the stored item.
func TestStore_Update_RejectsGrowAtByteCap(t *testing.T) {
	small := newItem("s", "a", map[string]interface{}{"v": 1})
	capBytes := scopeBufferOverhead + approxItemSize(small) + 8 // room for the small item, not a large replacement

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	if _, err := buf.appendItem(small); err != nil {
		t.Fatalf("append small: %v", err)
	}

	// A payload with 100 extra bytes overflows the tiny headroom.
	bigPayload, _ := json.Marshal(map[string]interface{}{
		"v":    1,
		"blob": "x_________________________________________________________________________________________________",
	})
	n, err := buf.updateByID("a", bigPayload, nil)
	if err == nil {
		t.Fatal("expected StoreFullError on grow past cap")
	}
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected *StoreFullError, got %T: %v", err, err)
	}
	if n != 0 {
		t.Fatalf("updated=%d want 0 on reject", n)
	}
	// Payload must still be the small original.
	got, _ := buf.getByID("a")
	if string(got.Payload) != string(small.Payload) {
		t.Fatalf("payload changed despite reject: %s", string(got.Payload))
	}
}

// /warm's byte-cap check runs across all scopes in the batch. A request
// whose net byte delta would push the store over the cap is rejected as a
// whole with StoreFullError, and no scope is applied.
func TestStore_ReplaceScopes_RejectsAtByteCap(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	// One scope-buffer overhead (for the pre-seed) + 3 items worth.
	// The /warm batch needs 2 new scopes (overhead × 2) + 4 items, so
	// expected post-batch usage = 3 overheads + 5 items, well past
	// the cap of 1 overhead + 3 items.
	capBytes := scopeBufferOverhead + itemSize*3

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	// Pre-seed an unrelated scope so we can assert it survives the reject.
	pre, _ := s.getOrCreateScope("untouched")
	if _, err := pre.appendItem(newItem("untouched", "u", nil)); err != nil {
		t.Fatalf("pre-seed: %v", err)
	}
	preLen := len(pre.items)

	// Batch adds 4 items + 2 new scopes — store already holds 1 item +
	// 1 scope overhead, so delta pushes total over the cap.
	grouped := map[string][]Item{
		"a": {newItem("a", "", nil), newItem("a", "", nil)},
		"b": {newItem("b", "", nil), newItem("b", "", nil)},
	}
	_, err := s.replaceScopes(grouped)
	if err == nil {
		t.Fatal("expected StoreFullError for over-cap batch")
	}
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected *StoreFullError, got %T: %v", err, err)
	}

	// No scope in the batch may have been applied, and the pre-seeded scope
	// must be untouched.
	if _, ok := s.getScope("a"); ok {
		t.Fatal("scope 'a' applied despite reject")
	}
	if _, ok := s.getScope("b"); ok {
		t.Fatal("scope 'b' applied despite reject")
	}
	if len(pre.items) != preLen {
		t.Fatalf("untouched scope mutated: len=%d want %d", len(pre.items), preLen)
	}
	preSize := approxItemSize(newItem("untouched", "u", nil))
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)+preSize; got != want {
		t.Fatalf("totalBytes=%d want %d (only pre-seed scope+item should count)", got, want)
	}
}

// /warm must never push totalBytes past the cap, even when concurrent
// /append traffic targets scopes outside the batch. The pre-fix design
// snapshotted totalBytes before commit, so an appender that slipped in
// between snapshot and commit could collectively overshoot the cap:
// the batch's pre-check would see room, the appender's own check would
// also see room, but once both committed the sum exceeded the cap.
// reserveBytes-based admission closes that window: the batch reserves
// its delta atomically via CAS, after which any concurrent appender
// sees the post-reserve total and is rejected if the cap is exceeded.
//
// Setup: cap = 5 item-sizes, store pre-seeded with 2 items, /warm grows
// the batch scope by +2 items, 16 concurrent appenders each try +1 on
// a different scope. Safe outcome: /warm fits (+2 → 4) and at most ONE
// appender fits (4 → 5). Anything past that violates the cap.
func TestStore_ReplaceScopes_StrictCapAgainstConcurrentAppends(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "x", nil))
	const appendersPerIter = 16
	// Per-scope overhead × (1 batch + 16 appenders) + 5 items worth.
	// Originally just `itemSize*5`; bumped to make room for the
	// per-scope overhead introduced in v0.5.14.
	capBytes := scopeBufferOverhead*(appendersPerIter+1) + itemSize*5

	const iterations = 500

	for iter := 0; iter < iterations; iter++ {
		s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

		// Pre-seed "batch" with 2 items; /warm will replace it with 4
		// items, net delta +2 item-sizes.
		batch, _ := s.getOrCreateScope("batch")
		_, _ = batch.appendItem(newItem("batch", "a", nil))
		_, _ = batch.appendItem(newItem("batch", "b", nil))

		grouped := map[string][]Item{
			"batch": {
				newItem("batch", "c", nil),
				newItem("batch", "d", nil),
				newItem("batch", "e", nil),
				newItem("batch", "f", nil),
			},
		}

		var wg sync.WaitGroup
		wg.Add(appendersPerIter + 1)
		for i := 0; i < appendersPerIter; i++ {
			i := i
			go func() {
				defer wg.Done()
				scope := "other" + string(rune('A'+i))
				// getOrCreateScope can fail with StoreFullError when
				// the per-scope overhead reservation hits the cap;
				// the test deliberately provisions just enough room
				// for all appenders, but a tight race may still see
				// a transient over-cap. Skip on err — the test
				// asserts the cap-strict invariant on whatever
				// landed.
				buf, err := s.getOrCreateScope(scope)
				if err != nil {
					return
				}
				_, _ = buf.appendItem(newItem(scope, "w", nil))
			}()
		}
		go func() {
			defer wg.Done()
			_, _ = s.replaceScopes(grouped)
		}()
		wg.Wait()

		got := s.totalBytes.Load()
		if got > capBytes {
			t.Fatalf("iter %d: totalBytes=%d exceeds cap=%d (race let writers overshoot)",
				iter, got, capBytes)
		}

		// Accounting invariant: totalBytes must equal sum of per-scope
		// item bytes PLUS scope-buffer overhead × scope count.
		var sum int64
		scopes := s.listScopes()
		for _, buf := range scopes {
			buf.mu.RLock()
			sum += buf.bytes
			buf.mu.RUnlock()
		}
		expected := sum + int64(len(scopes))*scopeBufferOverhead
		if got != expected {
			t.Fatalf("iter %d: totalBytes=%d != Σ buf.bytes + overhead = %d", iter, got, expected)
		}
	}
}

// /rebuild is all-or-nothing: if the new total bytes exceed the cap the
// rebuild aborts and the prior store is left intact.
func TestStore_RebuildAll_RejectsAtByteCap(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	// Cap fits 1 scope-overhead + 2 items. The /rebuild target tries
	// 1 scope-overhead + 3 items — should fail by 1 itemSize.
	capBytes := scopeBufferOverhead + itemSize*2

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	pre, _ := s.getOrCreateScope("prior")
	if _, err := pre.appendItem(newItem("prior", "p", nil)); err != nil {
		t.Fatalf("pre-seed: %v", err)
	}

	grouped := map[string][]Item{
		"big": {newItem("big", "", nil), newItem("big", "", nil), newItem("big", "", nil)},
	}
	_, _, err := s.rebuildAll(grouped)
	if err == nil {
		t.Fatal("expected StoreFullError for over-cap rebuild")
	}
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected *StoreFullError, got %T: %v", err, err)
	}

	// Prior state must still be intact — no partial swap.
	if _, ok := s.getScope("prior"); !ok {
		t.Fatal("prior scope wiped despite rebuild reject")
	}
	if _, ok := s.getScope("big"); ok {
		t.Fatal("rebuild was partially applied")
	}
}

// A successful /rebuild replaces the store entirely: totalBytes must match
// the sum of the newly installed items, not be additive to the prior state.
func TestStore_RebuildAll_ResetsByteCounter(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	pre, _ := s.getOrCreateScope("old")
	for i := 0; i < 5; i++ {
		if _, err := pre.appendItem(newItem("old", "", nil)); err != nil {
			t.Fatalf("pre-seed %d: %v", i, err)
		}
	}

	grouped := map[string][]Item{
		"new": {newItem("new", "n1", nil), newItem("new", "n2", nil)},
	}
	if _, _, err := s.rebuildAll(grouped); err != nil {
		t.Fatal(err)
	}

	// 1 new scope (overhead) + 2 items.
	expected := int64(scopeBufferOverhead) + approxItemSize(newItem("new", "n1", nil)) + approxItemSize(newItem("new", "n2", nil))
	if got := s.totalBytes.Load(); got != expected {
		t.Fatalf("totalBytes=%d want %d (counter must be reset to new total)", got, expected)
	}
}

// reserveBytes is the atomic admission primitive. Positive deltas honor the
// cap; negative deltas always succeed. A CAS loop isn't directly observable,
// so this test just validates the return-value contract.
func TestStore_ReserveBytes_RejectsPositiveOverCap(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100, MaxItemBytes: 1 << 20})
	if ok, _, _ := s.reserveBytes(80); !ok {
		t.Fatal("reserve 80/100 should succeed")
	}
	ok, current, cap := s.reserveBytes(30)
	if ok {
		t.Fatal("reserve 30 on top of 80 should fail (cap 100)")
	}
	if current != 80 {
		t.Fatalf("current=%d want 80 (unchanged on failed reserve)", current)
	}
	if cap != 100 {
		t.Fatalf("cap=%d want 100", cap)
	}
	if ok, _, _ := s.reserveBytes(-50); !ok {
		t.Fatal("negative reserve (release) must always succeed")
	}
	if got := s.totalBytes.Load(); got != 30 {
		t.Fatalf("totalBytes=%d want 30 after 80 + (-50)", got)
	}
}

// deleteScope must not race with an /append that obtained the buf pointer
// before the scope was removed from the map. Under the old RLock-snapshot
// pattern, the appended item's bytes leaked into s.totalBytes after the
// subtract happened on a stale value. This test drives many rounds of
// parallel append/delete on the same scope and asserts the final counter
// matches the items that survived in s.scopes.
func TestStore_DeleteScope_RaceWithAppend(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 1000, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	const rounds = 200
	for i := 0; i < rounds; i++ {
		scope := "race"
		buf, _ := s.getOrCreateScope(scope)
		// Prime with one item so deleteScope has real bytes to subtract.
		if _, err := buf.appendItem(newItem(scope, "", nil)); err != nil {
			t.Fatalf("prime append: %v", err)
		}

		done := make(chan struct{}, 2)
		go func() {
			buf, _ := s.getOrCreateScope(scope)
			_, _ = buf.appendItem(newItem(scope, "", nil))
			done <- struct{}{}
		}()
		go func() {
			_, _ = s.deleteScope(scope)
			done <- struct{}{}
		}()
		<-done
		<-done
	}

	// Final invariant: s.totalBytes == sum(buf.bytes) + scope-overhead
	// per live scope. Any ghost bytes from the race would inflate
	// totalBytes above the sum.
	var liveBytes int64
	live := s.listScopes()
	for _, buf := range live {
		buf.mu.RLock()
		liveBytes += buf.bytes
		buf.mu.RUnlock()
	}
	expected := liveBytes + int64(len(live))*scopeBufferOverhead
	if got := s.totalBytes.Load(); got != expected {
		t.Fatalf("totalBytes=%d but live scopes hold %d bytes + %d overhead = %d (ghost bytes from race)",
			got, liveBytes, int64(len(live))*scopeBufferOverhead, expected)
	}
}
