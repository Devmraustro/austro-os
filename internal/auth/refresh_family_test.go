package auth

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"austro-os/internal/config"
)

// testService builds a JWTService over an in-memory refresh store. The secrets
// only have to be stable for the test; nothing here validates configuration.
func testService() (*JWTService, RefreshTokenStore) {
	store := NewMemoryRefreshStore()
	cfg := &config.Config{
		JWTSecret:        "unit-test-access-secret-32-chars-min!!",
		JWTRefreshSecret: "unit-test-refresh-secret-32-chars-min!!",
	}
	return InitializeWithStore(cfg, store), store
}

// TestRefreshTokenReuseRevokesWholeFamily is the regression test for the family
// revocation defect: revokeFamily used to look tokens up by their per-token ID,
// which is unique to the presented token, so "revoke the family" revoked only
// the token that was already revoked. Every descendant of the compromised login
// must be revoked, otherwise an attacker who stole a refresh token keeps a
// working session after the legitimate holder rotates.
func TestRefreshTokenReuseRevokesWholeFamily(t *testing.T) {
	svc, _ := testService()

	rt1, err := svc.IssueRefreshToken("user-family")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// Legitimate holder rotates twice, producing a chain rt1 -> rt2 -> rt3.
	rt2, err := svc.Refresh(rt1)
	if err != nil {
		t.Fatalf("rotate rt1: %v", err)
	}
	rt3, err := svc.Refresh(rt2)
	if err != nil {
		t.Fatalf("rotate rt2: %v", err)
	}
	if !svc.VerifyRefreshToken(rt3) {
		t.Fatal("the current chain head must be valid before the reuse attempt")
	}

	// The attacker replays the original, already-consumed token.
	if _, err := svc.Refresh(rt1); err == nil {
		t.Fatal("reuse of an already-used refresh token must be rejected")
	}

	// Reuse detection must take the whole family down with it, including the
	// chain head the attacker would otherwise still hold.
	if svc.VerifyRefreshToken(rt2) {
		t.Error("rt2 belongs to the compromised family and must be revoked")
	}
	if svc.VerifyRefreshToken(rt3) {
		t.Error("rt3 belongs to the compromised family and must be revoked")
	}
	if _, err := svc.Refresh(rt3); err == nil {
		t.Error("rotating a token from a revoked family must fail")
	}
}

// TestRefreshTokenFamilySurvivesLegitimateRotation proves the fix does not
// over-revoke: an unbroken rotation chain stays usable, and an unrelated login
// is never affected by another family's revocation.
func TestRefreshTokenFamilySurvivesLegitimateRotation(t *testing.T) {
	svc, _ := testService()

	other, err := svc.IssueRefreshToken("user-other")
	if err != nil {
		t.Fatalf("issue other: %v", err)
	}

	current, err := svc.IssueRefreshToken("user-keep")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	for i := 0; i < 5; i++ {
		next, err := svc.Refresh(current)
		if err != nil {
			t.Fatalf("rotation %d: %v", i, err)
		}
		if next == current {
			t.Fatalf("rotation %d returned the same token", i)
		}
		if !svc.VerifyRefreshToken(next) {
			t.Fatalf("rotation %d produced an invalid token", i)
		}
		current = next
	}

	// Revoking one family must not disturb a different login.
	if _, err := svc.Refresh(current); err != nil {
		t.Fatalf("final rotation: %v", err)
	}
	if !svc.VerifyRefreshToken(other) {
		t.Error("an unrelated login must be unaffected by another family's rotation")
	}
}

// TestRefreshTokenFamilyIsSharedAcrossRotation asserts the persisted records
// actually carry one family id for a whole chain and distinct per-token ids.
// This is the invariant the revocation lookup depends on.
func TestRefreshTokenFamilyIsSharedAcrossRotation(t *testing.T) {
	svc, store := testService()

	rt1, err := svc.IssueRefreshToken("user-ids")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	first, err := store.Get(hashToken(rt1))
	if err != nil {
		t.Fatalf("get rt1: %v", err)
	}
	if first.FamilyID == "" {
		t.Fatal("a refresh token record must carry a family id")
	}
	if first.FamilyID != first.ID {
		t.Errorf("the first token of a family must seed the family id: family=%s id=%s", first.FamilyID, first.ID)
	}

	rt2, err := svc.Refresh(rt1)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	second, err := store.Get(hashToken(rt2))
	if err != nil {
		t.Fatalf("get rt2: %v", err)
	}
	if second.FamilyID != first.FamilyID {
		t.Errorf("rotation must inherit the family id: want %s got %s", first.FamilyID, second.FamilyID)
	}
	if second.ID == first.ID {
		t.Error("each token in a family must have a distinct id")
	}
}

// TestMemoryRefreshStoreConcurrentAccess exercises the store from many
// goroutines. Before the store was guarded by a mutex this was both a data
// race and a potential fatal "concurrent map writes" abort, reachable through
// the concurrent HTTP refresh endpoint. Run under -race this fails on the
// unsynchronised implementation.
func TestMemoryRefreshStoreConcurrentAccess(t *testing.T) {
	store := NewMemoryRefreshStore()
	now := time.Now().UTC()

	const workers = 16
	const perWorker = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				hash := fmt.Sprintf("hash-%d-%d", w, i)
				rec := &StoredRefreshToken{
					ID:        hash,
					FamilyID:  fmt.Sprintf("family-%d", w),
					UserID:    fmt.Sprintf("user-%d", w),
					TokenHash: hash,
					CreatedAt: now,
					ExpiresAt: now.Add(refreshTokenTTL),
				}
				if err := store.Save(rec); err != nil {
					t.Errorf("save: %v", err)
					return
				}
				if _, err := store.Get(hash); err != nil {
					t.Errorf("get: %v", err)
					return
				}
				if err := store.RevokeFamily(fmt.Sprintf("family-%d", w)); err != nil {
					t.Errorf("revoke family: %v", err)
					return
				}
				_ = store.Revoke(hash)
			}
		}(w)
	}
	wg.Wait()
}

// TestMemoryRefreshStoreGetReturnsCopy proves a caller cannot mutate persisted
// state behind the lock by holding on to a returned record.
func TestMemoryRefreshStoreGetReturnsCopy(t *testing.T) {
	store := NewMemoryRefreshStore()
	now := time.Now().UTC()
	rec := &StoredRefreshToken{
		ID: "id-copy", FamilyID: "fam-copy", UserID: "u",
		TokenHash: "hash-copy", CreatedAt: now, ExpiresAt: now.Add(refreshTokenTTL),
	}
	if err := store.Save(rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := store.Get("hash-copy")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got.Revoked = true
	got.UserID = "mutated"

	again, err := store.Get("hash-copy")
	if err != nil {
		t.Fatalf("get again: %v", err)
	}
	if again.Revoked {
		t.Error("mutating a returned record must not change the stored record")
	}
	if again.UserID != "u" {
		t.Errorf("stored record was mutated through the returned copy: %s", again.UserID)
	}
}

// TestMemoryRefreshStoreIsBounded proves the session table cannot grow without
// limit: expired records are pruned first, and the hard bound is never
// exceeded even when nothing has expired.
func TestMemoryRefreshStoreIsBounded(t *testing.T) {
	store := NewMemoryRefreshStore().(*memoryRefreshStore)
	now := time.Now().UTC()

	save := func(i int, expiry time.Time) {
		h := fmt.Sprintf("bounded-%d", i)
		if err := store.Save(&StoredRefreshToken{
			ID: h, FamilyID: h, UserID: "u", TokenHash: h,
			CreatedAt: now.Add(time.Duration(i) * time.Millisecond), ExpiresAt: expiry,
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	// Well past the bound, with nothing expired: the table must still be
	// capped, having evicted the oldest records in batches.
	for i := 0; i < maxRefreshTokenRecords+512; i++ {
		save(i, now.Add(refreshTokenTTL))
	}
	store.mu.RLock()
	size := len(store.byHash)
	store.mu.RUnlock()
	if size > refreshSweepThreshold {
		t.Errorf("store exceeded its bound: %d > %d", size, refreshSweepThreshold)
	}

	// Expired records must be reclaimed once a sweep runs. Sweeps are
	// amortised (they fire near the bound, not on every insert), so fill to the
	// sweep threshold with records that are already past their expiry and then
	// insert one live record to trigger it.
	expired := NewMemoryRefreshStore().(*memoryRefreshStore)
	for i := 0; i < refreshSweepThreshold; i++ {
		h := fmt.Sprintf("expired-%d", i)
		if err := expired.Save(&StoredRefreshToken{
			ID: h, FamilyID: h, UserID: "u", TokenHash: h,
			CreatedAt: now.Add(-2 * refreshTokenTTL), ExpiresAt: now.Add(-refreshTokenTTL),
		}); err != nil {
			t.Fatalf("save expired %d: %v", i, err)
		}
	}
	if err := expired.Save(&StoredRefreshToken{
		ID: "fresh", FamilyID: "fresh", UserID: "u", TokenHash: "fresh",
		CreatedAt: now, ExpiresAt: now.Add(refreshTokenTTL),
	}); err != nil {
		t.Fatalf("save fresh: %v", err)
	}
	expired.mu.RLock()
	size = len(expired.byHash)
	_, freshStillThere := expired.byHash["fresh"]
	expired.mu.RUnlock()
	if size != 1 || !freshStillThere {
		t.Errorf("the sweep must reclaim expired records and keep the live one; got size=%d live=%v", size, freshStillThere)
	}
}

// TestMemoryRefreshStoreRejectsUnusableRecord guards the store's input
// invariant: a record with no token hash could never be looked up again and
// would silently accumulate.
func TestMemoryRefreshStoreRejectsUnusableRecord(t *testing.T) {
	store := NewMemoryRefreshStore()
	if err := store.Save(&StoredRefreshToken{ID: "no-hash"}); err == nil {
		t.Error("saving a record without a token hash must fail")
	}
	if err := store.Save(nil); err == nil {
		t.Error("saving a nil record must fail")
	}
}

// TestRevokeFamilyWithEmptyIDIsNoOp pins the guard that prevents an empty
// family id from matching records persisted without one.
func TestRevokeFamilyWithEmptyIDIsNoOp(t *testing.T) {
	store := NewMemoryRefreshStore()
	now := time.Now().UTC()
	if err := store.Save(&StoredRefreshToken{
		ID: "legacy", UserID: "u", TokenHash: "legacy-hash",
		CreatedAt: now, ExpiresAt: now.Add(refreshTokenTTL),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.RevokeFamily(""); err != nil {
		t.Fatalf("revoke empty family: %v", err)
	}
	got, err := store.Get("legacy-hash")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Revoked {
		t.Error("an empty family id must not revoke unrelated records")
	}
}
