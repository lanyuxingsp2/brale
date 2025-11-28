package freqtrade

import "sync"

// positionLocker 管理每个 tradeID 的互斥锁，避免监控/更新/回调同时写同一仓位。
var positionLocker = &sync.Map{}

func getPositionLock(tradeID int) *sync.Mutex {
	lock, _ := positionLocker.LoadOrStore(tradeID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}
