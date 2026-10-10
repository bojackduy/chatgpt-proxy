package chatgpt

import (
	"context"
	_ "embed"
	"encoding/json"
	"github.com/chromedp/chromedp"
	"time"
)

//go:embed response.js
var responseProbeJS string

type responseState struct {
	IDs         []string `json:"ids"`
	LastID      string   `json:"lastID"`
	StopBtn     bool     `json:"stopBtn"`
	ContinueBtn bool     `json:"continueBtn"`
	ErrorText   bool     `json:"errorText"`
	Last        string   `json:"lastAssistant"`
	Reasoning   string   `json:"reasoning"`
	Fresh       bool     `json:"fresh"`
	Complete    bool     `json:"complete"`
}

type emptyAnswerWatch struct {
	Started  time.Time
	Since    time.Time
	SeenStop bool
}

func (w *emptyAnswerWatch) expired(st responseState, now time.Time) bool {
	if st.StopBtn {
		w.SeenStop = true
		w.Since = time.Time{}
		return false
	}
	if st.Last != "" || st.ContinueBtn {
		w.Since = time.Time{}
		return false
	}
	if w.SeenStop || st.Complete || now.Sub(w.Started) >= 30*time.Second {
		if w.Since.IsZero() {
			w.Since = now
		}
		return now.Sub(w.Since) >= 15*time.Second
	}
	return false
}

func responseProbe(tab context.Context, baseline []string) (responseState, error) {
	if baseline == nil {
		baseline = []string{}
	}
	b, _ := json.Marshal(baseline)
	var st responseState
	err := chromedp.Run(tab, chromedp.Evaluate(responseProbeJS+"("+string(b)+")", &st))
	return st, err
}
