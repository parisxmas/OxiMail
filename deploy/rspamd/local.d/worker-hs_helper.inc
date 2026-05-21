# hs_helper — the Hyperscan helper worker that pre-compiles the
# regex database into a shared cache before the normal worker
# loads it. With disable_hyperscan=true in local.d/options.inc
# the regex engine falls back to PCRE and the pre-compile step
# is dead weight (~83 MB of idle Lua VM).
#
# First attempt used `count = 0` — rspamd silently interpreted
# that as "default count" and forked one anyway. The explicit
# `enabled = false;` overrides the parent worker block's
# enabled flag and the master skips spawning entirely.
enabled = false;
