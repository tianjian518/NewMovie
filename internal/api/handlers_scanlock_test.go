package api

import (
	"sync"
	"testing"
)

// TestScanLock_OnlyOneAtATime 同一媒体库的扫描必须互斥。
// 没有这把锁时，前端连点两下「扫描」会起两个协程同时遍历同一个库：
// 两边各自 upsert 同一批条目、互相覆盖计数，还会把网盘请求量翻倍触发风控。
func TestScanLock_OnlyOneAtATime(t *testing.T) {
	s := &Server{}
	if !s.tryLockScan("lib1") {
		t.Fatal("首次加锁应成功")
	}
	if s.tryLockScan("lib1") {
		t.Fatal("同一个库重复加锁应失败")
	}
	if !s.tryLockScan("lib2") {
		t.Fatal("不同库互不影响，应能各自加锁")
	}
	if !s.ScanRunning("lib1") {
		t.Fatal("ScanRunning 应报告 lib1 在跑")
	}
	s.unlockScan("lib1")
	if s.ScanRunning("lib1") {
		t.Fatal("解锁后不应再报告在跑")
	}
	if !s.tryLockScan("lib1") {
		t.Fatal("解锁后应能重新加锁")
	}
}

// TestScanLock_ConcurrentOnlyOneWins 并发抢锁时有且只有一个赢家。
func TestScanLock_ConcurrentOnlyOneWins(t *testing.T) {
	s := &Server{}
	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			if s.tryLockScan("lib") {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("抢到锁的协程数 = %d, want 1", won)
	}
}
