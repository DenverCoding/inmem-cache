package scopecache

import (
	"encoding/json"
	"errors"
	"fmt"
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

// --- scopeBuffer.appendItem ---------------------------------------------------

func TestAppendItem_AssignsSeqMonotonically(t *testing.T) {
	buf := newscopeBuffer(10)

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
	buf := newscopeBuffer(10)

	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if _, err := buf.appendItem(newItem("s", "a", nil)); err == nil {
		t.Fatal("expected duplicate id rejection")
	}
}

func TestAppendItem_AllowsMultipleEmptyIDs(t *testing.T) {
	buf := newscopeBuffer(10)

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
	buf := newscopeBuffer(3)

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

// --- scopeBuffer.replaceAll ---------------------------------------------------

func TestReplaceAll_AssignsFreshSeqFromOne(t *testing.T) {
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(3)
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
	buf := newscopeBuffer(10)

	items := []Item{
		newItem("s", "a", nil),
		newItem("s", "a", nil),
	}
	if _, err := buf.replaceAll(items); err == nil {
		t.Fatal("expected duplicate id error")
	}
}

func TestReplaceAll_EmptyItemsClearsScope(t *testing.T) {
	buf := newscopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "", nil))

	if _, err := buf.replaceAll([]Item{}); err != nil {
		t.Fatal(err)
	}
	if len(buf.items) != 0 {
		t.Fatalf("expected empty buffer, got %d items", len(buf.items))
	}
}

// --- scopeBuffer.updateByID ---------------------------------------------------

func TestUpdateByID_HitPreservesSeq(t *testing.T) {
	buf := newscopeBuffer(10)
	original, _ := buf.appendItem(newItem("s", "a", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 2})
	n, err := buf.updateByID("a", newPayload)
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
	buf := newscopeBuffer(10)
	raw, _ := json.Marshal(map[string]interface{}{"v": 1})
	n, err := buf.updateByID("missing", raw)
	if err != nil {
		t.Fatalf("updateByID: %v", err)
	}
	if n != 0 {
		t.Fatalf("updated=%d want 0", n)
	}
}

// --- scopeBuffer.updateBySeq --------------------------------------------------

func TestUpdateBySeq_Hit(t *testing.T) {
	buf := newscopeBuffer(10)
	it, _ := buf.appendItem(newItem("s", "", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 2})
	n, err := buf.updateBySeq(it.Seq, newPayload)
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
	buf := newscopeBuffer(10)
	it, _ := buf.appendItem(newItem("s", "a", map[string]interface{}{"v": 1}))

	newPayload, _ := json.Marshal(map[string]interface{}{"v": 42})
	if _, err := buf.updateBySeq(it.Seq, newPayload); err != nil {
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
	buf := newscopeBuffer(10)
	raw, _ := json.Marshal(map[string]interface{}{"v": 1})
	n, err := buf.updateBySeq(999, raw)
	if err != nil {
		t.Fatalf("updateBySeq: %v", err)
	}
	if n != 0 {
		t.Fatalf("updated=%d want 0", n)
	}
}

// --- scopeBuffer.upsertByID ---------------------------------------------------

func TestUpsertByID_CreatesNewItem(t *testing.T) {
	buf := newscopeBuffer(10)

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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(2)
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

// --- scopeBuffer.counterAdd ---------------------------------------------------

func TestCounterAdd_CreatesWithStartingValue(t *testing.T) {
	buf := newscopeBuffer(10)

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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)

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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
	_, _ = buf.appendItem(Item{Scope: "c", ID: "k", Payload: json.RawMessage(`{"v":1}`)})

	_, _, err := buf.counterAdd("c", "k", 1)
	var cpe *CounterPayloadError
	if !errors.As(err, &cpe) {
		t.Fatalf("expected *CounterPayloadError, got %T: %v", err, err)
	}
}

func TestCounterAdd_RejectsOutOfRangeExisting(t *testing.T) {
	buf := newscopeBuffer(10)
	// 2^53 — one above the allowed ±(2^53-1) range.
	_, _ = buf.appendItem(Item{Scope: "c", ID: "k", Payload: json.RawMessage(`9007199254740992`)})

	_, _, err := buf.counterAdd("c", "k", 1)
	var cpe *CounterPayloadError
	if !errors.As(err, &cpe) {
		t.Fatalf("expected *CounterPayloadError, got %T: %v", err, err)
	}
}

func TestCounterAdd_RejectsOverflow(t *testing.T) {
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
	_, _, _ = buf.counterAdd("c", "k", -MaxCounterValue)

	_, _, err := buf.counterAdd("c", "k", -1)
	var coe *CounterOverflowError
	if !errors.As(err, &coe) {
		t.Fatalf("expected *CounterOverflowError, got %T: %v", err, err)
	}
}

func TestCounterAdd_RejectsAtScopeCapacity(t *testing.T) {
	buf := newscopeBuffer(1)
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

// --- scopeBuffer.deleteByID ---------------------------------------------------

func TestDeleteByID_Hit(t *testing.T) {
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
	n, _ := buf.deleteByID("missing")
	if n != 0 {
		t.Fatalf("deleted=%d want 0", n)
	}
}

func TestDeleteByID_DoesNotRollbackLastSeq(t *testing.T) {
	buf := newscopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))

	_, _ = buf.deleteByID("b")
	next, _ := buf.appendItem(newItem("s", "c", nil))
	if next.Seq != 3 {
		t.Fatalf("seq=%d want 3 (no rollback)", next.Seq)
	}
}

// --- scopeBuffer.deleteBySeq --------------------------------------------------

func TestDeleteBySeq_Hit(t *testing.T) {
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))

	if n, _ := buf.deleteBySeq(999); n != 0 {
		t.Fatalf("deleted=%d want 0", n)
	}
	if len(buf.items) != 1 {
		t.Fatalf("len=%d want 1", len(buf.items))
	}
}

func TestDeleteBySeq_NoIDItem(t *testing.T) {
	buf := newscopeBuffer(10)
	it, _ := buf.appendItem(newItem("s", "", nil))

	if n, _ := buf.deleteBySeq(it.Seq); n != 1 {
		t.Fatalf("deleted=%d want 1", n)
	}
	if len(buf.items) != 0 {
		t.Fatalf("len=%d want 0", len(buf.items))
	}
}

func TestDeleteBySeq_DoesNotRollbackLastSeq(t *testing.T) {
	buf := newscopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "a", nil))
	it2, _ := buf.appendItem(newItem("s", "b", nil))

	_, _ = buf.deleteBySeq(it2.Seq)
	next, _ := buf.appendItem(newItem("s", "c", nil))
	if next.Seq != 3 {
		t.Fatalf("seq=%d want 3 (no rollback)", next.Seq)
	}
}

// --- scopeBuffer.deleteUpToSeq ---------------------------------------------

func TestDeleteUpToSeq_RemovesPrefix(t *testing.T) {
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(5)
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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(fill * 2)
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

// --- scopeBuffer.tailOffset ---------------------------------------------------

func TestTailOffset_BasicAndEdges(t *testing.T) {
	buf := newscopeBuffer(10)
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

// --- scopeBuffer.sinceSeq -----------------------------------------------------

func TestSinceSeq_ReturnsItemsAfterCursor(t *testing.T) {
	buf := newscopeBuffer(10)
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
	buf := newscopeBuffer(10)
	for i := 1; i <= 5; i++ {
		_, _ = buf.appendItem(newItem("s", "", nil))
	}

	got, _ := buf.sinceSeq(0, 2)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
}

func TestSinceSeq_EmptyWhenPastEnd(t *testing.T) {
	buf := newscopeBuffer(10)
	_, _ = buf.appendItem(newItem("s", "", nil))

	got, _ := buf.sinceSeq(100, 0)
	if len(got) != 0 {
		t.Fatalf("len=%d want 0", len(got))
	}
}

// --- scopeBuffer.getByID / getBySeq -------------------------------------------

func TestGetByIDAndSeq(t *testing.T) {
	buf := newscopeBuffer(10)
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

// --- recordRead (7-day heat tracking) -----------------------------------------

// microsOnDay returns a microsecond Unix timestamp that falls on the given day index.
func microsOnDay(day int64) int64 {
	return day * 86400000000
}

func TestRecordRead_KeepsReadsWithinWindow(t *testing.T) {
	buf := newscopeBuffer(10)

	// Read on day 1000 and day 1001 (both within the 7-day window).
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1001))

	if buf.last7DReadCount.Load() != 2 {
		t.Fatalf("last7DReadCount=%d want 2 (buggy code would reset on day change)", buf.last7DReadCount.Load())
	}
}

func TestRecordRead_ExpiresBucketsOutsideWindow(t *testing.T) {
	buf := newscopeBuffer(10)

	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1001))
	buf.recordRead(microsOnDay(1002))

	if buf.last7DReadCount.Load() != 3 {
		t.Fatalf("pre-window last7DReadCount=%d want 3", buf.last7DReadCount.Load())
	}

	// Jump to day 1010 — all prior reads are > 6 days old.
	buf.recordRead(microsOnDay(1010))

	if buf.last7DReadCount.Load() != 1 {
		t.Fatalf("after expiry last7DReadCount=%d want 1", buf.last7DReadCount.Load())
	}
}

func TestRecordRead_ReusesBucketSlotAcross7DayCycle(t *testing.T) {
	buf := newscopeBuffer(10)

	// Day 1000 lands in slot 1000%7 = 6.
	buf.recordRead(microsOnDay(1000))
	// Day 1007 also lands in slot 6 — same physical slot, 7 days later.
	buf.recordRead(microsOnDay(1007))

	// Day 1000's read is now outside the rolling window (>= 7 days old).
	if buf.last7DReadCount.Load() != 1 {
		t.Fatalf("last7DReadCount=%d want 1 (old slot should have been expired)", buf.last7DReadCount.Load())
	}
}

// stats() must report a fresh Last7DReadCount even when no recent
// recordRead has expired stale buckets. Without computeLast7DReadCountLocked
// a scope read 8 days ago and never since would still report its old
// "warm" count via /stats and /delete_scope_candidates, biasing
// eviction decisions against scopes that are actually cold.
func TestStats_Last7DReadCount_ExpiresWithoutNewReads(t *testing.T) {
	buf := newscopeBuffer(10)

	// Three reads on day 1000 — cached field reads 3.
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1000))

	if buf.last7DReadCount.Load() != 3 {
		t.Fatalf("pre-stats last7DReadCount=%d want 3", buf.last7DReadCount.Load())
	}

	// Eight days later — no fresh recordRead, so the cached field
	// still reads 3. stats() must compute live and report 0.
	st := buf.stats(microsOnDay(1008))
	if st.Last7DReadCount != 0 {
		t.Errorf("stats(day 1008).Last7DReadCount=%d want 0; cached field=%d (stale)",
			st.Last7DReadCount, buf.last7DReadCount.Load())
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
	buf := newscopeBuffer(10)

	// 2 reads on day 1000, 1 on day 1003, 3 on day 1006.
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1000))
	buf.recordRead(microsOnDay(1003))
	buf.recordRead(microsOnDay(1006))
	buf.recordRead(microsOnDay(1006))
	buf.recordRead(microsOnDay(1006))

	if buf.last7DReadCount.Load() != 6 {
		t.Fatalf("last7DReadCount=%d want 6", buf.last7DReadCount.Load())
	}

	// Read on day 1007 — day 1000 falls out of window (1007-6=1001, 1000 < 1001).
	buf.recordRead(microsOnDay(1007))

	// Expected: 0 from day 1000, 1 from day 1003, 3 from day 1006, 1 from day 1007 = 5.
	if buf.last7DReadCount.Load() != 5 {
		t.Fatalf("last7DReadCount=%d want 5 (day 1000's 2 reads should expire)", buf.last7DReadCount.Load())
	}
}

// --- approxSizeBytes ----------------------------------------------------------

func TestApproxSizeBytes_IgnoresReservedCapacity(t *testing.T) {
	buf := newscopeBuffer(10000)
	size := buf.approxSizeBytes()

	// Buggy code counted cap(items)*32 = 320KB for an empty scope.
	if size > 2048 {
		t.Fatalf("empty scope approx_scope_bytes=%d want < 2KB (should not count reserved capacity)", size)
	}
}

func TestApproxSizeBytes_GrowsWithItems(t *testing.T) {
	buf := newscopeBuffer(10000)
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
	buf := newscopeBuffer(8)

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

// walkApproxSize is the original O(items) formula approxSizeBytesLocked
// used to compute. Kept as a local-to-test reference so the cached
// (O(1)) version can be pinned for parity across mutation paths. If
// the formula in approxSizeBytesLocked ever changes, update this
// helper in lockstep — the two MUST stay byte-equal.
func walkApproxSize(b *scopeBuffer) int64 {
	var total int64
	total += 64
	total += int64(len(b.items)) * 32
	for _, item := range b.items {
		total += approxItemSize(item)
	}
	total += int64(len(b.byID)) * 32
	for k := range b.byID {
		total += int64(len(k))
	}
	total += int64(len(b.bySeq)) * 16
	total += int64(len(b.readHeatBuckets)) * 16
	return total
}

// approxSizeBytesLocked must equal the walk-based formula after every
// mutation path. Pinning this is what makes the O(1) rewrite safe:
// admission control is unchanged, but observability would silently
// drift if any path forgot to update b.bytes or b.idKeyBytes.
func TestApproxSizeBytes_MatchesWalkAcrossMutations(t *testing.T) {
	buf := newscopeBuffer(100)

	check := func(label string) {
		t.Helper()
		got := buf.approxSizeBytes()
		want := walkApproxSize(buf)
		if got != want {
			t.Errorf("%s: cached=%d walk=%d (drift; an incremental update path is missing)",
				label, got, want)
		}
	}

	check("empty scope")

	for i := 0; i < 5; i++ {
		if _, err := buf.appendItem(newItem("s", fmt.Sprintf("id_%d", i),
			map[string]interface{}{"v": i})); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	check("after 5 appends with non-empty ids")

	if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
		t.Fatalf("append no-id: %v", err)
	}
	check("after append with empty id (byID untouched)")

	if _, _, err := buf.upsertByID(newItem("s", "id_2",
		map[string]interface{}{"v": 999, "extra": "longer payload"})); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}
	check("after upsert replace (ID unchanged, payload grew)")

	if _, err := buf.updateByID("id_3", json.RawMessage(`{"v":3,"x":"larger"}`)); err != nil {
		t.Fatalf("update: %v", err)
	}
	check("after updateByID")

	if _, _, err := buf.counterAdd("s", "counter_a", 1); err != nil {
		t.Fatalf("counter create: %v", err)
	}
	check("after counterAdd create (new id)")

	if _, _, err := buf.counterAdd("s", "counter_a", 5); err != nil {
		t.Fatalf("counter inc: %v", err)
	}
	check("after counterAdd increment (key unchanged, value grew)")

	if _, err := buf.deleteByID("id_0"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	check("after deleteByID (id removed from byID)")

	if _, err := buf.deleteUpToSeq(3); err != nil {
		t.Fatalf("deleteUpToSeq: %v", err)
	}
	check("after deleteUpToSeq (bulk delete)")

	if _, err := buf.replaceAll([]Item{
		newItem("s", "fresh_a", nil),
		newItem("s", "fresh_b_longer_id", nil),
		newItem("s", "", nil),
	}); err != nil {
		t.Fatalf("replaceAll: %v", err)
	}
	check("after replaceAll (commitReplacement)")
}

// idKeyBytes drift test: a path that adds to byID without bumping the
// counter would inflate approx_scope_mb relative to the walk; a delete
// path that forgets the subtraction would deflate it. Track the
// counter directly across an add-then-delete cycle as a tighter check
// than the parity test above.
func TestIDKeyBytes_TracksByIDKeysExactly(t *testing.T) {
	buf := newscopeBuffer(100)

	if buf.idKeyBytes != 0 {
		t.Fatalf("fresh scope: idKeyBytes=%d want 0", buf.idKeyBytes)
	}

	if _, err := buf.appendItem(newItem("s", "abc", nil)); err != nil {
		t.Fatalf("append abc: %v", err)
	}
	if buf.idKeyBytes != 3 {
		t.Errorf("after append abc: idKeyBytes=%d want 3", buf.idKeyBytes)
	}

	if _, err := buf.appendItem(newItem("s", "twelve_chars", nil)); err != nil {
		t.Fatalf("append twelve_chars: %v", err)
	}
	if buf.idKeyBytes != 3+12 {
		t.Errorf("after append twelve_chars: idKeyBytes=%d want 15", buf.idKeyBytes)
	}

	if _, err := buf.deleteByID("abc"); err != nil {
		t.Fatalf("delete abc: %v", err)
	}
	if buf.idKeyBytes != 12 {
		t.Errorf("after delete abc: idKeyBytes=%d want 12", buf.idKeyBytes)
	}

	if _, err := buf.deleteByID("twelve_chars"); err != nil {
		t.Fatalf("delete twelve_chars: %v", err)
	}
	if buf.idKeyBytes != 0 {
		t.Errorf("after delete all: idKeyBytes=%d want 0", buf.idKeyBytes)
	}
}

// --- lastWriteTS --------------------------------------------------------------
//
// lastWriteTS advances on every path that mutates the scope (append,
// upsert, update, counter_add, delete, deleteUpToSeq, replaceAll) and
// stays put on reads (recordRead). The "preCall <= lastWriteTS" bracket
// is the resilient assertion shape: clock resolution on Windows can be
// ~16ms, so a strictly-greater check against createdTS would be flaky;
// tests instead assert that lastWriteTS sits at or beyond a stamp
// captured immediately before the write call.

func TestLastWriteTS_FreshScopeEqualsCreatedTS(t *testing.T) {
	buf := newscopeBuffer(10)
	if buf.lastWriteTS != buf.createdTS {
		t.Fatalf("fresh scope: lastWriteTS=%d createdTS=%d (must be equal — both initialised from one nowUnixMicro() call)",
			buf.lastWriteTS, buf.createdTS)
	}
	if buf.lastWriteTS == 0 {
		t.Fatal("fresh scope: lastWriteTS=0 (must be initialised to a real timestamp, not left zero)")
	}
}

func TestLastWriteTS_AdvancesOnAppend(t *testing.T) {
	buf := newscopeBuffer(10)
	pre := nowUnixMicro()
	it, err := buf.appendItem(newItem("s", "", nil))
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if buf.lastWriteTS < pre {
		t.Errorf("lastWriteTS=%d pre=%d (must be >= pre-call stamp)", buf.lastWriteTS, pre)
	}
	if buf.lastWriteTS != it.Ts {
		t.Errorf("lastWriteTS=%d item.Ts=%d (insertNewItemLocked must use one nowUs for both)",
			buf.lastWriteTS, it.Ts)
	}
}

func TestLastWriteTS_AdvancesOnUpsertReplace(t *testing.T) {
	buf := newscopeBuffer(10)
	if _, _, err := buf.upsertByID(newItem("s", "a", nil)); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	beforeReplace := buf.lastWriteTS

	pre := nowUnixMicro()
	if _, _, err := buf.upsertByID(newItem("s", "a", map[string]interface{}{"v": 2})); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}
	if buf.lastWriteTS < pre {
		t.Errorf("lastWriteTS=%d pre=%d (replace path must stamp lastWriteTS)", buf.lastWriteTS, pre)
	}
	if buf.lastWriteTS < beforeReplace {
		t.Errorf("lastWriteTS=%d went backwards from %d (replace must not regress the timestamp)",
			buf.lastWriteTS, beforeReplace)
	}
}

func TestLastWriteTS_AdvancesOnUpdate(t *testing.T) {
	buf := newscopeBuffer(10)
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	pre := nowUnixMicro()
	if _, err := buf.updateByID("a", json.RawMessage(`{"v":2}`)); err != nil {
		t.Fatalf("update: %v", err)
	}
	if buf.lastWriteTS < pre {
		t.Errorf("lastWriteTS=%d pre=%d (replaceItemAtIndexLocked must stamp lastWriteTS)",
			buf.lastWriteTS, pre)
	}
}

func TestLastWriteTS_AdvancesOnCounterAdd(t *testing.T) {
	buf := newscopeBuffer(10)

	preCreate := nowUnixMicro()
	if _, _, err := buf.counterAdd("s", "c", 1); err != nil {
		t.Fatalf("counter create: %v", err)
	}
	if buf.lastWriteTS < preCreate {
		t.Errorf("after create: lastWriteTS=%d pre=%d (create branch must stamp)",
			buf.lastWriteTS, preCreate)
	}
	afterCreate := buf.lastWriteTS

	preInc := nowUnixMicro()
	if _, _, err := buf.counterAdd("s", "c", 1); err != nil {
		t.Fatalf("counter increment: %v", err)
	}
	if buf.lastWriteTS < preInc {
		t.Errorf("after increment: lastWriteTS=%d pre=%d (replace branch via replaceItemAtIndexLocked must stamp)",
			buf.lastWriteTS, preInc)
	}
	if buf.lastWriteTS < afterCreate {
		t.Errorf("increment regressed lastWriteTS from %d to %d", afterCreate, buf.lastWriteTS)
	}
}

func TestLastWriteTS_AdvancesOnDelete(t *testing.T) {
	buf := newscopeBuffer(10)
	for i := 0; i < 5; i++ {
		if _, err := buf.appendItem(newItem("s", "", map[string]interface{}{"i": i})); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	preDelByID := nowUnixMicro()
	if _, err := buf.deleteBySeq(2); err != nil {
		t.Fatalf("deleteBySeq: %v", err)
	}
	if buf.lastWriteTS < preDelByID {
		t.Errorf("after deleteBySeq: lastWriteTS=%d pre=%d", buf.lastWriteTS, preDelByID)
	}

	preDelUpTo := nowUnixMicro()
	if _, err := buf.deleteUpToSeq(4); err != nil {
		t.Fatalf("deleteUpToSeq: %v", err)
	}
	if buf.lastWriteTS < preDelUpTo {
		t.Errorf("after deleteUpToSeq: lastWriteTS=%d pre=%d", buf.lastWriteTS, preDelUpTo)
	}
}

func TestLastWriteTS_AdvancesOnReplaceAll(t *testing.T) {
	buf := newscopeBuffer(10)
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	pre := nowUnixMicro()
	if _, err := buf.replaceAll([]Item{newItem("s", "x", nil), newItem("s", "y", nil)}); err != nil {
		t.Fatalf("replaceAll: %v", err)
	}
	if buf.lastWriteTS < pre {
		t.Errorf("after replaceAll (commitReplacement): lastWriteTS=%d pre=%d", buf.lastWriteTS, pre)
	}
}

// recordRead is a read-path bookkeeping update; it must not advance
// lastWriteTS. lastAccessTS is the matching read-side counter.
func TestLastWriteTS_NotAffectedByReads(t *testing.T) {
	buf := newscopeBuffer(10)
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}
	beforeRead := buf.lastWriteTS

	buf.recordRead(nowUnixMicro())

	if buf.lastWriteTS != beforeRead {
		t.Errorf("recordRead changed lastWriteTS: before=%d after=%d (reads must not bump write timestamp)",
			beforeRead, buf.lastWriteTS)
	}
}

// stats() must surface lastWriteTS unchanged. Readers of the snapshot
// rely on this as the authoritative "when was this scope last written"
// signal — drift here would make /stats lie even though the underlying
// field is correct.
func TestStats_SurfacesLastWriteTS(t *testing.T) {
	buf := newscopeBuffer(10)
	pre := nowUnixMicro()
	if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	st := buf.stats(nowUnixMicro())
	if st.LastWriteTS != buf.lastWriteTS {
		t.Errorf("stats.LastWriteTS=%d buf.lastWriteTS=%d (snapshot must mirror the field)",
			st.LastWriteTS, buf.lastWriteTS)
	}
	if st.LastWriteTS < pre {
		t.Errorf("stats.LastWriteTS=%d pre=%d", st.LastWriteTS, pre)
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
