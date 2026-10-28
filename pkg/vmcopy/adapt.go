package vmcopy

// AdaptConfig is the rule that sets the speed of a running copy from the
// latency of the share it reads. It grows slowly and shrinks fast: a backup that
// is a little too slow costs minutes, a VM that is too slow costs its users.
type AdaptConfig struct {
	// Start is the speed a copy begins at and the lowest it goes to, in bytes
	// per second for each disk.
	Start int64
	// Step is how much the speed grows each time the share is quiet.
	Step int64
	// Max is the highest speed. 0 means there is none.
	Max int64
	// LimitMs is the average share latency above which the copy slows down.
	LimitMs float64
}

// Next is the speed a copy at cur should run at, given the latest reading of
// the average latency of its share. ok is false when there is no reading; a
// missing reading never raises the speed.
func (c AdaptConfig) Next(cur int64, latencyMs float64, ok bool) int64 {
	cur = max(cur, c.Start)
	if !ok {
		return cur
	}
	if latencyMs > c.LimitMs {
		return max(c.Start, cur/2)
	}
	next := cur + c.Step
	if c.Max > 0 {
		next = min(next, c.Max)
	}
	return next
}
