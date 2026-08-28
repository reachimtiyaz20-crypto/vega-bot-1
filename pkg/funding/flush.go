package funding

// writeAndFlush writes a record and pushes it to disk immediately.
//
// Health records are written after the observation batch has already been
// flushed, so without this they sit in the 64 KB buffer until the NEXT poll.
// Nothing is lost, but every monitoring surface -- the vega command, the
// dashboard -- would show the previous poll's numbers and look frozen.
//
// Observations are batched because they arrive 371 at a time. Health records
// arrive one at a time and are what a human reads, so they are flushed
// immediately. The cost is one fsync per poll.
func (m *Monitor) writeAndFlush(rec Record) error {
	if err := m.Journal.Write(rec); err != nil {
		return err
	}
	return m.Journal.Flush()
}
