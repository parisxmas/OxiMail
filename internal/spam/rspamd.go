package spam

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// rspamdScanTimeout bounds one Rspamd scan. Rspamd does its own network
// work (RBLs, fuzzy hashing), so the budget is generous.
const rspamdScanTimeout = 10 * time.Second

// rspamdChecker is the content stage: it POSTs the message to a Rspamd
// daemon's /checkv2 endpoint and maps Rspamd's action to a verdict.
//
// It fails open — any error reaching or parsing Rspamd yields Accept, so
// a scanner outage degrades filtering rather than blocking all mail.
type rspamdChecker struct {
	endpoint string
	client   *http.Client
}

func newRspamdChecker(baseURL string) *rspamdChecker {
	return &rspamdChecker{
		endpoint: strings.TrimRight(baseURL, "/") + "/checkv2",
		client:   &http.Client{Timeout: rspamdScanTimeout},
	}
}

// rspamdResult is the part of Rspamd's /checkv2 response OxiMail uses.
type rspamdResult struct {
	Action string  `json:"action"`
	Score  float64 `json:"score"`
}

// check scans one message with Rspamd and returns its verdict.
func (c *rspamdChecker) check(remoteIP, mailFrom string, rcptTo []string, raw []byte) Verdict {
	req, err := http.NewRequest(http.MethodPost, c.endpoint, bytes.NewReader(raw))
	if err != nil {
		log.Printf("spam: rspamd request: %v — accepting", err)
		return Accept
	}
	// Rspamd takes the SMTP envelope as request headers.
	if remoteIP != "" {
		req.Header.Set("IP", remoteIP)
	}
	if mailFrom != "" {
		req.Header.Set("From", mailFrom)
	}
	for _, rcpt := range rcptTo {
		req.Header.Add("Rcpt", rcpt)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		log.Printf("spam: rspamd unreachable (%v) — accepting", err)
		return Accept
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("spam: rspamd returned %s — accepting", resp.Status)
		return Accept
	}

	var result rspamdResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("spam: rspamd response: %v — accepting", err)
		return Accept
	}
	return verdictFromAction(result.Action)
}

// verdictFromAction maps a Rspamd action to an OxiMail verdict.
//
// TODO: honor "add header" / "rewrite subject" by tagging the delivered
// message (an X-Spam header, or a junk flag) rather than treating them
// as a plain Accept.
func verdictFromAction(action string) Verdict {
	switch action {
	case "reject":
		return Reject
	case "soft reject", "greylist":
		return Greylist
	default:
		// "no action", "add header", "rewrite subject", and anything
		// unrecognised — deliver it.
		return Accept
	}
}
