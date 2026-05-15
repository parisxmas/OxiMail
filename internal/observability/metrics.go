package observability

import "github.com/prometheus/client_golang/prometheus"

// The Prometheus metrics OxiMail exports. They are package-level
// variables so any component can update them by name, and so tests can
// inspect their state. All are registered with the default registry
// from init() — promhttp.Handler() exposes them at /metrics.
var (
	// SMTPMessages counts inbound SMTP messages by the spam pipeline's
	// final verdict.
	SMTPMessages = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oximail_smtp_messages_total",
			Help: "Inbound SMTP messages by verdict (accept | greylist | reject).",
		},
		[]string{"verdict"},
	)

	// QueueDeliveries counts outbound queue delivery outcomes, one per
	// recipient. "delivered" — accepted by the remote MX. "deferred" —
	// temp failure, will be retried. "bounced" — permanently abandoned
	// (the sender receives a DSN).
	QueueDeliveries = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oximail_queue_deliveries_total",
			Help: "Outbound queue delivery outcomes per recipient.",
		},
		[]string{"result"},
	)

	// QueueDue is the number of outbound queue messages that were due at
	// the last scan — i.e. ready for a delivery attempt.
	QueueDue = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "oximail_queue_due_messages",
			Help: "Outbound queue messages currently due for a delivery attempt.",
		},
	)

	// Logins counts authentication attempts by protocol and outcome.
	Logins = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oximail_logins_total",
			Help: "Authentication attempts by protocol (imap | submission | webmail) and result (ok | fail).",
		},
		[]string{"protocol", "result"},
	)
)

func init() {
	prometheus.MustRegister(SMTPMessages, QueueDeliveries, QueueDue, Logins)
	// Pre-initialize the known label combinations so the counters show
	// up in /metrics at 0 from the start. Prometheus client_golang
	// omits a CounterVec series until it has been touched, which makes
	// rate() return NaN until the first event — dashboards prefer 0.
	for _, v := range []string{"accept", "greylist", "reject"} {
		SMTPMessages.WithLabelValues(v).Add(0)
	}
	for _, r := range []string{"delivered", "deferred", "bounced"} {
		QueueDeliveries.WithLabelValues(r).Add(0)
	}
	for _, p := range []string{"imap", "submission", "webmail"} {
		for _, r := range []string{"ok", "fail"} {
			Logins.WithLabelValues(p, r).Add(0)
		}
	}
}
