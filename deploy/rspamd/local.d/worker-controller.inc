# Controller worker serves rspamc, the web UI, /metrics, /learn,
# /scan, and /history. We use NONE of those — OxiMail POSTs to
# the normal worker's /checkv2 endpoint on 11333 for every scan;
# we don't have a Prometheus scrape pointed at /metrics; bayes
# is disabled so /learn is moot; and the web UI is unreachable
# from outside the docker network anyway.
#
# Disabling the controller saves ~137 MB of resident memory
# (each worker carries its own copy of the maps + plugin VM).
# The compose healthcheck moves to a TCP probe on 11333.
enabled = false;
