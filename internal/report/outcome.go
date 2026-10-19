package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pikopod/pikopod/internal/alert"
)

type Outcome struct {
	Recorded   int `json:"recorded"`
	Reproduced int `json:"reproduced"`
	Committed  int `json:"committed"`
	Recurred   int `json:"recurred"`
}

func packExists(dir, name string) (os.FileInfo, bool) {
	for _, ext := range []string{".yaml", ".yml"} {
		if info, err := os.Stat(filepath.Join(dir, name+ext)); err == nil && !info.IsDir() {
			return info, true
		}
	}
	return nil, false
}

func ComputeOutcome(events []alert.DriftEvent, dataDir, workTree string) Outcome {
	var o Outcome
	for _, ev := range events {
		if !(ev.Kind.IsIncident() || ev.Kind.IsDivergence()) {
			continue
		}
		o.Recorded++
		name := "incident-" + strings.TrimPrefix(ev.Fingerprint, "fp_")
		committed, isCommitted := packExists(filepath.Join(workTree, "scenarios"), name)
		_, reproduced := packExists(filepath.Join(dataDir, "scenarios"), name)
		if reproduced || isCommitted {
			o.Reproduced++
		}
		if !isCommitted {
			continue
		}
		o.Committed++
		if ev.LastSeen.After(committed.ModTime()) {
			o.Recurred++
		}
	}
	return o
}

func (o Outcome) Line() string {
	return fmt.Sprintf("%d provider failures recorded · %d reproduced locally · %d committed as scenarios · %d recurred after being committed",
		o.Recorded, o.Reproduced, o.Committed, o.Recurred)
}
