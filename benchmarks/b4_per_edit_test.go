package benchmarks

import (
	"testing"

	"github.com/Deln0r/ygo"
)

// BenchmarkB4_TransactionPerEdit replays the B4 trace the way an editor
// produces it: one transaction per edit, with an update handler that takes
// each transaction's update.
func BenchmarkB4_TransactionPerEdit(b *testing.B) {
	tf, _ := loadB4Trace(b)
	for i := 0; i < b.N; i++ {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
		t := ygo.NewText(d, "text")
		bytes := 0
		d.OnUpdate(func(update []byte, _ any) { bytes += len(update) })
		for _, e := range tf.Edits {
			pos, delLen, ins := unpackEdit(e)
			txn := d.WriteTxn()
			if delLen > 0 {
				_ = t.Delete(txn, pos, delLen)
			}
			if len(ins) > 0 {
				_ = t.Insert(txn, pos, ins)
			}
			txn.Commit()
		}
		if i == 0 {
			if got := t.String(); got != tf.FinalText {
				b.Fatalf("final text mismatch: got %d chars, want %d chars", len(got), len(tf.FinalText))
			}
			b.ReportMetric(float64(bytes), "updateBytes")
		}
	}
}
