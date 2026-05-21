# rspamd top-level options. Anything that lands here merges with
# /usr/share/rspamd/config/options.inc — same per-key override
# semantics as the rest of local.d/.

# Disable Hyperscan — Intel's JIT regex engine. Saves ~40-60 MB of
# RSS (one compiled DB per worker, persisting in private pages)
# at the cost of regex matching falling back to PCRE. The
# trade-off is "scan latency goes from ~200 ms to ~400 ms per
# message"; a personal mailbox sees a few messages per minute at
# peak, so the extra millisecond budget is invisible.
#
# Re-enable when traffic warrants — the toggle is one line and
# rspamd recompiles the DB on boot.
disable_hyperscan = true;
