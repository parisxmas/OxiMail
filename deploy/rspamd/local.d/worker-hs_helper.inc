# hs_helper — the Hyperscan helper worker that pre-compiles the
# regex database into a shared cache before the normal worker
# loads it. With disable_hyperscan=true in local.d/options.inc
# the regex engine falls back to PCRE and the pre-compile step
# is dead weight (~80 MB of idle Lua VM).
count = 0;
