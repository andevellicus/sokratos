package toolreg

import (
	"sync"
	"testing"
	"time"
)

func TestApprovalCacheBasic(t *testing.T) {
	c := NewApprovalCache(100 * time.Millisecond)

	if c.Check("key1") {
		t.Fatal("unseen key should return false")
	}

	c.Record("key1")
	if !c.Check("key1") {
		t.Fatal("just-recorded key should return true")
	}
	if c.Check("key2") {
		t.Fatal("different key should return false")
	}
}

func TestApprovalCacheTTLExpiry(t *testing.T) {
	c := NewApprovalCache(50 * time.Millisecond)

	c.Record("key1")
	if !c.Check("key1") {
		t.Fatal("key should be valid before TTL")
	}

	time.Sleep(60 * time.Millisecond)
	if c.Check("key1") {
		t.Fatal("key should expire after TTL")
	}
}

func TestApprovalCacheConcurrency(t *testing.T) {
	c := NewApprovalCache(time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.Record("key")
		}()
		go func() {
			defer wg.Done()
			c.Check("key")
		}()
	}
	wg.Wait()
}
