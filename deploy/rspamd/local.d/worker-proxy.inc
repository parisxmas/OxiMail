# rspamd_proxy — the milter/proxy worker that lets rspamd sit
# in front of (or between) an MTA via the milter or HTTP-proxy
# protocols. We never use it: OxiMail calls rspamd directly via
# the /checkv2 HTTP endpoint on the normal worker. The proxy
# worker idles ~140 MB carrying its own copy of every map and
# Lua plugin.
#
# Setting count = 0 tells the master process not to fork any
# proxy workers.
count = 0;
