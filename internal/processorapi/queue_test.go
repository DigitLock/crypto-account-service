package processorapi_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	opqueue "github.com/DigitLock/crypto-account-service/internal/operator"
	"github.com/DigitLock/crypto-account-service/internal/repository"
	"github.com/DigitLock/crypto-account-service/internal/testchain"
)

// The operator queue at shutdown (S2 st8, the leftover of st7): a send that fails while its context is alive is
// logged; a send that fails because its context was cancelled is not, as in the tracker and the listener. Both are
// treated as sent: the proxy relays the transaction to Anvil and only holds the answer.
func TestQueueQuietAtShutdown(t *testing.T) {
	c := testchain.Start(t)
	sending := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		resp, err := http.Post(c.RPCURL, "application/json", bytes.NewReader(body))
		if err != nil {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if strings.Contains(string(body), `"eth_sendRawTransaction"`) {
			sending <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	}))
	t.Cleanup(proxy.Close)
	e := newEnv(t, options{chain: c, rpcURL: proxy.URL, realDebit: true, noCRS: true})

	send := func(sendCtx context.Context) {
		t.Helper()
		var slot opqueue.Slot
		if err := pgx.BeginFunc(ctx, e.cardAuth, func(tx pgx.Tx) error {
			var err error
			slot, err = e.queue.Reserve(ctx, repository.New(tx), opqueue.PurposeRelease, nil, nil)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.queue.Send(sendCtx, slot, opqueue.Call{Purpose: opqueue.PurposeRelease, To: e.chain.Operator, Gas: 21000}, nil); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	// Alive: the answer is held past rpc_read_timeout.
	before := len(e.logs.String())
	send(ctx)
	<-sending
	if tail := e.logs.String()[before:]; !strings.Contains(tail, "operator transaction send failed; treated as sent") {
		t.Errorf("a failed send with the context alive is not logged:\n%s", tail)
	}

	// Cancelled while the send waits for its answer.
	cancelled, cancel := context.WithCancel(ctx)
	go func() {
		<-sending
		cancel()
	}()
	before = len(e.logs.String())
	send(cancelled)
	if tail := e.logs.String()[before:]; strings.Contains(tail, `"level":"WARN"`) || strings.Contains(tail, `"level":"ERROR"`) {
		t.Errorf("the queue logged a failure at shutdown:\n%s", tail)
	}
	e.noNonceGap(t)
}
