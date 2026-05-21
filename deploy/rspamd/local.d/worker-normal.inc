# Personal mailbox sees on the order of dozens of messages per
# hour at peak — well within the capacity of a single normal
# worker. Rspamd's default spawns one worker per CPU; with 3
# CPUs on this host that meant 3 × ~143 MB resident workers (each
# carrying its own copy of the compiled hyperscan DB + per-worker
# Lua VM state). Pinning to one drops the per-worker total
# from ~430 MB to ~143 MB visible RSS.
count = 1;
