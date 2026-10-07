package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

// Variables of the processor simulator (SRS — Card Spend §2.1.1 Processor simulator). The group never opens the
// database and never reads CASCTL_DATABASE_URL.
const (
	simURLVar      = "CASCTL_CARD_AUTH_URL"
	simUserVar     = "CASCTL_PROCESSOR_USERNAME"
	simPasswordVar = "CASCTL_PROCESSOR_PASSWORD"
)

// simTimeout bounds one request: an authorization is answered within decision_deadline, a waiter as well.
const simTimeout = 30 * time.Second

// errServerFailure is the error of a 5xx answer: the answer is printed, the exit code is not zero.
var errServerFailure = errors.New("sim: card-auth answered 5xx")

// simClient is the processor side of the API: the base URL and the Basic pair from the environment.
type simClient struct {
	base     *url.URL
	username string
	password string // never printed
	http     *http.Client
}

// simClient reads the environment. Errors name the variable, never its value.
func (a *app) simClient() (*simClient, error) {
	raw := a.getenv(simURLVar)
	if raw == "" {
		return nil, errors.New(simURLVar + " is not set: the base URL of card-auth, such as http://127.0.0.1:8092")
	}
	base, err := url.Parse(raw)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New(simURLVar + " must be an http or https URL with a host")
	}
	c := &simClient{base: base, username: a.getenv(simUserVar), password: a.getenv(simPasswordVar),
		http: &http.Client{Timeout: simTimeout}}
	if c.username == "" {
		return nil, errors.New(simUserVar + " is not set: the username of the processor pair")
	}
	if c.password == "" {
		return nil, errors.New(simPasswordVar + " is not set: the password of the processor pair")
	}
	return c, nil
}

// answer is one output line: the HTTP status and the body as card-auth sent it.
type answer struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// do sends one request to escapedPath, already escaped, and returns its answer. A transport error carries no URL
// and no credential.
func (c *simClient) do(ctx context.Context, method, escapedPath string, body []byte) (answer, error) {
	u := *c.base
	u.RawPath = strings.TrimSuffix(c.base.EscapedPath(), "/") + escapedPath
	u.Path, _ = url.PathUnescape(u.RawPath)
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return answer{}, errors.New("sim: cannot build the request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.SetBasicAuth(c.username, c.password)
	resp, err := c.http.Do(req)
	if err != nil {
		// The error of the client quotes the URL; only the inner cause is kept.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return answer{}, fmt.Errorf("sim: no answer from %s: %s", simURLVar, strings.ReplaceAll(err.Error(), c.password, "[redacted]"))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return answer{}, fmt.Errorf("sim: the answer of %s was cut", simURLVar)
	}
	out := answer{Status: resp.StatusCode, Body: raw}
	if !json.Valid(raw) {
		out.Body, _ = json.Marshal(string(raw))
	}
	if len(raw) == 0 {
		out.Body = json.RawMessage("null")
	}
	return out, nil
}

// printAnswer writes one JSON line under mu.
func printAnswer(cmd *cobra.Command, mu *sync.Mutex, a answer) {
	line, _ := json.Marshal(a)
	mu.Lock()
	defer mu.Unlock()
	fmt.Fprintln(cmd.OutOrStdout(), string(line))
}

func (a *app) simCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sim",
		Short: "Play the processor against the API of card-auth, test networks only; no database is opened",
		Long: "Play the processor against the API of card-auth (SRS — Card Spend §2.1.1), test networks only.\n" +
			"Environment: " + simURLVar + ", " + simUserVar + ", " + simPasswordVar + ". The password is never printed.\n" +
			"Output: one JSON line per answer, {\"status\": <HTTP status>, \"body\": <body>}. The exit code is 0 for any\n" +
			"answer of the API, 4xx included, and not 0 for a transport error or a 5xx.",
	}
	cmd.AddCommand(a.simAuthorizeCmd(), a.simReturnCmd(), a.simGetCmd())
	return cmd
}

func (a *app) simAuthorizeCmd() *cobra.Command {
	var authID, cardRef, amount, currency, merchantName, merchantMCC, merchantCountry string
	var repeat, parallel int
	cmd := &cobra.Command{
		Use:   "authorize",
		Short: "POST /v1/authorizations; --repeat N sends the same request N times, --parallel P at a time",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if repeat < 1 || parallel < 1 {
				return errors.New("sim authorize: --repeat and --parallel must be at least 1")
			}
			c, err := a.simClient()
			if err != nil {
				return err
			}
			// Only the flags given are sent: validation stays in card-auth.
			req := map[string]any{}
			flags := cmd.Flags()
			for name, v := range map[string]string{"auth-id": authID, "card-ref": cardRef, "amount": amount, "currency": currency} {
				if flags.Changed(name) {
					req[strings.ReplaceAll(name, "-", "_")] = v
				}
			}
			merchant := map[string]string{}
			for name, v := range map[string]string{"merchant-name": merchantName, "merchant-mcc": merchantMCC, "merchant-country": merchantCountry} {
				if flags.Changed(name) {
					merchant[strings.TrimPrefix(name, "merchant-")] = v
				}
			}
			if len(merchant) > 0 {
				req["merchant"] = merchant
			}
			body, err := json.Marshal(req)
			if err != nil {
				return err
			}

			var mu sync.Mutex
			var failures []error
			jobs := make(chan struct{})
			var wg sync.WaitGroup
			for range min(parallel, repeat) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range jobs {
						ans, err := c.do(cmd.Context(), http.MethodPost, "/v1/authorizations", body)
						if err == nil {
							printAnswer(cmd, &mu, ans)
							if ans.Status >= 500 {
								err = errServerFailure
							}
						}
						if err != nil {
							mu.Lock()
							failures = append(failures, err)
							mu.Unlock()
						}
					}
				}()
			}
			for range repeat {
				jobs <- struct{}{}
			}
			close(jobs)
			wg.Wait()
			if len(failures) > 0 {
				if len(failures) > 1 {
					return fmt.Errorf("%w (%d of %d requests failed)", failures[0], len(failures), repeat)
				}
				return failures[0]
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&authID, "auth-id", "", "auth_id of the request")
	f.StringVar(&cardRef, "card-ref", "", "card_ref of the request")
	f.StringVar(&amount, "amount", "", "amount, a decimal string such as 25.40")
	f.StringVar(&currency, "currency", "", "currency, three letters such as USD")
	f.StringVar(&merchantName, "merchant-name", "", "merchant.name")
	f.StringVar(&merchantMCC, "merchant-mcc", "", "merchant.mcc")
	f.StringVar(&merchantCountry, "merchant-country", "", "merchant.country")
	f.IntVar(&repeat, "repeat", 1, "send the same request N times")
	f.IntVar(&parallel, "parallel", 1, "at most P requests at a time")
	return cmd
}

func (a *app) simReturnCmd() *cobra.Command {
	var authID, returnID, typ, amount string
	cmd := &cobra.Command{
		Use:   "return",
		Short: "POST /v1/authorizations/{auth_id}/returns",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.simClient()
			if err != nil {
				return err
			}
			req := map[string]any{}
			flags := cmd.Flags()
			for name, v := range map[string]string{"return-id": returnID, "type": typ, "amount": amount} {
				if flags.Changed(name) {
					req[strings.ReplaceAll(name, "-", "_")] = v
				}
			}
			body, err := json.Marshal(req)
			if err != nil {
				return err
			}
			return a.simOne(cmd, c, http.MethodPost, "/v1/authorizations/"+url.PathEscape(authID)+"/returns", body)
		},
	}
	f := cmd.Flags()
	f.StringVar(&authID, "auth-id", "", "auth_id of the authorization")
	f.StringVar(&returnID, "return-id", "", "return_id of the request")
	f.StringVar(&typ, "type", "", "REVERSAL or REFUND")
	f.StringVar(&amount, "amount", "", "amount, a decimal string; omitted: everything not yet returned")
	_ = cmd.MarkFlagRequired("auth-id")
	return cmd
}

func (a *app) simGetCmd() *cobra.Command {
	var authID string
	cmd := &cobra.Command{
		Use:   "get",
		Short: "GET /v1/authorizations/{auth_id}",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.simClient()
			if err != nil {
				return err
			}
			return a.simOne(cmd, c, http.MethodGet, "/v1/authorizations/"+url.PathEscape(authID), nil)
		},
	}
	cmd.Flags().StringVar(&authID, "auth-id", "", "auth_id of the authorization")
	_ = cmd.MarkFlagRequired("auth-id")
	return cmd
}

// simOne sends one request and prints its answer.
func (a *app) simOne(cmd *cobra.Command, c *simClient, method, path string, body []byte) error {
	ans, err := c.do(cmd.Context(), method, path, body)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	printAnswer(cmd, &mu, ans)
	if ans.Status >= 500 {
		return errServerFailure
	}
	return nil
}
